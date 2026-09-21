package target

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-ldap/ldap/v3"

	"github.com/dirkpetersen/dolly"
	"github.com/dirkpetersen/dolly/internal/config"
	"github.com/dirkpetersen/dolly/internal/fixture"
	"github.com/dirkpetersen/dolly/internal/ldapfake"
	"github.com/dirkpetersen/dolly/internal/model"
	"github.com/dirkpetersen/dolly/internal/planner"
)

// The template's target layout.
const (
	people = "ou=people,dc=local"
	groups = "ou=group,dc=local"
	state  = "ou=dolly,dc=local"
)

func udn(uid string) string { return "uid=" + uid + "," + people }
func gdn(cn string) string  { return "cn=" + cn + "," + groups }
func urec(n string) string {
	return "cn=00000000-0000-0000-0000-00000000000" + n + ",ou=users," + state
}
func grec(n string) string {
	return "cn=00000000-0000-0000-0000-0000000000" + n + ",ou=groups," + state
}

func templateConfig(t *testing.T) *config.Config {
	t.Helper()
	c, err := config.Parse(dolly.ConfigTemplate, "/test/dolly.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// scenario: jdoe is new, carol was renamed from carol in AD to carol2,
// dave is gone from AD. lab is a new group (jdoe, bob); staff exists with
// the owned members carol and dave and the local member local1, and gains
// jdoe.
const scenario = `now: 2024-06-01T00:00:00Z
ad:
  users:
    - {guid: 00000000-0000-0000-0000-000000000001, dn: "CN=jdoe,OU=People,DC=example,DC=edu", attrs: {uid: [jdoe], uidNumber: ["1001"], gidNumber: ["100"]}, userAccountControl: 512}
    - {guid: 00000000-0000-0000-0000-000000000002, dn: "CN=bob,OU=People,DC=example,DC=edu", attrs: {uid: [bob], uidNumber: ["1002"], gidNumber: ["100"]}, userAccountControl: 512}
    - {guid: 00000000-0000-0000-0000-000000000003, dn: "CN=carol,OU=People,DC=example,DC=edu", attrs: {uid: [carol2], uidNumber: ["1003"], gidNumber: ["100"]}, userAccountControl: 512}
  groups:
    - {guid: 00000000-0000-0000-0000-0000000000a1, dn: "CN=lab,OU=Groups,DC=example,DC=edu", attrs: {name: [lab], gidNumber: ["5001"]},
       members: ["CN=jdoe,OU=People,DC=example,DC=edu", "CN=bob,OU=People,DC=example,DC=edu"]}
    - {guid: 00000000-0000-0000-0000-0000000000a2, dn: "CN=staff,OU=Groups,DC=example,DC=edu", attrs: {name: [staff], gidNumber: ["5002"]},
       members: ["CN=jdoe,OU=People,DC=example,DC=edu", "CN=carol,OU=People,DC=example,DC=edu"]}
target:
  entries:
    - {dn: "ou=people,dc=local", attrs: {objectClass: [organizationalUnit], ou: [people]}}
    - {dn: "ou=group,dc=local", attrs: {objectClass: [organizationalUnit], ou: [group]}}
    - {dn: "ou=dolly,dc=local", attrs: {objectClass: [organizationalUnit], ou: [dolly]}}
    - {dn: "ou=users,ou=dolly,dc=local", attrs: {objectClass: [organizationalUnit], ou: [users]}}
    - {dn: "ou=groups,ou=dolly,dc=local", attrs: {objectClass: [organizationalUnit], ou: [groups]}}
    - {dn: "uid=bob,ou=people,dc=local", attrs: {objectClass: [account, posixAccount], uid: [bob], cn: [bob], uidNumber: ["1002"], gidNumber: ["100"], homeDirectory: [/home/bob], loginShell: [/bin/bash]}}
    - {dn: "cn=00000000-0000-0000-0000-000000000002,ou=users,ou=dolly,dc=local", attrs: {objectClass: [organizationalRole], cn: [00000000-0000-0000-0000-000000000002], seeAlso: ["uid=bob,ou=people,dc=local"]}}
    - {dn: "uid=carol,ou=people,dc=local", attrs: {objectClass: [account, posixAccount], uid: [carol], cn: [carol2], uidNumber: ["1003"], gidNumber: ["100"], homeDirectory: [/home/carol2], loginShell: [/bin/bash]}}
    - {dn: "cn=00000000-0000-0000-0000-000000000003,ou=users,ou=dolly,dc=local", attrs: {objectClass: [organizationalRole], cn: [00000000-0000-0000-0000-000000000003], seeAlso: ["uid=carol,ou=people,dc=local"]}}
    - {dn: "uid=dave,ou=people,dc=local", attrs: {objectClass: [account, posixAccount], uid: [dave], cn: [dave], uidNumber: ["1004"], gidNumber: ["100"], homeDirectory: [/home/dave], loginShell: [/bin/bash]}}
    - {dn: "cn=00000000-0000-0000-0000-000000000004,ou=users,ou=dolly,dc=local", attrs: {objectClass: [organizationalRole], cn: [00000000-0000-0000-0000-000000000004], seeAlso: ["uid=dave,ou=people,dc=local"]}}
    - {dn: "cn=staff,ou=group,dc=local", attrs: {objectClass: [groupOfNames, posixGroup], cn: [staff], gidNumber: ["5002"],
       member: ["uid=carol,ou=people,dc=local", "uid=dave,ou=people,dc=local", "uid=local1,ou=people,dc=local"], memberUid: [carol, dave, local1]}}
    - {dn: "cn=00000000-0000-0000-0000-0000000000a2,ou=groups,ou=dolly,dc=local", attrs: {objectClass: [organizationalRole], cn: [00000000-0000-0000-0000-0000000000a2],
       seeAlso: ["cn=staff,ou=group,dc=local"], roleOccupant: ["uid=carol,ou=people,dc=local", "uid=dave,ou=people,dc=local"]}}
`

// load builds the scenario's plan and a fake directory holding its target
// entries.
func load(t *testing.T) (*planner.Plan, *ldapfake.Dir) {
	t.Helper()
	p, dir, _ := loadCfg(t, templateConfig(t))
	return p, dir
}

// loadCfg is load with a given configuration; it also returns the
// snapshots, for re-planning against the fake directory.
func loadCfg(t *testing.T, cfg *config.Config) (*planner.Plan, *ldapfake.Dir, *fixture.Snapshots) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "scenario.yaml")
	if err := os.WriteFile(path, []byte(scenario), 0o600); err != nil {
		t.Fatal(err)
	}
	snaps, err := fixture.Load(path, cfg, true)
	if err != nil {
		t.Fatal(err)
	}
	p, err := planner.Build(snaps.AD, snaps.Target, snaps.Records, cfg, planner.Options{Users: true, Groups: true, Now: snaps.Now})
	if err != nil {
		t.Fatal(err)
	}
	dir := ldapfake.New()
	var entries []*model.Entry
	entries = append(entries, snaps.Target.Users...)
	entries = append(entries, snaps.Target.Groups...)
	for _, dn := range []string{people, groups, state, "ou=users," + state, "ou=groups," + state} {
		attr, val, _ := model.RDN(dn)
		entries = append(entries, &model.Entry{DN: dn, Attrs: map[string][]string{"objectClass": {"organizationalUnit"}, attr: {val}}})
	}
	for _, r := range append(snaps.Records.Users, snaps.Records.Groups...) {
		entries = append(entries, r.Entry(state))
	}
	for _, e := range entries {
		dir.Put(e.DN, e.Attrs, time.Now())
	}
	return p, dir, snaps
}

// replan reads the fake directory back and plans again against the same AD
// snapshot.
func replan(t *testing.T, cfg *config.Config, dir *ldapfake.Dir, snaps *fixture.Snapshots) *planner.Plan {
	t.Helper()
	var entries []*model.Entry
	for _, dn := range dir.DNs() {
		entries = append(entries, &model.Entry{DN: dn, Attrs: dir.Get(dn)})
	}
	tgt, recs, err := model.Classify(entries, model.Bases{Users: people, Groups: groups, State: state})
	if err != nil {
		t.Fatal(err)
	}
	tgt.UsersRead = true
	p, err := planner.Build(snaps.AD, tgt, recs, cfg, planner.Options{Users: true, Groups: true, Now: snaps.Now})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// With OpenLDAP's refint overlay, the modrdn of carol -> carol2 already
// rewrites member and roleOccupant values everywhere, so the planned
// fix-ups find the old value gone and the new one present: already done,
// not a failure (and nothing after them is skipped).
func TestApplyRefint(t *testing.T) {
	for _, mode := range []string{"member", "memberUid"} {
		t.Run(mode, func(t *testing.T) {
			cfg := templateConfig(t)
			if mode == "memberUid" {
				cfg.Mapping.Groups.ObjectClasses = []string{"posixGroup"}
				cfg.Mapping.Groups.Membership = []config.Membership{{Attribute: config.AttrMemberUID}}
				if err := cfg.Validate(); err != nil {
					t.Fatal(err)
				}
			}
			p, dir, snaps := loadCfg(t, cfg)
			dir.Refint = []string{"member", "roleOccupant", "seeAlso"}
			var log bytes.Buffer
			res := Apply(context.Background(), dir, p, &log, true)
			if res.Failed != 0 || res.Skipped != 0 || !res.OK() {
				var out bytes.Buffer
				res.Print(&out)
				t.Fatalf("refint apply:\n%s\n%s", out.String(), log.String())
			}
			if res.AlreadyDone == 0 || !strings.Contains(log.String(), "already renamed by the server (refint?)") {
				t.Errorf("no fix-up was recognized as done by the server:\n%s", log.String())
			}
			if occ := dir.Values(grec("a2"), "roleOccupant"); has(occ, udn("carol")) || !has(occ, udn("carol2")) {
				t.Errorf("staff record occupants %v", occ)
			}
			if m := dir.Values(gdn("staff"), "memberUid"); has(m, "carol") || !has(m, "carol2") {
				t.Errorf("staff memberUid %v", m)
			}
			if q := replan(t, cfg, dir, snaps); !q.Empty() {
				var out bytes.Buffer
				q.Print(&out)
				t.Errorf("not converged:\n%s", out.String())
			}
		})
	}
}

// The fallbacks after a failed value rename: the old value already gone and
// the new one missing is left alone (it may have been a local member that was
// removed concurrently; an owned one is re-added by the next run); both
// present deletes the old one.
func TestApplyRenameValueFallbacks(t *testing.T) {
	dir := ldapfake.New()
	dir.Put(gdn("g"), map[string][]string{"objectClass": {"groupOfNames", "posixGroup"}, "cn": {"g"}, "gidNumber": {"5000"},
		"member": {udn("b"), udn("c")}, "memberUid": {"b", "c"}}, time.Now())
	dir.Put(grec("a1"), map[string][]string{"objectClass": {"organizationalRole"}, "cn": {"x"}, "roleOccupant": {udn("c")}}, time.Now())
	ren := func(dn, attr, old, value string) planner.Op {
		return planner.Op{Kind: planner.RenameMember, DN: dn, Attr: attr, Old: old, Value: value}
	}
	ops := []planner.Op{
		ren(gdn("g"), "member", udn("a"), udn("a2")), // both gone: nothing to rename
		ren(gdn("g"), "member", udn("b"), udn("c")),  // both present: delete old
		ren(gdn("g"), "memberUid", "b", "c"),         // both present: delete old
		ren(gdn("g"), "memberUid", "zz", "c"),        // old gone, new present: done
		{Kind: planner.UpdateRecord, DN: grec("a1"), Changes: []planner.Change{ // old gone, new present: done
			{Type: planner.Delete, Attr: "roleOccupant", Values: []string{udn("a")}},
			{Type: planner.Add, Attr: "roleOccupant", Values: []string{udn("c")}}}},
		ren(gdn("g"), "member", udn("q"), udn("Q")), // case-only, old gone: can't tell, fails
	}
	res := Apply(context.Background(), dir, &planner.Plan{Ops: ops}, &bytes.Buffer{}, false)
	want := []Outcome{AlreadyDone, Applied, Applied, AlreadyDone, AlreadyDone, Failed}
	for i, r := range res.Ops {
		if r.Outcome != want[i] {
			t.Errorf("op %d %s: %s (%v), want %s", i+1, r.Op, r.Outcome, r.Err, want[i])
		}
	}
	m := dir.Values(gdn("g"), "member")
	if len(m) != 1 || has(m, udn("a2")) || !has(m, udn("c")) {
		t.Errorf("member %v", m)
	}
	if u := dir.Values(gdn("g"), "memberUid"); len(u) != 1 || u[0] != "c" {
		t.Errorf("memberUid %v", u)
	}
}

func has(vals []string, v string) bool {
	for _, x := range vals {
		if model.DNEqual(x, v) || x == v {
			return true
		}
	}
	return false
}

func TestApplyScenario(t *testing.T) {
	p, dir := load(t)
	var log bytes.Buffer
	res := Apply(context.Background(), dir, p, &log, true)
	if !res.OK() || res.Applied != len(p.Ops) {
		t.Fatalf("result %+v\n%s", res, log.String())
	}
	// Plan order, exactly: every write the fake saw is the next planned op.
	var writes []string
	for _, c := range dir.Calls {
		if !strings.HasPrefix(c, "search ") {
			writes = append(writes, c)
		}
	}
	if len(writes) != len(p.Ops) {
		t.Fatalf("%d writes for %d ops", len(writes), len(p.Ops))
	}
	for i, op := range p.Ops {
		if !strings.HasSuffix(writes[i], " "+op.DN) {
			t.Errorf("write %d is %q, want op %s", i+1, writes[i], op)
		}
	}
	if strings.Count(log.String(), "debug: step ") != len(p.Ops) {
		t.Errorf("--debug should list each applied op:\n%s", log.String())
	}

	if !dir.Has(udn("jdoe")) || !dir.Has(urec("1")) {
		t.Error("jdoe not created")
	}
	if dir.Has(udn("carol")) || !dir.Has(udn("carol2")) {
		t.Error("carol not renamed")
	}
	if n := dir.Values(urec("3"), "description"); len(n) != 0 {
		t.Errorf("carol's record keeps notes %v", n)
	}
	staff := dir.Get(gdn("staff"))
	for _, want := range []string{udn("carol2"), udn("jdoe"), udn("local1")} {
		if !has(staff["member"], want) {
			t.Errorf("staff lacks member %s: %v", want, staff["member"])
		}
	}
	if has(staff["member"], udn("dave")) || has(staff["memberUid"], "dave") || has(staff["member"], udn("carol")) {
		t.Errorf("staff still has dave or carol: %v %v", staff["member"], staff["memberUid"])
	}
	if occ := dir.Values(grec("a2"), "roleOccupant"); len(occ) != 2 || !has(occ, udn("carol2")) || !has(occ, udn("jdoe")) {
		t.Errorf("staff record occupants %v", occ)
	}
	if m := dir.Values(gdn("lab"), "memberUid"); len(m) != 2 {
		t.Errorf("lab members %v", m)
	}
	if !strings.HasPrefix(strings.Join(dir.Values(urec("4"), "description"), ""), "missing-since=") {
		t.Error("dave's prune clock not started")
	}

	var out bytes.Buffer
	res.Print(&out)
	for _, want := range []string{
		"Result: " + strconv.Itoa(len(p.Ops)) + " applied, 0 already in place, 0 skipped because an earlier step failed, 0 failed\n",
		"Created groups (1)\n  lab (2 members)\n",
		"Changed groups (1)\n  staff +1 -1 (1 renamed): +jdoe -dave\n",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}

	// Applying the same plan again (a re-run after a crash at the very
	// end): the adds and single-value changes are already in place, and
	// only operations whose source is gone fail.
	res2 := Apply(context.Background(), dir, p, &log, false)
	if res2.AlreadyDone == 0 {
		t.Errorf("re-apply: nothing already in place: %+v", res2)
	}
}

// failOn fails the first operation matching op and dn (and, if set, a
// modify that touches value).
func failOn(op, dn, value string) func(string, string, any) error {
	return func(o, d string, req any) error {
		if o != op || !model.DNEqual(d, dn) {
			return nil
		}
		if m, ok := req.(*ldap.ModifyRequest); ok && value != "" {
			hit := false
			for _, ch := range m.Changes {
				for _, v := range ch.Modification.Vals {
					if v == value {
						hit = true
					}
				}
			}
			if !hit {
				return nil
			}
		}
		return ldapfake.Error(ldap.LDAPResultConstraintViolation, "injected failure")
	}
}

func outcomes(res *Result, o Outcome) []string {
	var out []string
	for _, r := range res.Ops {
		if r.Outcome == o {
			out = append(out, r.Op.String())
		}
	}
	return out
}

// Record first: a failed user record skips the user's entry, and a user
// that wasn't created joins no group: the new group lab waits for the next
// run, and staff doesn't get jdoe. Everything else is applied.
func TestApplyRecordFailureSkipsEntry(t *testing.T) {
	p, dir := load(t)
	dir.Fail = failOn("add", urec("1"), "")
	var log bytes.Buffer
	res := Apply(context.Background(), dir, p, &log, false)
	if res.Failed != 1 || res.OK() {
		t.Fatalf("result %+v", res)
	}
	skipped := strings.Join(outcomes(res, Skipped), "\n")
	for _, want := range []string{"add " + udn("jdoe"), "add " + gdn("lab"), "add record " + grec("a1"),
		gdn("staff") + ": add member " + udn("jdoe"), gdn("staff") + ": add memberUid jdoe"} {
		if !strings.Contains(skipped, want) {
			t.Errorf("not skipped: %s\nskipped:\n%s", want, skipped)
		}
	}
	if dir.Has(udn("jdoe")) || dir.Has(gdn("lab")) || has(dir.Values(grec("a2"), "roleOccupant"), udn("jdoe")) {
		t.Error("a dependent op ran")
	}
	if !dir.Has(udn("carol2")) || has(dir.Values(gdn("staff"), "member"), udn("dave")) {
		t.Error("an independent op didn't run")
	}
	if !strings.Contains(log.String(), "dolly: step ") || !strings.Contains(log.String(), "failed: add record "+urec("1")) {
		t.Errorf("failure not logged with step and DN:\n%s", log.String())
	}
	var out bytes.Buffer
	res.Print(&out)
	if !strings.Contains(out.String(), "Failed (1)") || !strings.Contains(out.String(), "needs step ") {
		t.Errorf("output:\n%s", out.String())
	}
}

// A failed modrdn skips the reference fix-ups and the "rename complete"
// note, so the next run can finish the rename; staff's other changes go on.
func TestApplyModRDNFailure(t *testing.T) {
	p, dir := load(t)
	dir.Fail = failOn("modifydn", udn("carol"), "")
	res := Apply(context.Background(), dir, p, &bytes.Buffer{}, false)
	if res.Failed != 1 || res.Skipped < 4 {
		t.Fatalf("result %+v\nskipped: %v", res, outcomes(res, Skipped))
	}
	if !has(dir.Values(gdn("staff"), "member"), udn("carol")) || has(dir.Values(gdn("staff"), "member"), udn("carol2")) {
		t.Error("staff's carol member was renamed although the modrdn failed")
	}
	if n := dir.Values(urec("3"), "description"); len(n) != 1 || !strings.HasPrefix(n[0], "renaming-from=") {
		t.Errorf("carol's renaming-from note: %v", n)
	}
	if !has(dir.Values(gdn("staff"), "member"), udn("jdoe")) || has(dir.Values(gdn("staff"), "member"), udn("dave")) {
		t.Error("staff's independent member changes didn't run")
	}
}

// A failed member delete keeps the roleOccupant, so the member stays owned
// and is removed on the next run instead of turning local.
func TestApplyMemberDeleteFailure(t *testing.T) {
	p, dir := load(t)
	dir.Fail = failOn("modify", gdn("staff"), udn("dave"))
	res := Apply(context.Background(), dir, p, &bytes.Buffer{}, false)
	if res.Failed != 1 || res.Skipped != 2 {
		t.Fatalf("result %+v\nskipped: %v", res, outcomes(res, Skipped))
	}
	if !has(dir.Values(grec("a2"), "roleOccupant"), udn("dave")) || !has(dir.Values(gdn("staff"), "memberUid"), "dave") {
		t.Error("dave's ownership or memberUid was removed after the member delete failed")
	}
	if !has(dir.Values(gdn("staff"), "member"), udn("jdoe")) {
		t.Error("jdoe wasn't added")
	}
}

// Independent failures are all collected; the run continues.
func TestApplyCollectsErrors(t *testing.T) {
	p, dir := load(t)
	f1, f2 := failOn("add", udn("jdoe"), ""), failOn("modifydn", udn("carol"), "")
	dir.Fail = func(o, d string, req any) error {
		if err := f1(o, d, req); err != nil {
			return err
		}
		return f2(o, d, req)
	}
	var log bytes.Buffer
	res := Apply(context.Background(), dir, p, &log, false)
	if res.Failed != 2 || res.Applied == 0 || res.OK() {
		t.Fatalf("result %+v", res)
	}
	if strings.Count(log.String(), " failed: ") != 2 {
		t.Errorf("log:\n%s", log.String())
	}
}

// Once the context ends (SIGTERM, run_timeout), no further op starts.
func TestApplyStopsOnCancel(t *testing.T) {
	p, dir := load(t)
	ctx, cancel := context.WithCancel(context.Background())
	n := 0
	dir.Before = func(op, dn string) {
		if n++; n == 3 {
			cancel()
		}
	}
	res := Apply(ctx, dir, p, &bytes.Buffer{}, false)
	if res.Applied != 3 || res.NotRun != len(p.Ops)-3 || res.Stopped == nil || res.OK() {
		t.Fatalf("result %+v", res)
	}
	if len(dir.Calls) != 3 {
		t.Errorf("calls after cancel: %v", dir.Calls)
	}
}

// Handcrafted plans for the idempotency rules and the generic dependency
// mechanism.
func TestApplyIdempotent(t *testing.T) {
	dir := ldapfake.New()
	dir.Put(gdn("g"), map[string][]string{"objectClass": {"groupOfNames", "posixGroup"}, "cn": {"g"}, "gidNumber": {"5000"},
		"member": {udn("a")}, "memberUid": {"a"}}, time.Now())
	dir.Put(urec("1"), map[string][]string{"objectClass": {"organizationalRole"}, "cn": {"x"}, "seeAlso": {udn("A")}}, time.Now())
	ops := []planner.Op{
		{Kind: planner.AddMember, DN: gdn("g"), Attr: "member", Value: "UID=a," + people}, // exists (DN match)
		{Kind: planner.AddMember, DN: gdn("g"), Attr: "memberUid", Value: "a"},            // exists
		{Kind: planner.DeleteMember, DN: gdn("g"), Attr: "memberUid", Value: "zz"},        // gone
		{Kind: planner.UpdateRecord, DN: urec("1"), Changes: []planner.Change{{Type: planner.Add, Attr: "roleOccupant", Values: []string{udn("q")}}}},
		{Kind: planner.UpdateRecord, DN: urec("1"), Changes: []planner.Change{{Type: planner.Add, Attr: "roleOccupant", Values: []string{udn("q")}}}},                                      // exists now
		{Kind: planner.AddRecord, DN: urec("1"), Attrs: []planner.Attribute{{Name: "objectClass", Values: []string{"OrganizationalRole"}}, {Name: "seeAlso", Values: []string{udn("a")}}}}, // same content
		{Kind: planner.AddRecord, DN: urec("1"), Attrs: []planner.Attribute{{Name: "seeAlso", Values: []string{udn("b")}}}},                                                                // different content
		{Kind: planner.RenameMember, DN: gdn("g"), Attr: "memberUid", Old: "missing", Value: "a"},                                                                                          // old gone, new present: done
		{Kind: planner.ModifyEntry, DN: gdn("g"), Changes: []planner.Change{{Type: planner.Replace, Attr: "memberUid", Values: []string{"x"}}}},                                            // refused
		{Kind: planner.DeleteEntry, DN: udn("nobody")},                                                                                                                                     // noSuchObject is an error
	}
	res := Apply(context.Background(), dir, &planner.Plan{Ops: ops}, &bytes.Buffer{}, false)
	want := []Outcome{AlreadyDone, AlreadyDone, AlreadyDone, Applied, AlreadyDone, AlreadyDone, Failed, AlreadyDone, Failed, Failed}
	for i, r := range res.Ops {
		if r.Outcome != want[i] {
			t.Errorf("op %d %s: %s (%v), want %s", i+1, r.Op, r.Outcome, r.Err, want[i])
		}
	}
	if !strings.Contains(res.Ops[8].Err.Error(), "refusing to replace the whole memberUid") {
		t.Errorf("replace error: %v", res.Ops[8].Err)
	}
	if !has(dir.Values(gdn("g"), "memberUid"), "a") {
		t.Error("memberUid was replaced")
	}
}

// Skips propagate: an op skipped because of a failure breaks what it
// provides too, and the blocker is the original failure.
func TestApplyDependencyChain(t *testing.T) {
	dir := ldapfake.New()
	dir.Fail = failOn("add", "cn=a,dc=x", "")
	ops := []planner.Op{
		{Kind: planner.AddEntry, DN: "cn=a,dc=x", Attrs: []planner.Attribute{{Name: "cn", Values: []string{"a"}}}, Provides: []string{"k1"}},
		{Kind: planner.AddEntry, DN: "cn=b,dc=x", Attrs: []planner.Attribute{{Name: "cn", Values: []string{"b"}}}, Needs: []string{"k1"}, Provides: []string{"k2"}},
		{Kind: planner.AddEntry, DN: "cn=c,dc=x", Attrs: []planner.Attribute{{Name: "cn", Values: []string{"c"}}}, Needs: []string{"k2"}},
		{Kind: planner.AddEntry, DN: "cn=d,dc=x", Attrs: []planner.Attribute{{Name: "cn", Values: []string{"d"}}}, Needs: []string{"other"}},
	}
	res := Apply(context.Background(), dir, &planner.Plan{Ops: ops}, &bytes.Buffer{}, false)
	got := []Outcome{res.Ops[0].Outcome, res.Ops[1].Outcome, res.Ops[2].Outcome, res.Ops[3].Outcome}
	if got[0] != Failed || got[1] != Skipped || got[2] != Skipped || got[3] != Applied {
		t.Errorf("outcomes %v", got)
	}
	if res.Ops[2].Blocker != 1 || res.Ops[2].Key != "k2" {
		t.Errorf("blocker %d key %s", res.Ops[2].Blocker, res.Ops[2].Key)
	}
}
