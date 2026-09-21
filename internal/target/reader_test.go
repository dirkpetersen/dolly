package target

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-ldap/ldap/v3"

	"github.com/dirkpetersen/dolly/internal/ldapfake"
	"github.com/dirkpetersen/dolly/internal/model"
	"github.com/dirkpetersen/dolly/internal/planner"
	"github.com/dirkpetersen/dolly/internal/source"
)

func TestUIDFilter(t *testing.T) {
	f := uidFilter([]string{"jdoe", "a*b", "x)(uid=*"})
	if f != `(|(uid=jdoe)(uid=a\2ab)(uid=x\29\28uid=\2a))` {
		t.Errorf("filter = %s", f)
	}
	if _, err := ldap.CompileFilter(f); err != nil {
		t.Errorf("filter doesn't compile: %v", err)
	}
}

// The existence lookup of a groups-only run matches on uid alone and asks
// only for uid: a target entry without uidNumber or posixAccount counts.
func TestLookupRequestNeedsOnlyUID(t *testing.T) {
	req := lookupRequest("ou=people,dc=local", []string{"jdoe", "bob"})
	if req.Filter != "(|(uid=jdoe)(uid=bob))" {
		t.Errorf("filter = %s, want only (uid=...) terms", req.Filter)
	}
	for _, bad := range []string{"objectclass", "posixaccount", "uidnumber", "gidnumber", "&"} {
		if strings.Contains(strings.ToLower(req.Filter), bad) {
			t.Errorf("filter %s must not contain %q", req.Filter, bad)
		}
	}
	if len(req.Attributes) != 1 || req.Attributes[0] != "uid" {
		t.Errorf("attributes = %v, want only uid", req.Attributes)
	}
	if req.BaseDN != "ou=people,dc=local" || req.Scope != ldap.ScopeWholeSubtree {
		t.Errorf("base %q scope %d", req.BaseDN, req.Scope)
	}
}

func TestToEntriesMergesAttributes(t *testing.T) {
	in := []*ldap.Entry{ldap.NewEntry("cn=b,ou=group,dc=local", map[string][]string{"memberUid": {"x", "y"}}),
		ldap.NewEntry("cn=a,ou=group,dc=local", map[string][]string{"cn": {"a"}})}
	out := toEntries(in)
	if len(out) != 2 || out[0].DN != "cn=a,ou=group,dc=local" || len(out[1].Get("memberuid")) != 2 {
		t.Errorf("entries = %+v", out)
	}
}

// fakeSubtree reads dir like Reader.subtree reads a server: every entry at
// or under base, with only the requested attributes.
func fakeSubtree(dir *ldapfake.Dir) subtreeFunc {
	return func(base, name string, attrs []string, optional bool) ([]*model.Entry, error) {
		if !dir.Has(base) {
			if optional {
				return nil, nil
			}
			return nil, fmt.Errorf("%s %s does not exist", name, base)
		}
		var out []*ldap.Entry
		for _, dn := range dir.DNs() {
			if !model.IsUnder(dn, base) {
				continue
			}
			res, err := dir.Search(ldap.NewSearchRequest(dn, ldap.ScopeBaseObject, ldap.NeverDerefAliases, 0, 0, false, "(objectClass=*)", dedupFold(attrs), nil))
			if err != nil {
				return nil, err
			}
			out = append(out, res.Entries...)
		}
		return toEntries(out), nil
	}
}

// Production bug: with state_base inside groups_base, the groups_base read
// also returned the ownership records, without seeAlso ("want exactly one
// seeAlso, got 0"). The records must come only from the state_base read,
// Dolly's containers must never look like groups, and a second run must
// plan nothing.
func TestReadStateBaseInsideGroupsBase(t *testing.T) {
	cfg := templateConfig(t)
	cfg.Target.StateBase = "ou=dolly," + groups
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	dir := ldapfake.New()
	for _, dn := range []string{people, groups} {
		attr, val, _ := model.RDN(dn)
		dir.Put(dn, map[string][]string{"objectClass": {"organizationalUnit"}, attr: {val}}, time.Now())
	}
	user := func(n int, uid string) *model.ADObject {
		return &model.ADObject{GUID: fmt.Sprintf("00000000-0000-0000-0000-%012x", n), DN: "CN=" + uid + ",OU=People,DC=example,DC=edu",
			Kind: model.KindUser, InScope: true, UserAccountControl: 512,
			Attrs: map[string][]string{"uid": {uid}, "uidNumber": {strconv.Itoa(1000 + n)}, "gidNumber": {"100"}}}
	}
	a, b := user(1, "a"), user(2, "b")
	g := &model.ADObject{GUID: "00000000-0000-0000-0000-0000000000a1", DN: "CN=lab,OU=Groups,DC=example,DC=edu", Kind: model.KindGroup, InScope: true,
		Attrs: map[string][]string{"name": {"lab"}, "gidNumber": {"5001"}}, Members: []string{a.DN, b.DN}}
	ad, err := source.Read(context.Background(), &source.Fake{InScopeUsers: []*model.ADObject{a, b}, InScopeGroups: []*model.ADObject{g}}, true)
	if err != nil {
		t.Fatal(err)
	}
	plan := func() (*planner.Plan, *model.TargetSnapshot) {
		t.Helper()
		tgt, recs, err := readSnapshot(cfg, "fake", true, fakeSubtree(dir))
		if err != nil {
			t.Fatal(err)
		}
		p, err := planner.Build(ad, tgt, recs, cfg, planner.Options{Users: true, Groups: true, Now: time.Now()})
		if err != nil {
			t.Fatal(err)
		}
		return p, tgt
	}

	p, _ := plan()
	var log bytes.Buffer
	if res := Apply(context.Background(), dir, p, &log, true); !res.OK() {
		var out bytes.Buffer
		res.Print(&out)
		t.Fatalf("first apply:\n%s\n%s", out.String(), log.String())
	}
	rec := "cn=" + g.GUID + ",ou=groups,ou=dolly," + groups
	if see := dir.Values(rec, "seeAlso"); len(see) != 1 || !model.DNEqual(see[0], gdn("lab")) {
		t.Fatalf("group record seeAlso = %v", see)
	}

	q, tgt := plan()
	if !q.Empty() {
		var out bytes.Buffer
		q.Print(&out)
		t.Errorf("not converged:\n%s", out.String())
	}
	if len(tgt.Groups) != 1 || !model.DNEqual(tgt.Groups[0].DN, gdn("lab")) {
		var dns []string
		for _, e := range tgt.Groups {
			dns = append(dns, e.DN)
		}
		t.Errorf("target groups = %v, want only %s", dns, gdn("lab"))
	}
	if !tgt.HasStateBase || !tgt.HasUserRecords || !tgt.HasGroupRecords {
		t.Errorf("containers not seen: %+v", tgt)
	}

	// The naive concatenation that caused the bug is now a clear error
	// instead of a silently stripped record.
	read := fakeSubtree(dir)
	gr, _ := read(groups, "groups_base", []string{"objectClass", "cn", "member"}, false)
	st, _ := read(cfg.Target.StateBase, "state_base", []string{"objectClass", "seeAlso", "roleOccupant"}, true)
	_, _, err = model.Classify(append(gr, st...), model.Bases{Users: people, Groups: groups, State: cfg.Target.StateBase})
	if err == nil || !strings.Contains(err.Error(), "appears twice") {
		t.Errorf("duplicate DN: %v", err)
	}
}

// Entries returned by both the users_base and groups_base reads (one base
// nested in the other) are merged into one entry with both attribute sets.
func TestCombineMergesNestedBases(t *testing.T) {
	gr := []*model.Entry{{DN: "uid=a,ou=people,ou=group,dc=local", Attrs: map[string][]string{"objectClass": {"account"}, "cn": {"a"}}}}
	us := []*model.Entry{{DN: "UID=a,ou=People,ou=group,dc=local", Attrs: map[string][]string{"objectclass": {"account"}, "uid": {"a"}}}}
	st := []*model.Entry{{DN: "ou=dolly,dc=local"}}
	out := combine("ou=dolly,dc=local", gr, us, st)
	if len(out) != 2 || len(out[0].Get("uid")) != 1 || len(out[0].Get("cn")) != 1 || len(out[0].Attrs) != 3 {
		t.Errorf("combined = %+v", out)
	}
}
