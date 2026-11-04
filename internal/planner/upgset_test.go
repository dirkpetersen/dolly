package planner

import (
	"strconv"
	"strings"
	"testing"

	"github.com/dirkpetersen/dolly/internal/config"
	"github.com/dirkpetersen/dolly/internal/model"
)

// The UPG set (source.upg, target.upg_base): a second group set with its
// own target base and record container (ou=upg,<state_base>).
const (
	adUPGs = "OU=UPG,DC=example,DC=edu"
	upgOU  = "ou=private,dc=local"
)

func pdn(cn string) string { return "cn=" + cn + "," + upgOU }

// upgCfg configures a UPG set made from AD groups or from the users.
func upgCfg(from string) func(*config.Config) {
	return func(c *config.Config) {
		c.Source.UPG = &config.UPGSource{From: from}
		if from == config.UPGFromAD {
			c.Source.UPG.Base, c.Source.UPG.Filter = adUPGs, "(objectClass=group)"
		}
		c.Target.UPGBase = upgOU
	}
}

// mkUPG is an AD group of the UPG search.
func mkUPG(n int, name string, members ...string) *model.ADObject {
	g := mkGroup(n, name, members...)
	g.DN = "CN=" + name + "," + adUPGs
	return g
}

// tUPG is a target UPG (both schemas) with the given gidNumber and member uids.
func tUPG(cn string, gid int, uids ...string) *model.Entry {
	e := &model.Entry{DN: pdn(cn), Attrs: map[string][]string{
		"objectClass": {"groupOfNames", "posixGroup"}, "cn": {cn}, "gidNumber": {strconv.Itoa(gid)}}}
	for _, u := range uids {
		e.AddValue("member", udn(u))
		e.AddValue("memberUid", u)
	}
	return e
}

// pRec is a UPG-set ownership record.
func pRec(n int, cn string, ownedUIDs ...string) *model.Entry {
	r := &model.Record{Kind: model.UPGRecord, GUID: guid(n), SeeAlso: pdn(cn)}
	for _, u := range ownedUIDs {
		r.Occupants = append(r.Occupants, udn(u))
	}
	return r.Entry(state)
}

// Rule (UPG set, from: ad): AD groups of the UPG search go to upg_base,
// with records in ou=upg, under the same rules as shared groups. An AD UPG
// that is a member of a shared group is not flattened into it; it counts as
// a skipped non-user member.
func TestRuleUPGFromAD(t *testing.T) {
	jdoe := mkUser(1, "jdoe")
	c := cfg(t, upgCfg(config.UPGFromAD))
	t.Run("created, and not flattened into a shared group", func(t *testing.T) {
		p := build(t, c, world{
			ad:     []*model.ADObject{jdoe, mkGroup(10, "hpc", "CN=u-jdoe,"+adUPGs)},
			upgs:   []*model.ADObject{mkUPG(20, "u-jdoe", jdoe.DN)},
			target: []*model.Entry{tUser(1, "jdoe"), uRec(1, "jdoe")},
		}, both)
		wantOps(t, p,
			"add-record upgrec:14 seeAlso="+pdn("u-jdoe")+" roleOccupant="+udn("jdoe"),
			"add-entry "+pdn("u-jdoe")+" member="+udn("jdoe")+" memberUid=jdoe",
		)
		wantWarning(t, p, WarnPending, gdn("hpc")) // hpc has no member left: not created
		if p.Counts.SkippedMembers != 1 || p.Counts.ADGroups != 1 || p.Counts.ADUPGs != 1 || p.Counts.UPGsAdded != 1 || p.Counts.GroupsAdded != 0 {
			t.Errorf("counts: %+v", p.Counts)
		}
		if op := p.Ops[1]; op.Section != SectionUPGs {
			t.Errorf("section %q", op.Section)
		}
	})
	t.Run("gone from AD: owned members and record go, the group stays", func(t *testing.T) {
		p := build(t, c, world{
			ad:     []*model.ADObject{jdoe},
			target: []*model.Entry{tUser(1, "jdoe"), uRec(1, "jdoe"), tUPG("u-jdoe", 5020, "jdoe", "local1"), pRec(20, "u-jdoe", "jdoe")},
		}, both)
		wantOps(t, p,
			"delete-member "+pdn("u-jdoe")+" member="+udn("jdoe"),
			"delete-member "+pdn("u-jdoe")+" memberUid=jdoe",
			"delete-record upgrec:14",
		)
		if p.Guard.MembershipRemovals != 1 || p.Guard.OwnedMemberships != 1 {
			t.Errorf("guard: %+v", p.Guard)
		}
	})
	t.Run("an entry without a record is a conflict", func(t *testing.T) {
		p := build(t, c, world{
			ad:     []*model.ADObject{jdoe},
			upgs:   []*model.ADObject{mkUPG(20, "u-jdoe", jdoe.DN)},
			target: []*model.Entry{tUser(1, "jdoe"), uRec(1, "jdoe"), tUPG("u-jdoe", 5020)},
		}, both)
		wantOps(t, p)
		wantWarning(t, p, WarnConflict, pdn("u-jdoe"))
	})
	t.Run("AD returned no UPGs: the guard trips", func(t *testing.T) {
		p := build(t, c, world{
			ad:     []*model.ADObject{jdoe, mkGroup(10, "hpc", jdoe.DN)},
			target: []*model.Entry{tUser(1, "jdoe"), uRec(1, "jdoe"), tUPG("u-jdoe", 5020, "jdoe"), pRec(20, "u-jdoe", "jdoe")},
		}, both)
		if !p.Guard.Tripped || !strings.Contains(strings.Join(p.Guard.Reasons, "\n"), "AD returned no user private groups") {
			t.Errorf("guard: %+v", p.Guard)
		}
	})
	t.Run("a group moved between the sets is a conflict", func(t *testing.T) {
		p := build(t, c, world{
			ad:     []*model.ADObject{jdoe},
			upgs:   []*model.ADObject{mkUPG(10, "hpc", jdoe.DN)},
			target: []*model.Entry{tUser(1, "jdoe"), uRec(1, "jdoe"), tGroup(10, "hpc", "jdoe"), gRec(10, "hpc", "jdoe")},
		}, both)
		wantOps(t, p)
		wantWarning(t, p, WarnConflict, "moved between the shared and UPG sets; delete its record cn="+guid(10)+",ou=groups,"+state+" and move the entry to "+upgOU+" (or delete the old entry), then run dolly adopt")
	})
	t.Run("the same name in both sets", func(t *testing.T) {
		p := build(t, c, world{
			ad:     []*model.ADObject{jdoe, mkGroup(10, "x", jdoe.DN)},
			upgs:   []*model.ADObject{mkUPG(20, "x", jdoe.DN)},
			target: []*model.Entry{tUser(1, "jdoe"), uRec(1, "jdoe")},
		}, Options{Groups: true})
		if !hasOp(p, "add-entry "+gdn("x")) || !hasOp(p, "add-entry "+pdn("x")) {
			t.Errorf("each set has its own names: %v", opStrs(p))
		}
	})
	t.Run("gid_from_upg looks in the UPG set", func(t *testing.T) {
		c := cfg(t, upgCfg(config.UPGFromAD), func(c *config.Config) {
			c.Mapping.Users.Required = []string{"uid", "uidNumber"}
			c.Mapping.Users.GIDFromUPG = &config.GIDFromUPG{GroupName: "u-{{ .uid }}", When: config.GIDWhenMissing}
		})
		u := mkUser(1, "jdoe")
		delete(u.Attrs, "gidNumber")
		shared := mkGroup(10, "u-jdoe") // the shared set's group of that name is not used
		p := build(t, c, world{ad: []*model.ADObject{u, shared}, upgs: []*model.ADObject{mkUPG(20, "u-jdoe")}}, Options{Users: true})
		if !hasOp(p, "add-entry "+udn("jdoe")) || p.Counts.GIDFromUPG != 1 {
			t.Fatalf("%v", opStrs(p))
		}
		for _, a := range p.Ops[1].Attrs {
			if a.Name == "gidNumber" && a.Values[0] != "5020" {
				t.Errorf("gidNumber %v, want the UPG's 5020", a.Values)
			}
		}
	})
}

// Rule (UPG set, from: users): every AD user with a user entry gets a
// posixGroup in upg_base: cn = uid, gidNumber from the user, the user as
// member, keyed by the user's objectGUID (record in ou=upg). Only runs that
// sync users plan them.
func TestRuleUPGFromUsers(t *testing.T) {
	jdoe := mkUser(1, "jdoe")
	c := cfg(t, upgCfg(config.UPGFromUsers))
	have := []*model.Entry{tUser(1, "jdoe"), uRec(1, "jdoe"), tUPG("jdoe", 100, "jdoe"), pRec(1, "jdoe", "jdoe")}
	for _, tc := range []struct {
		name   string
		c      *config.Config
		ad     []*model.ADObject
		target []*model.Entry
		opt    Options
		want   []string
	}{
		{"created after the user", c, []*model.ADObject{jdoe}, nil, Options{Users: true}, []string{
			"add-record rec:1 seeAlso=" + udn("jdoe"),
			"add-entry " + udn("jdoe") + " loginShell=/bin/bash",
			"add-record upgrec:1 seeAlso=" + pdn("jdoe") + " roleOccupant=" + udn("jdoe"),
			"add-entry " + pdn("jdoe") + " member=" + udn("jdoe") + " memberUid=jdoe",
		}},
		{"in sync", c, []*model.ADObject{jdoe}, have, Options{Users: true}, nil},
		{"a groups-only run leaves them alone", c, nil, have, Options{Groups: true}, nil},
		// The group stays (with the placeholder, as groupOfNames needs a
		// member), and so does its record while the user's record does.
		{"user gone: only its owned membership goes", c, nil, have, Options{Users: true}, []string{
			"update-record rec:1 replace description=missing-since=2024-06-01T12:00:00Z",
			"add-placeholder " + pdn("jdoe") + " member=" + empty,
			"delete-member " + pdn("jdoe") + " member=" + udn("jdoe"),
			"delete-member " + pdn("jdoe") + " memberUid=jdoe",
			"update-record upgrec:1 delete roleOccupant=" + udn("jdoe"),
		}},
		{"user back: its member comes back", c, []*model.ADObject{jdoe},
			[]*model.Entry{tUser(1, "jdoe"), uRec(1, "jdoe", "missing-since=2024-05-01T00:00:00Z"), tUPG("jdoe", 100), pRec(1, "jdoe")}, Options{Users: true}, []string{
				"update-record rec:1 replace description=",
				"update-record upgrec:1 add roleOccupant=" + udn("jdoe"),
				"add-member " + pdn("jdoe") + " member=" + udn("jdoe"),
				"add-member " + pdn("jdoe") + " memberUid=jdoe",
			}},
		{"user pruned: the UPG record goes too", cfg(t, upgCfg(config.UPGFromUsers), func(c *config.Config) { c.Sync.PruneUsers = true }), nil,
			[]*model.Entry{tUser(1, "jdoe"), uRec(1, "jdoe", "missing-since=2024-01-01T00:00:00Z"), tUPG("jdoe", 100, "jdoe"), pRec(1, "jdoe", "jdoe")}, Options{Users: true}, []string{
				"add-placeholder " + pdn("jdoe") + " member=" + empty,
				"delete-member " + pdn("jdoe") + " member=" + udn("jdoe"),
				"delete-member " + pdn("jdoe") + " memberUid=jdoe",
				"update-record upgrec:1 delete roleOccupant=" + udn("jdoe"),
				"delete-entry " + udn("jdoe"),
				"delete-record rec:1",
				"delete-record upgrec:1",
			}},
		{"user record gone: the UPG record goes", c, nil, []*model.Entry{tUser(1, "jdoe"), tUPG("jdoe", 100, "jdoe"), pRec(1, "jdoe", "jdoe")}, Options{Users: true}, []string{
			"add-placeholder " + pdn("jdoe") + " member=" + empty,
			"delete-member " + pdn("jdoe") + " member=" + udn("jdoe"),
			"delete-member " + pdn("jdoe") + " memberUid=jdoe",
			"delete-record upgrec:1",
		}},
		{"gidNumber changed", c, []*model.ADObject{func() *model.ADObject {
			u := mkUser(1, "jdoe")
			u.Attrs["gidNumber"] = []string{"200"}
			return u
		}()}, have, Options{Users: true}, []string{
			"modify-entry " + udn("jdoe") + " replace gidNumber=200",
			"modify-entry " + pdn("jdoe") + " replace gidNumber=200",
		}},
		{"gid_number names another attribute", cfg(t, upgCfg(config.UPGFromUsers), func(c *config.Config) { c.Source.UPG.GIDNumber = "uidNumber" }),
			[]*model.ADObject{jdoe}, have, Options{Users: true}, []string{
				"modify-entry " + pdn("jdoe") + " replace gidNumber=1001",
			}},
		{"disabled with an invalid gid_number: the owned membership goes", empNumCfg(t), []*model.ADObject{func() *model.ADObject {
			u := mkUser(1, "jdoe")
			u.UserAccountControl = 514
			u.Attrs["employeeNumber"] = []string{"4294967295"}
			return u
		}()}, []*model.Entry{tUser(1, "jdoe"), uRec(1, "jdoe"), tUPG("jdoe", 7, "jdoe"), pRec(1, "jdoe", "jdoe")}, Options{Users: true}, []string{
			"update-record rec:1 replace description=saved-shell=/bin/bash",
			"modify-entry " + udn("jdoe") + " replace employeeNumber=4294967295; replace loginShell=/sbin/nologin",
			"add-placeholder " + pdn("jdoe") + " member=" + empty,
			"delete-member " + pdn("jdoe") + " member=" + udn("jdoe"),
			"delete-member " + pdn("jdoe") + " memberUid=jdoe",
			"update-record upgrec:1 delete roleOccupant=" + udn("jdoe"),
		}},
		{"a local member stays", c, []*model.ADObject{jdoe}, []*model.Entry{tUser(1, "jdoe"), uRec(1, "jdoe"), tUPG("jdoe", 100, "jdoe", "local1"), pRec(1, "jdoe", "jdoe")},
			Options{Users: true}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := build(t, tc.c, world{ad: tc.ad, target: tc.target}, tc.opt)
			wantOps(t, p, tc.want...)
			if strings.HasPrefix(tc.name, "disabled") && !strings.HasPrefix(p.Ops[3].Reason, "user is disabled") {
				t.Errorf("reason %q", p.Ops[3].Reason)
			}
		})
	}
}

// A uid rename renames the user's UPG: the user's rename fixes the member
// values in every group (its UPG too), then the UPG gets a modrdn to the
// new uid and its record follows, crash-safe like any group rename.
func TestRuleUPGFromUsersRename(t *testing.T) {
	p := build(t, cfg(t, upgCfg(config.UPGFromUsers)), world{
		ad:     []*model.ADObject{mkUser(1, "jdoe2")},
		target: []*model.Entry{tUser(1, "jdoe"), uRec(1, "jdoe"), tUPG("jdoe", 100, "jdoe"), pRec(1, "jdoe", "jdoe")},
	}, Options{Users: true})
	nu := udn("jdoe2")
	wantOps(t, p,
		"update-record rec:1 replace seeAlso="+nu+"; replace description=renaming-from="+udn("jdoe"),
		"modrdn "+udn("jdoe")+" -> uid=jdoe2",
		"rename-member "+pdn("jdoe")+" member "+udn("jdoe")+" -> "+nu,
		"rename-member "+pdn("jdoe")+" memberUid jdoe -> jdoe2",
		"update-record upgrec:1 delete roleOccupant="+udn("jdoe")+"; add roleOccupant="+nu,
		"update-record rec:1 replace description=",
		"modify-entry "+nu+" replace cn=jdoe2",
		"update-record upgrec:1 replace seeAlso="+pdn("jdoe2")+"; replace description=renaming-from="+pdn("jdoe"),
		"modrdn "+pdn("jdoe")+" -> cn=jdoe2",
		"update-record upgrec:1 replace description=",
	)
	if p.Counts.UPGsRenamed != 1 {
		t.Errorf("counts: %+v", p.Counts)
	}
}

// Without source.upg, UPG records are left alone, with one warning; and a
// --groups run with from: users makes no UPGs, so it lists no UPG warnings.
func TestUPGRecordsWithoutUPGSet(t *testing.T) {
	jdoe := mkUser(1, "jdoe")
	target := []*model.Entry{tUser(1, "jdoe"), uRec(1, "jdoe"), tUPG("jdoe", 100, "jdoe"), pRec(1, "jdoe", "jdoe"), pRec(20, "u-x", "jdoe")}
	// The UPG records are still read: the test world adds ou=upg only with a UPG set.
	target = append(target, &model.Entry{DN: "ou=upg," + state, Attrs: map[string][]string{"objectClass": {"organizationalUnit"}}})
	p := build(t, cfg(t), world{ad: []*model.ADObject{jdoe, mkGroup(10, "hpc")}, target: target}, both)
	for _, s := range opStrs(p) {
		if strings.Contains(s, "upgrec") || strings.Contains(s, upgOU) {
			t.Errorf("UPG op without a UPG set: %s", s)
		}
	}
	wantWarning(t, p, WarnInvalid, "2 user private group records exist under ou=upg but source.upg is not set; they are left alone")

	u := mkUser(2, "bob")
	u.Attrs["employeeNumber"] = []string{"x"}
	g := buildGroupsOnlyUPG(t, u)
	for _, w := range g.Warnings {
		if strings.Contains(w.Message, "no user private group") {
			t.Errorf("--groups run lists a UPG warning: %v", w)
		}
	}
}

func buildGroupsOnlyUPG(t *testing.T, u *model.ADObject) *Plan {
	t.Helper()
	return build(t, empNumCfg(t), world{ad: []*model.ADObject{u, mkGroup(10, "hpc", u.DN)}}, Options{Groups: true})
}

// empNumCfg takes the UPG gidNumber from employeeNumber, which no other
// rule checks.
func empNumCfg(t *testing.T) *config.Config {
	return cfg(t, upgCfg(config.UPGFromUsers), func(c *config.Config) {
		c.Mapping.Users.Attributes["employeeNumber"] = "employeeNumber"
		c.Source.UPG.GIDNumber = "employeeNumber"
	})
}

// A synced user whose gid_number value turns invalid keeps its UPG as it
// is (member included); only the warning appears.
func TestRuleUPGFromUsersGIDTurnsInvalid(t *testing.T) {
	u := mkUser(1, "jdoe")
	u.Attrs["employeeNumber"] = []string{"4294967295"}
	p := build(t, empNumCfg(t), world{ad: []*model.ADObject{u},
		target: []*model.Entry{tUser(1, "jdoe"), uRec(1, "jdoe"), tUPG("jdoe", 7, "jdoe"), pRec(1, "jdoe", "jdoe")}}, Options{Users: true})
	for _, s := range opStrs(p) {
		if strings.Contains(s, upgOU) || strings.Contains(s, "upgrec") {
			t.Errorf("UPG changed: %s", s)
		}
	}
	wantWarning(t, p, WarnInvalid, "no user private group: employeeNumber")
}

// UPGs made from users take part in the duplicate gidNumber check, also
// against the shared groups.
func TestUPGFromUsersDuplicateGID(t *testing.T) {
	a, b := mkUser(1, "a"), mkUser(2, "b") // both have gidNumber 100
	g := mkGroup(10, "staff", a.DN)
	g.Attrs["gidNumber"] = []string{"100"}
	p := build(t, cfg(t, upgCfg(config.UPGFromUsers)), world{ad: []*model.ADObject{a, b, g}}, Options{Users: true})
	wantWarning(t, p, WarnDuplicateID, "used by staff, user private group a, user private group b")
}

// A user whose gidNumber source is missing or invalid gets no UPG (the user
// entry is unaffected), with a warning.
func TestRuleUPGFromUsersBadGID(t *testing.T) {
	c := cfg(t, upgCfg(config.UPGFromUsers), func(c *config.Config) {
		c.Mapping.Users.Attributes["employeeNumber"] = "employeeNumber"
		c.Source.UPG.GIDNumber = "employeeNumber"
	})
	u := mkUser(1, "jdoe")
	u.Attrs["employeeNumber"] = []string{"12345678901"}
	p := build(t, c, world{ad: []*model.ADObject{u}, target: []*model.Entry{tUser(1, "jdoe"), uRec(1, "jdoe")}}, Options{Users: true})
	if hasOp(p, "add-entry "+pdn("jdoe")) || hasOp(p, "add-record upgrec") {
		t.Errorf("UPG planned: %v", opStrs(p))
	}
	wantWarning(t, p, WarnInvalid, "no user private group: employeeNumber")
}

// A failed user add skips the user's UPG, record and entry (its member's
// entry is missing; the next run creates it); a failed UPG record skips
// only the UPG entry.
func TestDepsUPGFromUsers(t *testing.T) {
	p := build(t, cfg(t, upgCfg(config.UPGFromUsers)), world{ad: []*model.ADObject{mkUser(1, "jdoe")}}, Options{Users: true})
	wantSkipped(t, p, "add-entry "+udn("jdoe"),
		"add-record upgrec:1 seeAlso="+pdn("jdoe")+" roleOccupant="+udn("jdoe"),
		"add-entry "+pdn("jdoe")+" member="+udn("jdoe")+" memberUid=jdoe")
	// A failed user rename skips the UPG rename too; the next run does both.
	r := build(t, cfg(t, upgCfg(config.UPGFromUsers)), world{
		ad:     []*model.ADObject{mkUser(1, "jdoe2")},
		target: []*model.Entry{tUser(1, "jdoe"), uRec(1, "jdoe"), tUPG("jdoe", 100, "jdoe"), pRec(1, "jdoe", "jdoe")},
	}, Options{Users: true})
	_, skipped := simulate(r, "modrdn "+udn("jdoe"))
	if !strings.Contains(strings.Join(skipped, "\n"), "modrdn "+pdn("jdoe")) {
		t.Errorf("UPG rename not skipped: %v", skipped)
	}
	wantSkipped(t, p, "add-record upgrec:1",
		"add-entry "+pdn("jdoe")+" member="+udn("jdoe")+" memberUid=jdoe")
}

// Adopt claims existing UPGs: with from: ad the AD UPG groups, with from:
// users the UPGs of the synced users. Members in the group become owned.
func TestAdoptUPGs(t *testing.T) {
	jdoe := mkUser(1, "jdoe")
	t.Run("from ad", func(t *testing.T) {
		p := build(t, cfg(t, upgCfg(config.UPGFromAD)), world{
			ad:     []*model.ADObject{jdoe},
			upgs:   []*model.ADObject{mkUPG(20, "u-jdoe", jdoe.DN)},
			target: []*model.Entry{tUser(1, "jdoe"), tUPG("u-jdoe", 5020, "jdoe", "local1")},
		}, Options{Adopt: true})
		wantOps(t, p,
			"add-record rec:1 seeAlso="+udn("jdoe"),
			"add-record upgrec:14 seeAlso="+pdn("u-jdoe")+" roleOccupant="+udn("jdoe"),
		)
	})
	t.Run("from users", func(t *testing.T) {
		p := build(t, cfg(t, upgCfg(config.UPGFromUsers)), world{
			ad:     []*model.ADObject{jdoe},
			target: []*model.Entry{tUser(1, "jdoe"), tUPG("jdoe", 100, "jdoe")},
		}, Options{Adopt: true})
		wantOps(t, p,
			"add-record rec:1 seeAlso="+udn("jdoe"),
			"add-record upgrec:1 seeAlso="+pdn("jdoe")+" roleOccupant="+udn("jdoe"),
		)
	})
}

// The dry-run output shows the UPG set separately.
func TestPrintUPGSet(t *testing.T) {
	jdoe := mkUser(1, "jdoe")
	p := build(t, cfg(t, upgCfg(config.UPGFromAD)), world{ad: []*model.ADObject{jdoe}, upgs: []*model.ADObject{mkUPG(20, "u-jdoe", jdoe.DN)}}, both)
	var b strings.Builder
	p.Verbose = true
	p.Print(&b)
	for _, want := range []string{"AD: 1 user private groups (source.upg)", "0 user private group entries", "\nUser private groups (1)\n", "  upgs:        +1 ~0 >0"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("missing %q in\n%s", want, b.String())
		}
	}
}

// Config: source.upg and target.upg_base come together; upg_base is a base
// of its own.
func TestUPGConfig(t *testing.T) {
	for _, tc := range []struct {
		name string
		mod  func(*config.Config)
		err  string
	}{
		{"from ad", upgCfg(config.UPGFromAD), ""},
		{"from users", upgCfg(config.UPGFromUsers), ""},
		{"upg_base alone", func(c *config.Config) { c.Target.UPGBase = upgOU }, "source.upg is missing"},
		{"source.upg alone", func(c *config.Config) { upgCfg(config.UPGFromUsers)(c); c.Target.UPGBase = "" }, "target.upg_base"},
		{"bad from", func(c *config.Config) { upgCfg(config.UPGFromUsers)(c); c.Source.UPG.From = "ldap" }, "source.upg.from"},
		{"ad needs a filter", func(c *config.Config) { upgCfg(config.UPGFromAD)(c); c.Source.UPG.Filter = "" }, "source.upg.filter"},
		{"users takes no base", func(c *config.Config) { upgCfg(config.UPGFromUsers)(c); c.Source.UPG.Base = adUPGs }, "not used with from: users"},
		{"gid_number not mapped", func(c *config.Config) { upgCfg(config.UPGFromUsers)(c); c.Source.UPG.GIDNumber = "foo" }, "source.upg.gid_number"},
		{"gid_number with ad", func(c *config.Config) { upgCfg(config.UPGFromAD)(c); c.Source.UPG.GIDNumber = "uidNumber" }, "only used with from: users"},
		{"same as groups_base", func(c *config.Config) { upgCfg(config.UPGFromUsers)(c); c.Target.UPGBase = groupsOU }, "upg_base and target.groups_base must differ"},
		{"inside users_base", func(c *config.Config) { upgCfg(config.UPGFromUsers)(c); c.Target.UPGBase = "ou=upg," + people }, "must not be inside each other"},
		{"inside state_base", func(c *config.Config) { upgCfg(config.UPGFromUsers)(c); c.Target.UPGBase = "ou=x," + state }, "must not be inside target.state_base"},
		{"state_base inside it", func(c *config.Config) {
			upgCfg(config.UPGFromUsers)(c)
			c.Target.UPGBase = "dc=local" // users_base and groups_base are inside it
		}, "must not be inside each other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := cfg(t)
			tc.mod(c)
			err := c.Validate()
			switch {
			case tc.err == "" && err != nil:
				t.Errorf("rejected: %v", err)
			case tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)):
				t.Errorf("err = %v, want %q", err, tc.err)
			}
		})
	}
}
