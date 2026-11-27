package planner

import (
	"bytes"
	"strings"
	"testing"

	"github.com/dirkpetersen/dolly/internal/config"
	"github.com/dirkpetersen/dolly/internal/model"
)

func onlyMembers(c *config.Config) { c.Source.Users.OnlyGroupMembers = true }

// Rule (only_group_members): a sync of users reads no users base; only the
// valid members of the shared groups get user entries, also from outside
// the users base. A user in the base but in no group, and a user that is
// only in a UPG, get none. The default still reads the users base.
func TestRuleOnlyGroupMembers(t *testing.T) {
	outside := mkUser(2, "outside")
	outside.DN, outside.InScope = "CN=outside,"+adOther, false
	world := func() world {
		return world{
			ad: []*model.ADObject{
				mkUser(1, "inbase"), outside, mkUser(3, "lonely"), mkUser(4, "upgonly"),
				mkGroup(10, "hpc", "CN=inbase,"+adPeople, "CN=outside,"+adOther),
			},
			upgs: []*model.ADObject{mkUPG(20, "u-upgonly", "CN=upgonly,"+adPeople)},
		}
	}
	added := func(p *Plan) []string {
		var out []string
		for _, op := range p.Ops {
			if op.Kind == AddEntry && op.Section == SectionUsers {
				out = append(out, model.RDNValue(op.DN))
			}
		}
		return out
	}
	for _, tc := range []struct {
		name      string
		only      bool
		wantRead  bool
		wantUsers string
	}{
		{"default reads the users base", false, true, "inbase lonely outside upgonly"},
		{"only group members", true, false, "inbase outside"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mods := []func(*config.Config){upgCfg(config.UPGFromAD)}
			if tc.only {
				mods = append(mods, onlyMembers)
			}
			c := cfg(t, mods...)
			if got := both.ReadADUsers(c); got != tc.wantRead {
				t.Errorf("ReadADUsers = %v, want %v", got, tc.wantRead)
			}
			p := build(t, c, world(), both)
			if got := strings.Join(added(p), " "); got != tc.wantUsers {
				t.Errorf("user entries = %q, want %q\n%v", got, tc.wantUsers, opStrs(p))
			}
			if p.ADUsersRead != tc.wantRead || p.Guard.OnlyGroupMembers != tc.only {
				t.Errorf("ADUsersRead = %v, OnlyGroupMembers = %v", p.ADUsersRead, p.Guard.OnlyGroupMembers)
			}
			var b bytes.Buffer
			p.Print(&b)
			line := "Users: only group members (2 found by DN lookup)"
			if strings.Contains(b.String(), line) != tc.only {
				t.Errorf("summary line %q present = %v, want %v\n%s", line, !tc.only, tc.only, b.String())
			}
		})
	}
	// adopt ignores the setting: it still claims the users of the users base.
	if !(Options{Adopt: true}).ReadADUsers(cfg(t, onlyMembers)) {
		t.Error("adopt must read the users base")
	}
	// a groups-only run reads no users base either way.
	if (Options{Groups: true}).ReadADUsers(cfg(t)) {
		t.Error("a groups-only run must not read the users base")
	}
}

// A managed user that is no longer a member loses its owned memberships and
// starts the prune clock; only the existing prune rules delete the entry.
func TestRuleOnlyGroupMembersFormerMember(t *testing.T) {
	c := cfg(t, onlyMembers)
	w := world{
		ad:     []*model.ADObject{mkUser(1, "stay"), mkUser(2, "gone"), mkGroup(10, "hpc", "CN=stay,"+adPeople)},
		target: []*model.Entry{tUser(1, "stay"), uRec(1, "stay"), tUser(2, "gone"), uRec(2, "gone"), tGroup(10, "hpc", "stay", "gone"), gRec(10, "hpc", "stay", "gone")},
	}
	p := build(t, c, w, both)
	if hasOp(p, "delete-entry "+udn("gone")) {
		t.Errorf("entry deleted without prune_users: %v", opStrs(p))
	}
	if !hasOp(p, "delete-member "+gdn("hpc")+" member="+udn("gone")) {
		t.Errorf("owned membership kept: %v", opStrs(p))
	}
	if p.Counts.UsersMissing != 1 {
		t.Errorf("UsersMissing = %d, want 1\n%v", p.Counts.UsersMissing, opStrs(p))
	}
}

// Guard (only_group_members): the zero-users check counts the users found
// through the groups, not the users base.
func TestGuardOnlyGroupMembers(t *testing.T) {
	for _, tc := range []struct {
		name    string
		ad      []*model.ADObject
		tripped bool
		reason  string
	}{
		{"no groups", []*model.ADObject{mkUser(1, "jdoe")}, true, "AD returned no groups"},
		{"groups without valid members", []*model.ADObject{mkUser(1, "jdoe"), mkGroup(10, "hpc")}, true, "no member of the synced groups can get a user entry"},
		{"members exist", []*model.ADObject{mkUser(1, "jdoe"), mkGroup(10, "hpc", "CN=jdoe,"+adPeople)}, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := build(t, cfg(t, onlyMembers), world{ad: tc.ad}, both)
			if p.Guard.Tripped != tc.tripped {
				t.Fatalf("tripped = %v, want %v (%v)", p.Guard.Tripped, tc.tripped, p.Guard.Reasons)
			}
			if tc.reason != "" && !strings.Contains(strings.Join(p.Guard.Reasons, "; "), tc.reason) {
				t.Errorf("reasons %v, want %q", p.Guard.Reasons, tc.reason)
			}
		})
	}
}
