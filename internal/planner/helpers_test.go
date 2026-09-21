package planner

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dirkpetersen/dolly"
	"github.com/dirkpetersen/dolly/internal/config"
	"github.com/dirkpetersen/dolly/internal/model"
	"github.com/dirkpetersen/dolly/internal/source"
)

// Test world: the defaults of dolly.yaml.template.
const (
	adPeople = "OU=People,DC=example,DC=edu"
	adGroups = "OU=Groups,DC=example,DC=edu"
	adOther  = "OU=Elsewhere,DC=example,DC=edu" // outside both search bases
	people   = "ou=people,dc=local"
	groupsOU = "ou=group,dc=local"
	state    = "ou=dolly,dc=local"
	empty    = "cn=empty,dc=local"
)

var now = time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)

// cfg returns the template config, optionally modified.
func cfg(t *testing.T, mod ...func(*config.Config)) *config.Config {
	t.Helper()
	c, err := config.Parse(dolly.ConfigTemplate, "/test/dolly.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range mod {
		m(c)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	return c
}

// uidOnly configures RFC 2307 groups (memberUid only).
func uidOnly(c *config.Config) {
	c.Mapping.Groups.ObjectClasses = []string{"posixGroup"}
	c.Mapping.Groups.Membership = []config.Membership{{Attribute: config.AttrMemberUID}}
}

func guid(n int) string { return fmt.Sprintf("00000000-0000-0000-0000-%012x", n) }

// rec returns the short name opStr uses for a record DN.
func rec(n int) string { return "rec:" + strconv.FormatInt(int64(n), 16) }

func udn(uid string) string { return "uid=" + uid + "," + people }
func gdn(cn string) string  { return "cn=" + cn + "," + groupsOU }

// mkUser is an in-scope, enabled AD user with all required attributes.
func mkUser(n int, uid string) *model.ADObject {
	return &model.ADObject{GUID: guid(n), DN: "CN=" + uid + "," + adPeople, Kind: model.KindUser, InScope: true,
		UserAccountControl: 512,
		Attrs:              map[string][]string{"uid": {uid}, "uidNumber": {strconv.Itoa(1000 + n)}, "gidNumber": {"100"}}}
}

// mkGroup is an in-scope AD group; members are AD DNs.
func mkGroup(n int, name string, members ...string) *model.ADObject {
	return &model.ADObject{GUID: guid(n), DN: "CN=" + name + "," + adGroups, Kind: model.KindGroup, InScope: true,
		Attrs: map[string][]string{"name": {name}, "gidNumber": {strconv.Itoa(5000 + n)}}, Members: members}
}

// tUser is the target entry the default mapping produces for mkUser(n, uid).
func tUser(n int, uid string) *model.Entry {
	return &model.Entry{DN: udn(uid), Attrs: map[string][]string{
		"objectClass": {"account", "posixAccount"}, "uid": {uid}, "cn": {uid},
		"uidNumber": {strconv.Itoa(1000 + n)}, "gidNumber": {"100"},
		"homeDirectory": {"/home/" + uid}, "loginShell": {"/bin/bash"}}}
}

// tGroup is a target group (both schemas) with the given member uids.
func tGroup(n int, cn string, uids ...string) *model.Entry {
	e := &model.Entry{DN: gdn(cn), Attrs: map[string][]string{
		"objectClass": {"groupOfNames", "posixGroup"}, "cn": {cn}, "gidNumber": {strconv.Itoa(5000 + n)}}}
	for _, u := range uids {
		e.AddValue("member", udn(u))
		e.AddValue("memberUid", u)
	}
	return e
}

// tGroupUID is a target RFC 2307 group (memberUid only).
func tGroupUID(n int, cn string, uids ...string) *model.Entry {
	e := &model.Entry{DN: gdn(cn), Attrs: map[string][]string{
		"objectClass": {"posixGroup"}, "cn": {cn}, "gidNumber": {strconv.Itoa(5000 + n)}}}
	for _, u := range uids {
		e.AddValue("memberUid", u)
	}
	return e
}

// uRec and gRec are ownership record entries.
func uRec(n int, uid string, notes ...string) *model.Entry {
	r := &model.Record{Kind: model.UserRecord, GUID: guid(n), SeeAlso: udn(uid), Notes: notes}
	return r.Entry(state)
}

func gRec(n int, cn string, ownedUIDs ...string) *model.Entry {
	r := &model.Record{Kind: model.GroupRecord, GUID: guid(n), SeeAlso: gdn(cn)}
	for _, u := range ownedUIDs {
		r.Occupants = append(r.Occupants, udn(u))
	}
	return r.Entry(state)
}

// containers are the entries that exist on every configured target.
func containers() []*model.Entry {
	var out []*model.Entry
	for _, dn := range []string{people, groupsOU, state, "ou=users," + state, "ou=groups," + state} {
		out = append(out, &model.Entry{DN: dn, Attrs: map[string][]string{"objectClass": {"organizationalUnit"}}})
	}
	return out
}

type world struct {
	ad     []*model.ADObject
	target []*model.Entry
}

// build runs the planner on a world, reading AD through source.Read and
// the target through model.Classify, like a real run.
func build(t *testing.T, c *config.Config, w world, opt Options) *Plan {
	t.Helper()
	fake := &source.Fake{}
	for _, o := range w.ad {
		switch {
		case !o.InScope:
			fake.Others = append(fake.Others, o)
		case o.Kind == model.KindGroup:
			fake.InScopeGroups = append(fake.InScopeGroups, o)
		default:
			fake.InScopeUsers = append(fake.InScopeUsers, o)
		}
	}
	snap, err := source.Read(context.Background(), fake, opt.Users || opt.Adopt)
	if err != nil {
		t.Fatal(err)
	}
	entries := append(containers(), w.target...)
	tgt, recs, err := model.Classify(entries, model.Bases{Users: c.Target.UsersBase, Groups: c.Target.GroupsBase, State: c.Target.StateBase})
	if err != nil {
		t.Fatal(err)
	}
	tgt.UsersRead = true
	if opt.Now.IsZero() {
		opt.Now = now
	}
	p, err := Build(snap, tgt, recs, c, opt)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

var both = Options{Users: true, Groups: true}

// opStr renders an op compactly for comparisons. Record DNs become rec:<n>.
func opStr(op Op) string {
	dn := op.DN
	if strings.HasSuffix(dn, ","+state) && strings.HasPrefix(dn, "cn=0000") {
		n, _ := strconv.ParseInt(strings.TrimLeft(strings.SplitN(strings.TrimPrefix(dn, "cn="), ",", 2)[0][24:], "0"), 16, 64)
		dn = rec(int(n))
	}
	switch op.Kind {
	case AddEntry, AddRecord, CreateContainer:
		show := []string{"loginShell", "member", "memberUid"}
		if op.Kind == AddRecord {
			show = []string{"seeAlso", "roleOccupant", "description"}
		}
		var parts []string
		for _, name := range show {
			for _, a := range op.Attrs {
				if a.Name == name {
					parts = append(parts, a.Name+"="+strings.Join(a.Values, "|"))
				}
			}
		}
		return string(op.Kind) + " " + dn + " " + strings.Join(parts, " ")
	case ModifyEntry, UpdateRecord:
		var parts []string
		for _, c := range op.Changes {
			parts = append(parts, string(c.Type)+" "+c.Attr+"="+strings.Join(c.Values, "|"))
		}
		return string(op.Kind) + " " + dn + " " + strings.Join(parts, "; ")
	case ModRDN:
		return "modrdn " + dn + " -> " + op.NewRDN
	case AddMember, DeleteMember, AddPlaceholder, DeletePlaceholder:
		return string(op.Kind) + " " + dn + " " + op.Attr + "=" + op.Value
	case RenameMember:
		return "rename-member " + dn + " " + op.Attr + " " + op.Old + " -> " + op.Value
	}
	return string(op.Kind) + " " + dn
}

func opStrs(p *Plan) []string {
	var out []string
	for _, op := range p.Ops {
		out = append(out, strings.TrimSpace(opStr(op)))
	}
	return out
}

// wantOps checks the exact operation sequence.
func wantOps(t *testing.T, p *Plan, want ...string) {
	t.Helper()
	got := opStrs(p)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("ops mismatch\n got:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

// index returns the position of the first op whose rendering starts with prefix.
func index(t *testing.T, p *Plan, prefix string) int {
	t.Helper()
	for i, s := range opStrs(p) {
		if strings.HasPrefix(s, prefix) {
			return i
		}
	}
	t.Fatalf("no op starting with %q in\n  %s", prefix, strings.Join(opStrs(p), "\n  "))
	return -1
}

func hasOp(p *Plan, prefix string) bool {
	for _, s := range opStrs(p) {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}

func warnings(p *Plan, k WarningKind) []string {
	var out []string
	for _, w := range p.Warnings {
		if w.Kind == k {
			out = append(out, w.Subject+": "+w.Message)
		}
	}
	return out
}

// missing renders the plan's skipped members as "<uid> in <group DN>: <why>",
// in plan order (sorted by group, then uid).
func missing(p *Plan) []string {
	var out []string
	for _, m := range p.MissingMembers {
		out = append(out, m.UID+" in "+m.Group+": "+m.Why)
	}
	return out
}

// wantMissing checks the exact list of skipped members.
func wantMissing(t *testing.T, p *Plan, want ...string) {
	t.Helper()
	if got := missing(p); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("missing members mismatch\n got:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

// localUID is a local user entry (no record) with only a uid: no uidNumber,
// as in a groups-only deployment whose users come from elsewhere.
func localUID(dn, uid string) *model.Entry {
	return &model.Entry{DN: dn, Attrs: map[string][]string{"objectClass": {"account"}, "uid": {uid}}}
}

func wantWarning(t *testing.T, p *Plan, k WarningKind, substr string) {
	t.Helper()
	for _, w := range warnings(p, k) {
		if strings.Contains(w, substr) {
			return
		}
	}
	t.Errorf("no %s warning containing %q; warnings: %v", k, substr, p.Warnings)
}
