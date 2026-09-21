// Package target reads and writes the target LDAP server. This file is the
// read-only part: the snapshot of groups_base, users_base, and Dolly's
// records under state_base, and targeted uid lookups for groups-only runs.
// It takes no lock and writes nothing. The writer (apply.go, lock.go,
// status.go) works through the Conn interface.
package target

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/go-ldap/ldap/v3"

	"github.com/dirkpetersen/dolly/internal/config"
	"github.com/dirkpetersen/dolly/internal/ldapconn"
	"github.com/dirkpetersen/dolly/internal/model"
)

// pageSize is the page size for paged searches of the target. Paging
// doesn't lift a server's hard size limit: a base with more entries than
// the limit fails with sizeLimitExceeded, and the run aborts.
const pageSize = 500

// lookupChunk is how many uids one existence lookup asks for.
const lookupChunk = 50

// Reader reads the target server.
type Reader struct {
	conn *ldap.Conn
	// URL is the server in use.
	URL string
	cfg *config.Config
}

// Dial connects and binds to target.url, with StartTLS if target.start_tls
// is set, verifying the server against ca_file or the system roots.
func Dial(ctx context.Context, cfg *config.Config) (*Reader, error) {
	pw, err := cfg.Target.Password()
	if err != nil {
		return nil, fmt.Errorf("target: %w", err)
	}
	conn, err := ldapconn.Dial(ctx, ldapconn.Options{
		URL: cfg.Target.URL, StartTLS: cfg.Target.StartTLS, CAFile: cfg.Target.CAFile,
		Timeout: cfg.Sync.NetworkTimeout.Duration, BindDN: cfg.Target.BindDN, Password: pw,
	})
	if err != nil {
		return nil, fmt.Errorf("target %s: %w", cfg.Target.URL, err)
	}
	return &Reader{conn: conn, URL: cfg.Target.URL, cfg: cfg}, nil
}

// Close closes the connection.
func (r *Reader) Close() error { return r.conn.Close() }

// Read returns the target snapshot and Dolly's ownership records. It reads
// groups_base and state_base, and users_base only if readUsers is set: a
// groups-only run doesn't need it, and users_base may be larger than the
// server lets any search return. A missing groups_base or users_base is an
// error (Dolly never creates them); a missing state_base means no records
// yet (a real run creates it).
func (r *Reader) Read(ctx context.Context, readUsers bool) (*model.TargetSnapshot, *model.Records, error) {
	return readSnapshot(r.cfg, r.URL, readUsers, func(base, name string, attrs []string, optional bool) ([]*model.Entry, error) {
		return r.subtree(ctx, base, name, attrs, optional)
	})
}

// subtreeFunc reads every entry under base with the given attributes. A
// missing base is an error unless optional is set.
type subtreeFunc func(base, name string, attrs []string, optional bool) ([]*model.Entry, error)

// readSnapshot does Read's three subtree reads through read, then combines
// and classifies them. Errors from read are returned as they are; url names
// the server in classification errors.
func readSnapshot(cfg *config.Config, url string, readUsers bool, read subtreeFunc) (*model.TargetSnapshot, *model.Records, error) {
	t := cfg.Target
	groupAttrs := append([]string{"objectClass", "cn", config.AttrMember, config.AttrMemberUID}, model.SortedKeys(cfg.Mapping.Groups.Attributes)...)
	groups, err := read(t.GroupsBase, "groups_base", groupAttrs, false)
	if err != nil {
		return nil, nil, err
	}
	var users []*model.Entry
	if readUsers {
		userAttrs := append([]string{"objectClass", "uid"}, model.SortedKeys(cfg.Mapping.Users.Attributes)...)
		users, err = read(t.UsersBase, "users_base", userAttrs, false)
		if err != nil {
			return nil, nil, err
		}
	}
	state, err := read(t.StateBase, "state_base", []string{"objectClass", "cn", "ou", "seeAlso", "roleOccupant", "description"}, true)
	if err != nil {
		return nil, nil, err
	}
	snap, recs, err := model.Classify(combine(t.StateBase, groups, users, state), model.Bases{Users: t.UsersBase, Groups: t.GroupsBase, State: t.StateBase})
	if err != nil {
		return nil, nil, fmt.Errorf("target %s: %w", url, err)
	}
	snap.UsersRead = readUsers
	return snap, recs, nil
}

// combine merges the three subtree reads into one list with each DN once.
//
// state_base may live inside groups_base or users_base (config validation
// allows that, but not the reverse). The groups_base and users_base reads
// then also return Dolly's containers and records, with the wrong attribute
// list (no seeAlso or roleOccupant), so every entry at or under state_base
// is dropped from them and comes only from the state_base read.
//
// users_base and groups_base may be nested in each other, so an entry can
// come back from both reads, each time with a different attribute list.
// Those copies are merged (union of attributes) into one entry.
func combine(stateBase string, groups, users, state []*model.Entry) []*model.Entry {
	var out []*model.Entry
	seen := map[string]*model.Entry{}
	for _, list := range [][]*model.Entry{groups, users} {
		for _, e := range list {
			if model.IsUnder(e.DN, stateBase) {
				continue
			}
			k := model.MustDNKey(e.DN)
			if prev, ok := seen[k]; ok {
				mergeAttrs(prev, e)
				continue
			}
			seen[k] = e
			out = append(out, e)
		}
	}
	for _, e := range state {
		k := model.MustDNKey(e.DN)
		if _, ok := seen[k]; ok {
			// Only possible if a base is inside state_base, which config
			// validation rejects; let Classify report the duplicate.
			out = append(out, e)
			continue
		}
		seen[k] = e
		out = append(out, e)
	}
	return out
}

// mergeAttrs adds src's attribute values that dst lacks (attribute names
// compared case-insensitively). Both are the same server entry read with
// different attribute lists, so values of an attribute present in both are
// identical and dst's are kept.
func mergeAttrs(dst, src *model.Entry) {
	have := map[string]bool{}
	for a := range dst.Attrs {
		have[strings.ToLower(a)] = true
	}
	for a, v := range src.Attrs {
		if !have[strings.ToLower(a)] {
			dst.Attrs[a] = append([]string(nil), v...)
		}
	}
}

// subtree reads every entry under base (paged). A missing base is an error
// unless optional is set.
func (r *Reader) subtree(ctx context.Context, base, name string, attrs []string, optional bool) ([]*model.Entry, error) {
	req := ldap.NewSearchRequest(base, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 0, 0, false, "(objectClass=*)", dedupFold(attrs), nil)
	res, err := ldapconn.SearchPaged(ctx, r.conn, req, pageSize)
	switch {
	case err != nil && optional && ldapconn.IsNoSuchObject(err):
		return nil, nil
	case err != nil && ldapconn.IsNoSuchObject(err):
		return nil, fmt.Errorf("target %s: %s %s does not exist; create it first (Dolly never creates users_base or groups_base)", r.URL, name, base)
	case err != nil:
		return nil, fmt.Errorf("target %s: reading %s %s: %w", r.URL, name, base, err)
	}
	return toEntries(res), nil
}

// LookupUsers finds which of uids have an entry under users_base, without
// reading all of users_base: subtree searches for (|(uid=a)(uid=b)...) in
// chunks of lookupChunk, requesting only uid. A truncated or failed search
// aborts the lookup.
func (r *Reader) LookupUsers(ctx context.Context, uids []string) (*model.UserSet, error) {
	set := model.NewUserSet()
	uniq := map[string]string{}
	for _, u := range uids {
		if u != "" {
			uniq[strings.ToLower(u)] = u
		}
	}
	keys := model.SortedKeys(uniq)
	base := r.cfg.Target.UsersBase
	for i := 0; i < len(keys); i += lookupChunk {
		var chunk []string
		for _, k := range keys[i:min(i+lookupChunk, len(keys))] {
			chunk = append(chunk, uniq[k])
		}
		res, err := ldapconn.Search(ctx, r.conn, lookupRequest(base, chunk))
		switch {
		case err != nil && ldapconn.IsNoSuchObject(err):
			return nil, fmt.Errorf("target %s: users_base %s does not exist", r.URL, base)
		case err != nil:
			return nil, fmt.Errorf("target %s: looking up member uids under %s: %w", r.URL, base, err)
		}
		for _, e := range res {
			set.Add(e.DN, e.GetEqualFoldAttributeValues("uid")...)
		}
	}
	return set, nil
}

// lookupRequest is one existence lookup: a subtree search under base that
// filters only on uid and requests only uid.
//
// spec: group members must exist on the target, but neither side needs
// uidNumber, so the filter never mentions objectClass=posixAccount or
// uidNumber: any entry with the uid counts.
func lookupRequest(base string, uids []string) *ldap.SearchRequest {
	return ldap.NewSearchRequest(base, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 0, 0, false, uidFilter(uids), []string{"uid"}, nil)
}

// uidFilter returns (|(uid=a)(uid=b)...) with each value escaped.
func uidFilter(uids []string) string {
	var f strings.Builder
	f.WriteString("(|")
	for _, u := range uids {
		f.WriteString("(uid=" + ldap.EscapeFilter(u) + ")")
	}
	f.WriteString(")")
	return f.String()
}

func toEntries(in []*ldap.Entry) []*model.Entry {
	out := make([]*model.Entry, 0, len(in))
	for _, e := range in {
		m := &model.Entry{DN: e.DN, Attrs: map[string][]string{}}
		for _, a := range e.Attributes {
			m.Attrs[a.Name] = append(m.Attrs[a.Name], a.Values...)
		}
		out = append(out, m)
	}
	sort.SliceStable(out, func(i, j int) bool { return model.MustDNKey(out[i].DN) < model.MustDNKey(out[j].DN) })
	return out
}

func dedupFold(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, a := range in {
		if k := strings.ToLower(a); !seen[k] {
			seen[k] = true
			out = append(out, a)
		}
	}
	return out
}
