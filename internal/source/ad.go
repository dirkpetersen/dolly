package source

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/go-ldap/ldap/v3"

	"github.com/dirkpetersen/dolly/internal/config"
	"github.com/dirkpetersen/dolly/internal/ldapconn"
	"github.com/dirkpetersen/dolly/internal/model"
)

// lookupWorkers is how many base-scope DN lookups run at once on the one
// connection (go-ldap multiplexes concurrent requests).
const lookupWorkers = 8

// AD is the Source backed by Active Directory.
type AD struct {
	conn *ldap.Conn
	// URL is the domain controller in use.
	URL string
	cfg *config.Config

	userAttrs, groupAttrs, lookupAttrs []string
	// spelling maps a lowercased attribute name to the spelling the config
	// uses, so templates see `.unixHomeDirectory` however AD spells it.
	spelling map[string]string
}

// DialAD connects and binds to the first domain controller in source.urls
// that works. An ldap:// URL is always upgraded with StartTLS: Dolly never
// sends the AD password in the clear. A connect or bind failure moves on to
// the next DC, except for invalid credentials, which would fail on every DC
// and only count toward the account's lockout threshold.
func DialAD(ctx context.Context, cfg *config.Config) (*AD, error) {
	pw, err := cfg.Source.Password()
	if err != nil {
		return nil, fmt.Errorf("source: %w", err)
	}
	var fails []string
	for _, u := range cfg.Source.URLs {
		conn, err := ldapconn.Dial(ctx, ldapconn.Options{
			URL: u, StartTLS: ldapconn.Scheme(u) == "ldap", CAFile: cfg.Source.CAFile,
			Timeout: cfg.Sync.NetworkTimeout.Duration, BindDN: cfg.Source.BindDN, Password: pw,
		})
		if err == nil {
			return newAD(conn, u, cfg), nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		fails = append(fails, fmt.Sprintf("%s: %v", u, err))
		if ldap.IsErrorWithCode(err, ldap.LDAPResultInvalidCredentials) {
			break
		}
	}
	return nil, fmt.Errorf("no AD domain controller could be used:\n  %s", strings.Join(fails, "\n  "))
}

func newAD(conn *ldap.Conn, url string, cfg *config.Config) *AD {
	a := &AD{conn: conn, URL: url, cfg: cfg, spelling: map[string]string{}}
	base := []string{"objectGUID", "objectClass"}
	collect := func(extra []string, attrs map[string]string, required []string) []string {
		out := append(append([]string(nil), base...), extra...)
		for _, name := range model.SortedKeys(attrs) {
			if v, err := config.CompileValue(name, attrs[name]); err == nil {
				out = append(out, v.Sources()...)
			}
		}
		out = append(out, required...)
		return a.dedupAttrs(out)
	}
	a.userAttrs = collect([]string{"userAccountControl"}, cfg.Mapping.Users.Attributes, cfg.Mapping.Users.Required)
	a.groupAttrs = collect([]string{"member"}, cfg.Mapping.Groups.Attributes, cfg.Mapping.Groups.Required)
	a.lookupAttrs = a.dedupAttrs(append(append([]string(nil), a.userAttrs...), a.groupAttrs...))
	return a
}

// dedupAttrs removes case-insensitive duplicates and records spellings.
func (a *AD) dedupAttrs(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, n := range in {
		k := strings.ToLower(n)
		if seen[k] {
			continue
		}
		seen[k] = true
		if _, ok := a.spelling[k]; !ok {
			a.spelling[k] = n
		}
		out = append(out, n)
	}
	return out
}

// Close closes the connection.
func (a *AD) Close() error { return a.conn.Close() }

// Users implements Source: a paged subtree search of source.users.
func (a *AD) Users(ctx context.Context) ([]*model.ADObject, error) {
	return a.search(ctx, a.cfg.Source.Users, a.userAttrs)
}

// Groups implements Source: a paged subtree search of source.groups, with
// every group's members fetched in full by ranged retrieval.
func (a *AD) Groups(ctx context.Context) ([]*model.ADObject, error) {
	return a.search(ctx, a.cfg.Source.Groups, a.groupAttrs)
}

func (a *AD) search(ctx context.Context, s config.Search, attrs []string) ([]*model.ADObject, error) {
	req := ldap.NewSearchRequest(s.Base, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 0, 0, false, s.Filter, attrs, nil)
	entries, err := ldapconn.SearchPaged(ctx, a.conn, req, a.cfg.Source.PageSize)
	if err != nil {
		return nil, fmt.Errorf("AD %s: searching %s with %s: %w", a.URL, s.Base, s.Filter, err)
	}
	out := make([]*model.ADObject, 0, len(entries))
	for _, e := range entries {
		o, err := a.object(ctx, e)
		if err != nil {
			return nil, fmt.Errorf("AD %s: searching %s: %w", a.URL, s.Base, err)
		}
		out = append(out, o)
	}
	return out, nil
}

// Lookup implements Source with base-scope searches, lookupWorkers at a
// time. Each DN is searched with the users filter and, if that finds
// nothing, with the groups filter, so a member that matches neither is
// skipped exactly like one outside the configured scope.
func (a *AD) Lookup(ctx context.Context, dns []string) ([]*model.ADObject, []string, error) {
	type result struct {
		obj      *model.ADObject
		filtered bool
		err      error
	}
	results := make([]result, len(dns))
	jobs := make(chan int)
	var wg sync.WaitGroup
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	for w := 0; w < lookupWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				o, filtered, err := a.lookupOne(ctx, dns[i])
				results[i] = result{o, filtered, err}
				if err != nil {
					cancel()
				}
			}
		}()
	}
feed:
	for i := range dns {
		select {
		case jobs <- i:
		case <-ctx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()

	var found []*model.ADObject
	var filtered []string
	for i, r := range results {
		if r.err != nil && !errors.Is(r.err, context.Canceled) {
			return nil, nil, r.err
		}
		switch {
		case r.obj != nil:
			found = append(found, r.obj)
		case r.filtered:
			filtered = append(filtered, dns[i])
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	return found, filtered, nil
}

// lookupOne fetches one DN. It returns (nil, false, nil) if the DN doesn't
// exist in this domain, and (nil, true, nil) if it exists but matches
// neither filter.
func (a *AD) lookupOne(ctx context.Context, dn string) (*model.ADObject, bool, error) {
	for _, filter := range []string{a.cfg.Source.Users.Filter, a.cfg.Source.Groups.Filter} {
		req := ldap.NewSearchRequest(dn, ldap.ScopeBaseObject, ldap.NeverDerefAliases, 0, 0, false, filter, a.lookupAttrs, nil)
		entries, err := ldapconn.Search(ctx, a.conn, req)
		switch {
		case ldapconn.IsNoSuchObject(err), ldap.IsErrorWithCode(err, ldap.LDAPResultReferral):
			// Gone, or in another domain of the forest: unresolved.
			return nil, false, nil
		case err != nil:
			return nil, false, fmt.Errorf("AD %s: looking up %s: %w", a.URL, dn, err)
		case len(entries) > 0:
			o, err := a.object(ctx, entries[0])
			if err != nil {
				return nil, false, fmt.Errorf("AD %s: looking up %s: %w", a.URL, dn, err)
			}
			return o, false, nil
		}
	}
	return nil, true, nil
}

// object converts an AD entry, fetching the rest of a ranged member list.
func (a *AD) object(ctx context.Context, e *ldap.Entry) (*model.ADObject, error) {
	raw := e.GetEqualFoldRawAttributeValue("objectGUID")
	guid, err := model.FormatGUID(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", e.DN, err)
	}
	o := &model.ADObject{GUID: guid, DN: e.DN, Attrs: map[string][]string{}}
	var members []string
	next := -1
	for _, at := range e.Attributes {
		name := strings.ToLower(at.Name)
		switch {
		case name == "objectguid":
			continue
		case name == "member" || strings.HasPrefix(name, "member;"):
			r, err := parseRange(at.Name)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", e.DN, err)
			}
			members = append(members, at.Values...)
			if r.ranged && !r.last {
				next = r.end + 1
			}
			continue
		}
		key := at.Name
		if s, ok := a.spelling[name]; ok {
			key = s
		}
		o.Attrs[key] = append([]string(nil), at.Values...)
	}
	o.Kind = kindOf(o.Get("objectClass"))
	if v := o.First("userAccountControl"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%s: userAccountControl %q: %w", e.DN, v, err)
		}
		o.UserAccountControl = int(n)
	}
	if next >= 0 {
		more, err := a.rangedMembers(ctx, e.DN, next)
		if err != nil {
			return nil, err
		}
		members = append(members, more...)
	}
	if o.Kind == model.KindGroup {
		o.Members = members
	}
	return o, nil
}

// rangedMembers fetches member values from start on with ranged retrieval
// (member;range=start-*), until AD marks the last chunk with "*". A chunk
// that doesn't continue where the last one ended is an error: the list
// would be incomplete.
func (a *AD) rangedMembers(ctx context.Context, dn string, start int) ([]string, error) {
	var out []string
	for {
		want := fmt.Sprintf("member;range=%d-*", start)
		req := ldap.NewSearchRequest(dn, ldap.ScopeBaseObject, ldap.NeverDerefAliases, 0, 0, false, "(objectClass=*)", []string{want}, nil)
		entries, err := ldapconn.Search(ctx, a.conn, req)
		if err != nil {
			return nil, fmt.Errorf("AD %s: ranged retrieval of member on %s at %d: %w", a.URL, dn, start, err)
		}
		if len(entries) != 1 {
			return nil, fmt.Errorf("AD %s: ranged retrieval of member on %s at %d: group disappeared", a.URL, dn, start)
		}
		var chunk *rangeInfo
		for _, at := range entries[0].Attributes {
			if !strings.HasPrefix(strings.ToLower(at.Name), "member;") {
				continue
			}
			r, err := parseRange(at.Name)
			if err != nil {
				return nil, fmt.Errorf("AD %s: %s: %w", a.URL, dn, err)
			}
			if r.ranged {
				out = append(out, at.Values...)
				chunk = &r
			}
		}
		switch {
		case chunk == nil:
			return nil, fmt.Errorf("AD %s: ranged retrieval of member on %s stopped at %d without a final chunk", a.URL, dn, start)
		case chunk.start != start:
			return nil, fmt.Errorf("AD %s: ranged retrieval of member on %s: asked for %d, got range starting at %d", a.URL, dn, start, chunk.start)
		case chunk.last:
			return out, nil
		case chunk.end < start:
			return nil, fmt.Errorf("AD %s: ranged retrieval of member on %s: range %d-%d doesn't advance", a.URL, dn, chunk.start, chunk.end)
		}
		start = chunk.end + 1
	}
}

// rangeInfo is a parsed attribute description such as member;range=0-1499.
type rangeInfo struct {
	attr       string // attribute name without options
	ranged     bool   // a range option is present
	start, end int    // end is -1 for "*"
	last       bool   // end is "*": this is the final chunk
}

// parseRange parses an attribute description from AD. "member" is not
// ranged; "member;range=0-1499" is a chunk with more to come; and
// "member;range=1500-*" is the final chunk. Other options are ignored.
func parseRange(desc string) (rangeInfo, error) {
	parts := strings.Split(desc, ";")
	r := rangeInfo{attr: parts[0], end: -1}
	for _, opt := range parts[1:] {
		v, ok := cutPrefixFold(opt, "range=")
		if !ok {
			continue
		}
		lo, hi, ok := strings.Cut(v, "-")
		if !ok {
			return r, fmt.Errorf("malformed range option %q", desc)
		}
		s, err := strconv.Atoi(lo)
		if err != nil || s < 0 {
			return r, fmt.Errorf("malformed range option %q", desc)
		}
		r.ranged, r.start = true, s
		if hi == "*" {
			r.last = true
			continue
		}
		e, err := strconv.Atoi(hi)
		if err != nil || e < s-1 {
			return r, fmt.Errorf("malformed range option %q", desc)
		}
		r.end = e
	}
	return r, nil
}

func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix) {
		return s[len(prefix):], true
	}
	return "", false
}

// kindOf classifies an AD object by its objectClass values. Computers are
// objectClass user too, so they are checked first.
func kindOf(classes []string) model.Kind {
	has := map[string]bool{}
	for _, c := range classes {
		has[strings.ToLower(c)] = true
	}
	switch {
	case has["group"]:
		return model.KindGroup
	case has["computer"]:
		return model.KindOther
	case has["user"]:
		return model.KindUser
	}
	return model.KindOther
}
