package planner

import (
	"math/rand"
	"strconv"
	"strings"
	"testing"

	"github.com/dirkpetersen/dolly/internal/config"
	"github.com/dirkpetersen/dolly/internal/model"
)

// One test per rule in README "Ownership records" (and CLAUDE.md hard
// requirement 2). Record names in expectations: rec:1 is guid(1), rec:a is
// guid(10). Users are guid 1-9, groups guid 10 and up.

// Rule: a user in the AD group but not in the target group is added and
// recorded as Dolly-owned, record first.
func TestRuleAddMemberRecordedAsOwned(t *testing.T) {
	jdoe := mkUser(1, "jdoe")
	p := build(t, cfg(t), world{
		ad:     []*model.ADObject{jdoe, mkGroup(10, "hpc", jdoe.DN)},
		target: []*model.Entry{tUser(1, "jdoe"), uRec(1, "jdoe"), tGroup(10, "hpc", "local1"), gRec(10, "hpc")},
	}, both)
	wantOps(t, p,
		"update-record rec:a add roleOccupant="+udn("jdoe"),
		"add-member "+gdn("hpc")+" member="+udn("jdoe"),
		"add-member "+gdn("hpc")+" memberUid=jdoe",
	)
}

// Rule: a Dolly-owned member who left the AD group, or was deleted from AD,
// is removed from the target group and from the record (values first).
func TestRuleRemoveOwnedMember(t *testing.T) {
	jdoe, bob := mkUser(1, "jdoe"), mkUser(2, "bob")
	target := []*model.Entry{tUser(1, "jdoe"), uRec(1, "jdoe"), tUser(2, "bob"), uRec(2, "bob"),
		tGroup(10, "hpc", "bob", "jdoe"), gRec(10, "hpc", "bob", "jdoe")}
	removal := []string{
		"delete-member " + gdn("hpc") + " member=" + udn("jdoe"),
		"delete-member " + gdn("hpc") + " memberUid=jdoe",
		"update-record rec:a delete roleOccupant=" + udn("jdoe"),
	}
	t.Run("left the group", func(t *testing.T) {
		p := build(t, cfg(t), world{ad: []*model.ADObject{jdoe, bob, mkGroup(10, "hpc", bob.DN)}, target: target}, both)
		wantOps(t, p, removal...)
	})
	t.Run("deleted from AD", func(t *testing.T) {
		p := build(t, cfg(t), world{ad: []*model.ADObject{bob, mkGroup(10, "hpc", bob.DN)}, target: target}, both)
		wantOps(t, p, append([]string{"update-record rec:1 replace description=missing-since=2024-06-01T12:00:00Z"}, removal...)...)
	})
}

// Rule: a member already in the target group without a record is local and
// is never removed, even if the same user is also in the AD group. It is
// not claimed either.
func TestRuleLocalMemberNeverRemoved(t *testing.T) {
	jdoe := mkUser(1, "jdoe")
	p := build(t, cfg(t), world{
		ad:     []*model.ADObject{jdoe, mkGroup(10, "hpc", jdoe.DN)},
		target: []*model.Entry{tUser(1, "jdoe"), uRec(1, "jdoe"), tGroup(10, "hpc", "jdoe", "local1"), gRec(10, "hpc")},
	}, both)
	wantOps(t, p)
	t.Run("local member also gone from AD group", func(t *testing.T) {
		p := build(t, cfg(t), world{
			ad:     []*model.ADObject{jdoe, mkGroup(10, "hpc")},
			target: []*model.Entry{tUser(1, "jdoe"), uRec(1, "jdoe"), tGroup(10, "hpc", "jdoe", "local1"), gRec(10, "hpc")},
		}, both)
		wantOps(t, p)
	})
}

// Rule: a member value removed by hand on the target is re-added only if
// Dolly owns the membership (a roleOccupant names it) and AD still lists the
// user: AD wins for Dolly-owned members. A local member removed by hand is
// never Dolly's business and isn't restored.
func TestRuleManualRemoval(t *testing.T) {
	jdoe := mkUser(1, "jdoe")
	ad := []*model.ADObject{jdoe, mkGroup(10, "hpc", jdoe.DN)}
	users := []*model.Entry{tUser(1, "jdoe"), uRec(1, "jdoe"), localUID(udn("local1"), "local1")}
	t.Run("owned member removed by hand is re-added", func(t *testing.T) {
		p := build(t, cfg(t), world{ad: ad, target: append(users, tGroup(10, "hpc", "local1"), gRec(10, "hpc", "jdoe"))}, both)
		wantOps(t, p,
			"add-member "+gdn("hpc")+" member="+udn("jdoe"),
			"add-member "+gdn("hpc")+" memberUid=jdoe")
	})
	t.Run("owned memberUid value removed by hand is re-added", func(t *testing.T) {
		g := tGroup(10, "hpc", "jdoe", "local1")
		g.DeleteValue("memberUid", "jdoe", strings.EqualFold)
		p := build(t, cfg(t), world{ad: ad, target: append(users, g, gRec(10, "hpc", "jdoe"))}, both)
		wantOps(t, p, "add-member "+gdn("hpc")+" memberUid=jdoe")
	})
	t.Run("local member removed by hand is not restored", func(t *testing.T) {
		// local1 was a local member (no roleOccupant) and is no longer in
		// the group; its user entry still exists. Nothing is written.
		p := build(t, cfg(t), world{ad: ad, target: append(users, tGroup(10, "hpc", "jdoe"), gRec(10, "hpc", "jdoe"))}, both)
		wantOps(t, p)
	})
	t.Run("local member also in the AD group comes back as owned", func(t *testing.T) {
		// Known consequence: jdoe was a local member of hpc and is also in
		// the AD group. Once removed by hand, the next run can't tell this
		// from a new AD membership, so it adds jdoe as Dolly-owned.
		p := build(t, cfg(t), world{ad: ad, target: append(users, tGroup(10, "hpc", "local1"), gRec(10, "hpc"))}, both)
		wantOps(t, p,
			"update-record rec:a add roleOccupant="+udn("jdoe"),
			"add-member "+gdn("hpc")+" member="+udn("jdoe"),
			"add-member "+gdn("hpc")+" memberUid=jdoe")
	})
}

// Rule: a target user or group with the same name as an AD entry but no
// ownership record is a conflict: logged and skipped.
func TestRuleConflictWithoutRecord(t *testing.T) {
	jdoe := mkUser(1, "jdoe")
	p := build(t, cfg(t), world{
		ad:     []*model.ADObject{jdoe, mkGroup(10, "hpc", jdoe.DN)},
		target: []*model.Entry{tUser(1, "jdoe"), tGroup(10, "hpc")},
	}, both)
	wantOps(t, p)
	wantWarning(t, p, WarnConflict, udn("jdoe")+": entry exists on the target without an ownership record")
	wantWarning(t, p, WarnConflict, gdn("hpc")+": group exists on the target without an ownership record")
}

// Rule: a user renamed in AD (same objectGUID) is renamed in place, and its
// member and memberUid values are updated in every group, local ones
// included, as are roleOccupant values. A case-only change is a rename.
func TestRuleRenameByGUID(t *testing.T) {
	for _, tc := range []struct{ name, newUID string }{{"new name", "jdoe2"}, {"case only", "JDoe"}} {
		t.Run(tc.name, func(t *testing.T) {
			u := mkUser(1, tc.newUID)
			p := build(t, cfg(t), world{
				ad: []*model.ADObject{u, mkGroup(10, "hpc", u.DN)},
				target: []*model.Entry{tUser(1, "jdoe"), uRec(1, "jdoe"), tGroup(10, "hpc", "jdoe"), gRec(10, "hpc", "jdoe"),
					tGroup(11, "lab", "jdoe")}, // lab is a local group, jdoe a local member
			}, both)
			nu := udn(tc.newUID)
			wantOps(t, p,
				"update-record rec:1 replace seeAlso="+nu+"; replace description=renaming-from="+udn("jdoe"),
				"modrdn "+udn("jdoe")+" -> uid="+tc.newUID,
				"rename-member "+gdn("hpc")+" member "+udn("jdoe")+" -> "+nu,
				"rename-member "+gdn("hpc")+" memberUid jdoe -> "+tc.newUID,
				"rename-member "+gdn("lab")+" member "+udn("jdoe")+" -> "+nu,
				"rename-member "+gdn("lab")+" memberUid jdoe -> "+tc.newUID,
				"update-record rec:a delete roleOccupant="+udn("jdoe")+"; add roleOccupant="+nu,
				"update-record rec:1 replace description=",
				"modify-entry "+nu+" replace cn="+tc.newUID,
			)
		})
	}
	t.Run("group renamed", func(t *testing.T) {
		g := mkGroup(10, "hpc-new")
		p := build(t, cfg(t, uidOnly), world{ad: []*model.ADObject{mkUser(1, "x"), g},
			target: []*model.Entry{tGroupUID(10, "hpc"), gRec(10, "hpc")}}, Options{Groups: true})
		wantOps(t, p,
			"update-record rec:a replace seeAlso="+gdn("hpc-new")+"; replace description=renaming-from="+gdn("hpc"),
			"modrdn "+gdn("hpc")+" -> cn=hpc-new",
			"update-record rec:a replace description=",
		) // modrdn already replaced cn, the RDN attribute
	})
}

// A rename onto a name that already exists is a conflict: logged, skipped.
func TestRuleRenameCollisionIsConflict(t *testing.T) {
	u := mkUser(1, "bob")
	p := build(t, cfg(t), world{
		ad:     []*model.ADObject{u},
		target: []*model.Entry{tUser(1, "jdoe"), uRec(1, "jdoe"), tUser(2, "bob")},
	}, Options{Users: true})
	wantOps(t, p)
	wantWarning(t, p, WarnConflict, "was renamed, but "+udn("bob")+" already exists")
}

// Rule: a user gone from AD (deleted, out of scope, or missing the attribute
// its uid is mapped from) loses all Dolly-owned memberships right away; the
// entry stays. (A user missing only uidNumber or gidNumber stays a member:
// see TestRuleRelaxedMemberRequirements.)
func TestRuleGoneUserLosesOwnedMemberships(t *testing.T) {
	bob := mkUser(2, "bob")
	cases := map[string]func() []*model.ADObject{
		"deleted": func() []*model.ADObject { return nil },
		// Moved out of the users base and no longer in a synced group: AD
		// still returns it (through a group Dolly doesn't sync, here one
		// missing gidNumber), but it isn't a managed user any more.
		"out of scope": func() []*model.ADObject {
			u := mkUser(1, "jdoe")
			u.DN, u.InScope = "CN=jdoe,"+adOther, false
			unsynced := mkGroup(12, "unsynced", u.DN)
			delete(unsynced.Attrs, "gidNumber")
			return []*model.ADObject{u, unsynced}
		},
		"missing uid": func() []*model.ADObject {
			u := mkUser(1, "jdoe")
			delete(u.Attrs, "uid")
			return []*model.ADObject{u}
		},
	}
	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			hpc := mkGroup(10, "hpc", bob.DN)
			ad := []*model.ADObject{bob, hpc}
			for _, o := range extra() {
				ad = append(ad, o)
				if name == "missing uid" {
					hpc.Members = append(hpc.Members, o.DN)
				}
			}
			p := build(t, cfg(t), world{ad: ad, target: []*model.Entry{
				tUser(1, "jdoe"), uRec(1, "jdoe"), tUser(2, "bob"), uRec(2, "bob"),
				tGroup(10, "hpc", "bob", "jdoe"), tGroup(11, "lab", "jdoe"), gRec(10, "hpc", "bob", "jdoe")}}, both)
			wantOps(t, p,
				"update-record rec:1 replace description=missing-since=2024-06-01T12:00:00Z",
				"delete-member "+gdn("hpc")+" member="+udn("jdoe"),
				"delete-member "+gdn("hpc")+" memberUid=jdoe",
				"update-record rec:a delete roleOccupant="+udn("jdoe"),
			)
			if name == "out of scope" && p.Counts.Followed != 1 {
				t.Errorf("the out-of-scope user must still be read from AD; followed = %d", p.Counts.Followed)
			}
		})
	}
}

// Rule: the user entry is deleted only with prune_users and after
// prune_after_days. Pruning also removes local memberships, so no group
// points at a missing user. Order: memberships, entry, record.
func TestRulePrune(t *testing.T) {
	bob := mkUser(2, "bob")
	target := func(since string) []*model.Entry {
		return []*model.Entry{tUser(1, "jdoe"), uRec(1, "jdoe", "missing-since="+since), tUser(2, "bob"), uRec(2, "bob"),
			tGroup(10, "hpc", "bob", "jdoe"), gRec(10, "hpc", "bob", "jdoe"), tGroup(11, "lab", "jdoe")}
	}
	ad := []*model.ADObject{bob, mkGroup(10, "hpc", bob.DN)}
	prune := func(c *config.Config) { c.Sync.PruneUsers = true; c.Sync.PruneAfterDays = 30 }

	p := build(t, cfg(t, prune), world{ad: ad, target: target("2024-04-01T00:00:00Z")}, both)
	wantOps(t, p,
		"delete-member "+gdn("hpc")+" member="+udn("jdoe"),
		"delete-member "+gdn("hpc")+" memberUid=jdoe",
		"update-record rec:a delete roleOccupant="+udn("jdoe"),
		"add-placeholder "+gdn("lab")+" member="+empty,
		"delete-member "+gdn("lab")+" member="+udn("jdoe"),
		"delete-member "+gdn("lab")+" memberUid=jdoe",
		"delete-entry "+udn("jdoe"),
		"delete-record rec:1",
	)
	// The local membership in lab is removed too, but it is counted apart
	// and not against the guard.
	if p.Guard.UserRemovals != 1 || p.Guard.MembershipRemovals != 1 || p.Guard.LocalRemovals != 1 {
		t.Errorf("guard counts: %+v", p.Guard)
	}

	t.Run("not yet due", func(t *testing.T) {
		p := build(t, cfg(t, prune), world{ad: ad, target: target("2024-05-15T00:00:00Z")}, both)
		if hasOp(p, "delete-entry") || hasOp(p, "delete-record") {
			t.Errorf("pruned too early:\n%s", strings.Join(opStrs(p), "\n"))
		}
	})
	t.Run("prune_users off", func(t *testing.T) {
		p := build(t, cfg(t), world{ad: ad, target: target("2020-01-01")}, both)
		if hasOp(p, "delete-entry") || hasOp(p, "delete-member "+gdn("lab")) {
			t.Errorf("pruned with prune_users off:\n%s", strings.Join(opStrs(p), "\n"))
		}
	})
}

// Rule: a group deleted from AD loses its Dolly-owned members and its
// ownership record. The group itself and its local members stay.
func TestRuleGroupGoneFromAD(t *testing.T) {
	jdoe := mkUser(1, "jdoe")
	p := build(t, cfg(t), world{
		ad:     []*model.ADObject{jdoe, mkGroup(11, "other", jdoe.DN)},
		target: []*model.Entry{tUser(1, "jdoe"), uRec(1, "jdoe"), tGroup(10, "hpc", "jdoe", "local1"), gRec(10, "hpc", "jdoe"), tGroup(11, "other", "jdoe"), gRec(11, "other", "jdoe")},
	}, both)
	wantOps(t, p,
		"delete-member "+gdn("hpc")+" member="+udn("jdoe"),
		"delete-member "+gdn("hpc")+" memberUid=jdoe",
		"delete-record rec:a",
	)
}

// Rule: roleOccupant always names members by DN (<rdn>=<uid>,<users_base>),
// also with memberUid only, where the user entry must exist under
// users_base with that uid, but not necessarily at that DN.
func TestRuleOccupantIsDNWithMemberUIDOnly(t *testing.T) {
	jdoe := mkUser(1, "jdoe")
	p := build(t, cfg(t, uidOnly), world{
		ad:     []*model.ADObject{jdoe, mkGroup(10, "hpc", jdoe.DN)},
		target: []*model.Entry{localUID("uid=jdoe,ou=staff,"+people, "jdoe"), tGroupUID(10, "hpc", "local1"), gRec(10, "hpc")},
	}, Options{Groups: true})
	wantOps(t, p,
		"update-record rec:a add roleOccupant="+udn("jdoe"),
		"add-member "+gdn("hpc")+" memberUid=jdoe",
	)
	t.Run("removal derives memberUid from the DN", func(t *testing.T) {
		p := build(t, cfg(t, uidOnly), world{
			ad:     []*model.ADObject{jdoe, mkGroup(10, "hpc")},
			target: []*model.Entry{tGroupUID(10, "hpc", "jdoe"), gRec(10, "hpc", "jdoe")},
		}, Options{Groups: true})
		wantOps(t, p,
			"delete-member "+gdn("hpc")+" memberUid=jdoe",
			"update-record rec:a delete roleOccupant="+udn("jdoe"),
		)
	})
}

// Rule: with member (groupOfNames) a new AD group without resolvable
// members isn't created; with memberUid only it is created right away.
func TestRuleEmptyGroupCreation(t *testing.T) {
	ad := []*model.ADObject{mkUser(1, "jdoe"), mkGroup(10, "hpc", "CN=ws01,OU=Computers,DC=example,DC=edu")}
	p := build(t, cfg(t), world{ad: ad}, Options{Groups: true})
	wantOps(t, p)
	wantWarning(t, p, WarnPending, gdn("hpc"))

	p = build(t, cfg(t, uidOnly), world{ad: ad}, Options{Groups: true})
	wantOps(t, p,
		"add-record rec:a seeAlso="+gdn("hpc"),
		"add-entry "+gdn("hpc"),
	)
}

// Rule: with member, removing the last member adds the empty_group_member
// placeholder first; it is removed once a real member exists again.
func TestRulePlaceholder(t *testing.T) {
	jdoe := mkUser(1, "jdoe")
	t.Run("added before the last member goes", func(t *testing.T) {
		p := build(t, cfg(t), world{
			ad:     []*model.ADObject{jdoe, mkGroup(10, "hpc")},
			target: []*model.Entry{tUser(1, "jdoe"), uRec(1, "jdoe"), tGroup(10, "hpc", "jdoe"), gRec(10, "hpc", "jdoe")},
		}, both)
		wantOps(t, p,
			"add-placeholder "+gdn("hpc")+" member="+empty,
			"delete-member "+gdn("hpc")+" member="+udn("jdoe"),
			"delete-member "+gdn("hpc")+" memberUid=jdoe",
			"update-record rec:a delete roleOccupant="+udn("jdoe"),
		)
	})
	t.Run("removed once a real member exists", func(t *testing.T) {
		g := tGroup(10, "hpc")
		g.Set("member", []string{empty})
		p := build(t, cfg(t), world{
			ad:     []*model.ADObject{jdoe, mkGroup(10, "hpc", jdoe.DN)},
			target: []*model.Entry{tUser(1, "jdoe"), uRec(1, "jdoe"), g, gRec(10, "hpc")},
		}, both)
		wantOps(t, p,
			"update-record rec:a add roleOccupant="+udn("jdoe"),
			"add-member "+gdn("hpc")+" member="+udn("jdoe"),
			"add-member "+gdn("hpc")+" memberUid=jdoe",
			"delete-placeholder "+gdn("hpc")+" member="+empty,
		)
	})
	// A placeholder next to a leaving owned member stays put: no delete,
	// add, delete churn.
	t.Run("kept when the last real member leaves", func(t *testing.T) {
		g := tGroup(10, "hpc", "jdoe")
		g.AddValue("member", empty)
		p := build(t, cfg(t), world{
			ad:     []*model.ADObject{jdoe, mkGroup(10, "hpc")},
			target: []*model.Entry{tUser(1, "jdoe"), uRec(1, "jdoe"), g, gRec(10, "hpc", "jdoe")},
		}, both)
		wantOps(t, p,
			"delete-member "+gdn("hpc")+" member="+udn("jdoe"),
			"delete-member "+gdn("hpc")+" memberUid=jdoe",
			"update-record rec:a delete roleOccupant="+udn("jdoe"),
		)
	})
	t.Run("never with memberUid only", func(t *testing.T) {
		p := build(t, cfg(t, uidOnly), world{
			ad:     []*model.ADObject{jdoe, mkGroup(10, "hpc")},
			target: []*model.Entry{tGroupUID(10, "hpc", "jdoe"), gRec(10, "hpc", "jdoe")},
		}, Options{Groups: true})
		if hasOp(p, "add-placeholder") {
			t.Error("placeholder with memberUid only")
		}
	})
}

// Rule: a disabled account keeps its entry, loses its Dolly-owned
// memberships (local ones stay), and gets disabled_shell. The old shell is
// saved in the record first and restored when the account is re-enabled.
func TestRuleDisabledAccount(t *testing.T) {
	jdoe, bob := mkUser(1, "jdoe"), mkUser(2, "bob")
	t.Run("disable", func(t *testing.T) {
		jdoe.UserAccountControl = 514
		defer func() { jdoe.UserAccountControl = 512 }()
		p := build(t, cfg(t), world{
			ad: []*model.ADObject{jdoe, bob, mkGroup(10, "hpc", jdoe.DN, bob.DN)},
			target: []*model.Entry{tUser(1, "jdoe"), uRec(1, "jdoe"), tUser(2, "bob"), uRec(2, "bob"),
				tGroup(10, "hpc", "bob", "jdoe"), gRec(10, "hpc", "bob", "jdoe"), tGroup(11, "lab", "jdoe")},
		}, both)
		wantOps(t, p,
			"update-record rec:1 replace description=saved-shell=/bin/bash",
			"modify-entry "+udn("jdoe")+" replace loginShell=/sbin/nologin",
			"delete-member "+gdn("hpc")+" member="+udn("jdoe"),
			"delete-member "+gdn("hpc")+" memberUid=jdoe",
			"update-record rec:a delete roleOccupant="+udn("jdoe"),
		)
	})
	disabledTarget := func(shell string) []*model.Entry {
		u := tUser(1, "jdoe")
		u.Set("loginShell", []string{shell})
		return []*model.Entry{u, uRec(1, "jdoe", "saved-shell=/bin/zsh"), tGroup(10, "hpc"), gRec(10, "hpc")}
	}
	t.Run("re-enable restores the shell", func(t *testing.T) {
		p := build(t, cfg(t), world{ad: []*model.ADObject{jdoe, mkGroup(10, "hpc", jdoe.DN)}, target: disabledTarget("/sbin/nologin")}, both)
		wantOps(t, p,
			"modify-entry "+udn("jdoe")+" replace loginShell=/bin/zsh",
			"update-record rec:1 replace description=",
			"update-record rec:a add roleOccupant="+udn("jdoe"),
			"add-member "+gdn("hpc")+" member="+udn("jdoe"),
			"add-member "+gdn("hpc")+" memberUid=jdoe",
		)
	})
	t.Run("re-enable keeps a shell an admin changed", func(t *testing.T) {
		p := build(t, cfg(t), world{ad: []*model.ADObject{jdoe}, target: disabledTarget("/bin/tcsh")}, Options{Users: true})
		wantOps(t, p, "update-record rec:1 replace description=")
	})
	t.Run("new disabled user", func(t *testing.T) {
		u := mkUser(3, "new")
		u.UserAccountControl = 514
		p := build(t, cfg(t), world{ad: []*model.ADObject{u}}, Options{Users: true})
		wantOps(t, p,
			"add-record rec:3 seeAlso="+udn("new")+" description=saved-shell=/bin/bash",
			"add-entry "+udn("new")+" loginShell=/sbin/nologin",
		)
	})
	t.Run("empty disabled_shell leaves the shell alone", func(t *testing.T) {
		jdoe.UserAccountControl = 514
		defer func() { jdoe.UserAccountControl = 512 }()
		p := build(t, cfg(t, func(c *config.Config) { c.Sync.DisabledShell = "" }), world{
			ad: []*model.ADObject{jdoe}, target: []*model.Entry{tUser(1, "jdoe"), uRec(1, "jdoe")}}, Options{Users: true})
		wantOps(t, p)
	})
}

// Rule: create_only attributes are written at creation and never again;
// other mapped attributes follow AD.
func TestRuleCreateOnly(t *testing.T) {
	u := mkUser(1, "jdoe")
	u.Attrs["loginShell"] = []string{"/bin/bash"}
	u.Attrs["unixHomeDirectory"] = []string{"/home/jdoe"}
	u.Attrs["gecos"] = []string{"Jane Doe"}
	e := tUser(1, "jdoe")
	e.Set("loginShell", []string{"/bin/ksh"})
	e.Set("homeDirectory", []string{"/data/jdoe"})
	p := build(t, cfg(t), world{ad: []*model.ADObject{u}, target: []*model.Entry{e, uRec(1, "jdoe")}}, Options{Users: true})
	wantOps(t, p, "modify-entry "+udn("jdoe")+" replace gecos=Jane Doe")
}

// Rule: the AD primary group (primaryGroupID) is ignored; only the group's
// member attribute counts.
//
// This test is intentionally structural: there is no code path that reads
// primaryGroupID or primaryGroupToken, so it only pins down that setting
// them changes nothing (the group stays empty).
func TestRulePrimaryGroupIgnored(t *testing.T) {
	u := mkUser(1, "jdoe")
	u.Attrs["primaryGroupID"] = []string{"513"}
	g := mkGroup(10, "domain-users")
	g.Attrs["primaryGroupToken"] = []string{"513"}
	p := build(t, cfg(t, uidOnly), world{ad: []*model.ADObject{u, g}}, Options{Groups: true})
	wantOps(t, p,
		"add-record rec:a seeAlso="+gdn("domain-users"),
		"add-entry "+gdn("domain-users"),
	)
}

// Rule: users need uid, uidNumber, and gidNumber, groups need name and
// gidNumber. Anything without them is ignored everywhere, listed once, and
// an ignored child group isn't flattened into its parent.
func TestRuleRequiredAttributes(t *testing.T) {
	jdoe, bob := mkUser(1, "jdoe"), mkUser(2, "bob")
	nogid := mkUser(3, "nogid")
	delete(nogid.Attrs, "gidNumber")
	extNoNum := &model.ADObject{GUID: guid(4), DN: "CN=ext," + adOther, Kind: model.KindUser, Attrs: map[string][]string{"uid": {"ext"}, "gidNumber": {"1"}}}
	child := mkGroup(11, "child", bob.DN)
	delete(child.Attrs, "gidNumber")
	parent := mkGroup(10, "parent", jdoe.DN, nogid.DN, extNoNum.DN, child.DN)
	p := build(t, cfg(t), world{ad: []*model.ADObject{jdoe, bob, nogid, extNoNum, child, parent}}, both)
	wantOps(t, p,
		"add-record rec:2 seeAlso="+udn("bob"),
		"add-entry "+udn("bob")+" loginShell=/bin/bash",
		"add-record rec:1 seeAlso="+udn("jdoe"),
		"add-entry "+udn("jdoe")+" loginShell=/bin/bash",
		"add-record rec:a seeAlso="+gdn("parent")+" roleOccupant="+udn("jdoe"),
		"add-entry "+gdn("parent")+" member="+udn("jdoe")+" memberUid=jdoe",
	)
	for _, s := range []string{"CN=nogid," + adPeople + ": user missing required attribute gidNumber",
		"CN=ext," + adOther + ": user missing required attribute uidNumber",
		"CN=child," + adGroups + ": group missing required attribute gidNumber"} {
		wantWarning(t, p, WarnIgnored, s)
	}
	if p.Counts.IgnoredEntries != 3 {
		t.Errorf("ignored = %d, want 3 (each listed once)", p.Counts.IgnoredEntries)
	}
}

// Rule: members outside the search bases with the required attributes are
// followed: users become managed users, child groups are flattened but not
// created. Non-user, non-group members are skipped.
func TestRuleOutOfScopeMembers(t *testing.T) {
	jdoe := mkUser(1, "jdoe")
	ext := mkUser(2, "ext")
	ext.DN, ext.InScope = "CN=ext,"+adOther, false
	ext2 := mkUser(3, "ext2")
	ext2.DN, ext2.InScope = "CN=ext2,"+adOther, false
	extGrp := mkGroup(11, "extgrp", ext2.DN)
	extGrp.DN, extGrp.InScope = "CN=extgrp,"+adOther, false
	ws := &model.ADObject{GUID: guid(99), DN: "CN=ws01,OU=Computers,DC=example,DC=edu", Kind: model.KindOther}
	hpc := mkGroup(10, "hpc", jdoe.DN, ext.DN, extGrp.DN, ws.DN)
	p := build(t, cfg(t), world{ad: []*model.ADObject{jdoe, ext, ext2, extGrp, ws, hpc}}, both)
	wantOps(t, p,
		"add-record rec:2 seeAlso="+udn("ext"),
		"add-entry "+udn("ext")+" loginShell=/bin/bash",
		"add-record rec:3 seeAlso="+udn("ext2"),
		"add-entry "+udn("ext2")+" loginShell=/bin/bash",
		"add-record rec:1 seeAlso="+udn("jdoe"),
		"add-entry "+udn("jdoe")+" loginShell=/bin/bash",
		"add-record rec:a seeAlso="+gdn("hpc")+" roleOccupant="+udn("ext")+"|"+udn("ext2")+"|"+udn("jdoe"),
		"add-entry "+gdn("hpc")+" member="+udn("ext")+"|"+udn("ext2")+"|"+udn("jdoe")+" memberUid=ext|ext2|jdoe",
	)
	if p.Counts.SkippedMembers != 1 || p.Counts.Followed != 4 {
		t.Errorf("skipped=%d followed=%d, want 1 and 4", p.Counts.SkippedMembers, p.Counts.Followed)
	}
}

// Rule: the spelling of uid is kept exactly as in AD.
func TestRuleUIDCasePreserved(t *testing.T) {
	u := mkUser(1, "JDoe")
	p := build(t, cfg(t), world{ad: []*model.ADObject{u, mkGroup(10, "hpc", u.DN)}}, both)
	wantOps(t, p,
		"add-record rec:1 seeAlso="+udn("JDoe"),
		"add-entry "+udn("JDoe")+" loginShell=/bin/bash",
		"add-record rec:a seeAlso="+gdn("hpc")+" roleOccupant="+udn("JDoe"),
		"add-entry "+gdn("hpc")+" member="+udn("JDoe")+" memberUid=JDoe",
	)
	if a := p.Ops[1].Attrs; a[len(a)-2].Name != "uid" || a[len(a)-2].Values[0] != "JDoe" {
		t.Errorf("uid attribute: %+v", a)
	}
}

// Rule: duplicate uidNumber or gidNumber values, between AD entries or
// against local entries, are warnings and never block the run.
func TestRuleDuplicateIDNumbers(t *testing.T) {
	a, b := mkUser(1, "a"), mkUser(2, "b")
	b.Attrs["uidNumber"] = a.Attrs["uidNumber"]
	local := tUser(3, "local")
	local.Set("uidNumber", a.Attrs["uidNumber"])
	g1, g2 := mkGroup(10, "g1", a.DN), mkGroup(11, "g2", a.DN)
	g2.Attrs["gidNumber"] = g1.Attrs["gidNumber"]
	p := build(t, cfg(t), world{ad: []*model.ADObject{a, b, g1, g2}, target: []*model.Entry{local}}, both)
	wantWarning(t, p, WarnDuplicateID, "uidNumber 1001: used by a, b, local "+udn("local"))
	wantWarning(t, p, WarnDuplicateID, "gidNumber 5010: used by g1, g2")
	if !hasOp(p, "add-entry "+udn("b")) || !hasOp(p, "add-entry "+gdn("g2")) {
		t.Error("duplicates must not block creation")
	}
}

// Rule: a changed uidNumber or gidNumber is applied and always reported.
func TestRuleChangedIDNumber(t *testing.T) {
	u := mkUser(1, "jdoe")
	u.Attrs["uidNumber"] = []string{"2001"}
	g := mkGroup(10, "hpc", u.DN)
	g.Attrs["gidNumber"] = []string{"6000"}
	p := build(t, cfg(t), world{ad: []*model.ADObject{u, g},
		target: []*model.Entry{tUser(1, "jdoe"), uRec(1, "jdoe"), tGroup(10, "hpc", "jdoe"), gRec(10, "hpc", "jdoe")}}, both)
	wantOps(t, p,
		"modify-entry "+udn("jdoe")+" replace uidNumber=2001",
		"modify-entry "+gdn("hpc")+" replace gidNumber=6000",
	)
	wantWarning(t, p, WarnIDChanged, udn("jdoe")+": uidNumber changes from 1001 to 2001")
	wantWarning(t, p, WarnIDChanged, gdn("hpc")+": gidNumber changes from 5010 to 6000")
}

// Rule: values of ASCII-only (IA5) attributes such as gecos are
// transliterated to ASCII.
func TestRuleASCIITransliteration(t *testing.T) {
	u := mkUser(1, "jdoe")
	u.Attrs["gecos"] = []string{"José Müller-Øster"}
	u.Attrs["loginShell"] = []string{"/bin/bäsh"}
	p := build(t, cfg(t), world{ad: []*model.ADObject{u}}, Options{Users: true})
	for _, a := range p.Ops[1].Attrs {
		switch a.Name {
		case "gecos":
			if a.Values[0] != "Jose Muller-Oster" {
				t.Errorf("gecos = %q", a.Values[0])
			}
		case "loginShell":
			if a.Values[0] != "/bin/bash" {
				t.Errorf("loginShell = %q", a.Values[0])
			}
		}
	}
}

// CLAUDE.md: nested groups are flattened with a cycle guard; a group whose
// only members are groups is still created; flatten_nested: false stops it.
func TestRuleNestedFlattening(t *testing.T) {
	jdoe, bob := mkUser(1, "jdoe"), mkUser(2, "bob")
	parent := mkGroup(10, "parent", jdoe.DN)
	child := mkGroup(11, "child", bob.DN, parent.DN) // cycle: child -> parent -> child
	parent.Members = append(parent.Members, child.DN)
	onlyGroups := mkGroup(12, "only-groups", child.DN)
	users := []*model.Entry{tUser(1, "jdoe"), uRec(1, "jdoe"), tUser(2, "bob"), uRec(2, "bob")}
	p := build(t, cfg(t), world{ad: []*model.ADObject{jdoe, bob, parent, child, onlyGroups}, target: users}, Options{Groups: true})
	both := udn("bob") + "|" + udn("jdoe")
	wantOps(t, p,
		"add-record rec:b seeAlso="+gdn("child")+" roleOccupant="+both,
		"add-entry "+gdn("child")+" member="+both+" memberUid=bob|jdoe",
		"add-record rec:c seeAlso="+gdn("only-groups")+" roleOccupant="+both,
		"add-entry "+gdn("only-groups")+" member="+both+" memberUid=bob|jdoe",
		"add-record rec:a seeAlso="+gdn("parent")+" roleOccupant="+both,
		"add-entry "+gdn("parent")+" member="+both+" memberUid=bob|jdoe",
	)
	p = build(t, cfg(t, func(c *config.Config) { c.Mapping.Groups.FlattenNested = false }),
		world{ad: []*model.ADObject{jdoe, bob, parent, child, onlyGroups}, target: users}, Options{Groups: true})
	wantOps(t, p,
		"add-record rec:b seeAlso="+gdn("child")+" roleOccupant="+udn("bob"),
		"add-entry "+gdn("child")+" member="+udn("bob")+" memberUid=bob",
		"add-record rec:a seeAlso="+gdn("parent")+" roleOccupant="+udn("jdoe"),
		"add-entry "+gdn("parent")+" member="+udn("jdoe")+" memberUid=jdoe",
	)
}

// CLAUDE.md: members resolve by DN, never by CN (CNs repeat and contain
// escaped commas).
func TestRuleResolveMembersByDN(t *testing.T) {
	a := mkUser(1, "gowe")
	a.DN = `CN=Gow\, Edward L,OU=Staff,` + adPeople
	b := mkUser(2, "gowe2")
	b.DN = `CN=Gow\, Edward L,OU=Faculty,` + adPeople
	g := mkGroup(10, "hpc", `cn=gow\2C edward l, ou=faculty,`+adPeople) // same DN, other spelling
	p := build(t, cfg(t), world{ad: []*model.ADObject{a, b, g}}, both)
	if !hasOp(p, "add-record rec:a seeAlso="+gdn("hpc")+" roleOccupant="+udn("gowe2")) {
		t.Errorf("member not resolved by DN:\n%s", strings.Join(opStrs(p), "\n"))
	}
}

// CLAUDE.md: duplicate uids are warned about and one wins; the current
// owner of the target entry wins over a newcomer.
func TestRuleDuplicateUID(t *testing.T) {
	a, b := mkUser(1, "jdoe"), mkUser(2, "jdoe")
	b.DN = "CN=jdoe-2," + adPeople
	p := build(t, cfg(t), world{ad: []*model.ADObject{a, b}}, Options{Users: true})
	wantOps(t, p, "add-record rec:1 seeAlso="+udn("jdoe"), "add-entry "+udn("jdoe")+" loginShell=/bin/bash")
	wantWarning(t, p, WarnDuplicate, `uid "jdoe" is also used by CN=jdoe,`)

	p = build(t, cfg(t), world{ad: []*model.ADObject{a, b}, target: []*model.Entry{tUser(2, "jdoe"), uRec(2, "jdoe")}}, Options{Users: true})
	wantOps(t, p)
	wantWarning(t, p, WarnDuplicate, `uid "jdoe" is also used by CN=jdoe-2,`)
}

// Hard requirement 4: users and groups sync independently.
func TestUsersAndGroupsIndependently(t *testing.T) {
	jdoe := mkUser(1, "jdoe")
	w := world{ad: []*model.ADObject{jdoe, mkGroup(10, "hpc", jdoe.DN)}}
	t.Run("users only", func(t *testing.T) {
		p := build(t, cfg(t), w, Options{Users: true})
		wantOps(t, p, "add-record rec:1 seeAlso="+udn("jdoe"), "add-entry "+udn("jdoe")+" loginShell=/bin/bash")
	})
	t.Run("groups only", func(t *testing.T) {
		p := build(t, cfg(t), world{ad: w.ad, target: []*model.Entry{tUser(1, "jdoe"), uRec(1, "jdoe")}}, Options{Groups: true})
		wantOps(t, p,
			"add-record rec:a seeAlso="+gdn("hpc")+" roleOccupant="+udn("jdoe"),
			"add-entry "+gdn("hpc")+" member="+udn("jdoe")+" memberUid=jdoe")
	})
	// A groups-only run never adds a user that has no entry on the target
	// yet (with member or memberUid only): the member is skipped until a
	// users sync, or someone else, creates the entry.
	t.Run("groups only skips users not yet created", func(t *testing.T) {
		bob := mkUser(2, "bob")
		ad := []*model.ADObject{jdoe, bob, mkGroup(10, "hpc", jdoe.DN, bob.DN)}
		p := build(t, cfg(t), world{ad: ad, target: []*model.Entry{tUser(1, "jdoe"), uRec(1, "jdoe")}}, Options{Groups: true})
		wantOps(t, p,
			"add-record rec:a seeAlso="+gdn("hpc")+" roleOccupant="+udn("jdoe"),
			"add-entry "+gdn("hpc")+" member="+udn("jdoe")+" memberUid=jdoe")
		wantMissing(t, p, "bob in "+gdn("hpc")+": no entry on the target at "+udn("bob"))
		if len(p.Warnings) != 0 {
			t.Errorf("a skipped member is not a warning: %v", p.Warnings)
		}

		p = build(t, cfg(t), world{ad: ad}, Options{Groups: true})
		wantOps(t, p) // no member has an entry, so the group waits too
		wantWarning(t, p, WarnPending, gdn("hpc")+": AD group has no resolvable members")

		p = build(t, cfg(t, uidOnly), world{ad: ad}, Options{Groups: true})
		wantOps(t, p, // posixGroup may be empty
			"add-record rec:a seeAlso="+gdn("hpc"),
			"add-entry "+gdn("hpc"))
		wantMissing(t, p,
			"bob in "+gdn("hpc")+": no entry on the target under "+people,
			"jdoe in "+gdn("hpc")+": no entry on the target under "+people)

		p = build(t, cfg(t), world{ad: ad}, both) // users are created first
		index(t, p, "add-entry "+gdn("hpc")+" member="+udn("bob")+"|"+udn("jdoe"))
		wantMissing(t, p)
	})
	t.Run("groups only follows the target's user DN, not a pending rename", func(t *testing.T) {
		u := mkUser(1, "jdoe2")
		p := build(t, cfg(t), world{ad: []*model.ADObject{u, mkGroup(10, "hpc", u.DN)},
			target: []*model.Entry{tUser(1, "jdoe"), uRec(1, "jdoe"), tGroup(10, "hpc", "jdoe"), gRec(10, "hpc", "jdoe")}},
			Options{Groups: true})
		wantOps(t, p)
	})
	t.Run("users only leaves memberships of gone users to the groups phase", func(t *testing.T) {
		p := build(t, cfg(t), world{ad: []*model.ADObject{mkUser(2, "bob")},
			target: []*model.Entry{tUser(1, "jdoe"), uRec(1, "jdoe"), tGroup(10, "hpc", "jdoe"), gRec(10, "hpc", "jdoe")}},
			Options{Users: true})
		if hasOp(p, "delete-member") {
			t.Errorf("users-only run removed memberships:\n%s", strings.Join(opStrs(p), "\n"))
		}
	})
}

// "Adopting an existing tree": adopt claims entries that match by name and
// marks members that are also in the AD group as owned; the rest are
// local. Users ignored by sync are claimed too, so the first sync removes
// their memberships.
func TestAdopt(t *testing.T) {
	jdoe, bob := mkUser(1, "jdoe"), mkUser(2, "bob")
	nogid := mkUser(3, "nogid")
	delete(nogid.Attrs, "gidNumber")
	gone := mkUser(4, "notintarget")
	child := mkGroup(11, "child", nogid.DN)
	delete(child.Attrs, "gidNumber") // ignored by sync, but ad2openldap flattened it
	hpc := mkGroup(10, "hpc", jdoe.DN, child.DN)
	target := []*model.Entry{tUser(1, "jdoe"), tUser(2, "bob"), tUser(3, "nogid"), tUser(5, "local1"),
		tGroup(10, "hpc", "jdoe", "nogid", "bob", "local1")}
	p := build(t, cfg(t), world{ad: []*model.ADObject{jdoe, bob, nogid, gone, child, hpc}, target: target}, Options{Adopt: true})
	wantOps(t, p,
		"add-record rec:2 seeAlso="+udn("bob"),
		"add-record rec:1 seeAlso="+udn("jdoe"),
		"add-record rec:3 seeAlso="+udn("nogid"),
		"add-record rec:a seeAlso="+gdn("hpc")+" roleOccupant="+udn("jdoe")+"|"+udn("nogid"),
	)
	if p.Guard.Tripped {
		t.Error("guard must not apply to adopt")
	}

	// The first sync after adopt: nogid is ignored, so it loses the owned
	// membership; bob and local1 are local and stay.
	after := append(target, uRec(1, "jdoe"), uRec(2, "bob"), uRec(3, "nogid"), gRec(10, "hpc", "jdoe", "nogid"))
	p = build(t, cfg(t), world{ad: []*model.ADObject{jdoe, bob, nogid, gone, child, hpc}, target: after}, both)
	for _, want := range []string{
		"update-record rec:3 replace description=missing-since=",
		"delete-member " + gdn("hpc") + " member=" + udn("nogid"),
		"update-record rec:a delete roleOccupant=" + udn("nogid"),
	} {
		index(t, p, want)
	}
	if hasOp(p, "delete-member "+gdn("hpc")+" member="+udn("bob")) {
		t.Error("adopt made bob owned")
	}

	t.Run("already adopted is a no-op", func(t *testing.T) {
		p := build(t, cfg(t), world{ad: []*model.ADObject{jdoe, bob, nogid, gone, child, hpc}, target: after}, Options{Adopt: true})
		wantOps(t, p)
	})
}

// Crash-safe order: the ownership record is written before an entry or
// member is added, and entries and members are removed before their record.
func TestCrashSafeOrdering(t *testing.T) {
	jdoe, bob := mkUser(1, "jdoe"), mkUser(2, "bob")
	p := build(t, cfg(t, func(c *config.Config) { c.Sync.PruneUsers = true }), world{
		ad: []*model.ADObject{jdoe, bob, mkGroup(10, "hpc", jdoe.DN), mkGroup(11, "new", jdoe.DN)},
		target: []*model.Entry{tUser(3, "old"), uRec(3, "old", "missing-since=2020-01-01"),
			tUser(2, "bob"), uRec(2, "bob"),
			tGroup(10, "hpc", "bob", "old"), gRec(10, "hpc", "bob", "old")},
	}, both)
	before := func(a, b string) {
		t.Helper()
		if i, j := index(t, p, a), index(t, p, b); i >= j {
			t.Errorf("%q (#%d) must come before %q (#%d)", a, i, b, j)
		}
	}
	// adds: record first
	before("add-record rec:1 seeAlso="+udn("jdoe"), "add-entry "+udn("jdoe"))
	before("add-record rec:b seeAlso="+gdn("new"), "add-entry "+gdn("new"))
	before("update-record rec:a add roleOccupant="+udn("jdoe"), "add-member "+gdn("hpc")+" member="+udn("jdoe"))
	before("update-record rec:a add roleOccupant="+udn("jdoe"), "add-member "+gdn("hpc")+" memberUid=jdoe")
	// removals: values and entries first, record last
	before("delete-member "+gdn("hpc")+" member="+udn("bob"), "update-record rec:a delete roleOccupant="+udn("bob"))
	before("delete-member "+gdn("hpc")+" memberUid=bob", "update-record rec:a delete roleOccupant="+udn("bob"))
	before("delete-member "+gdn("hpc")+" member="+udn("old"), "update-record rec:a delete roleOccupant="+udn("old"))
	before("delete-entry "+udn("old"), "delete-record rec:3")
	before("update-record rec:a delete roleOccupant="+udn("old"), "delete-entry "+udn("old"))
}

// Crash recovery: the plan finishes what an interrupted run started.
func TestCrashRecovery(t *testing.T) {
	jdoe := mkUser(1, "jdoe")
	t.Run("record written, member not added", func(t *testing.T) {
		p := build(t, cfg(t), world{ad: []*model.ADObject{jdoe, mkGroup(10, "hpc", jdoe.DN)},
			target: []*model.Entry{tUser(1, "jdoe"), uRec(1, "jdoe"), tGroup(10, "hpc", "local1"), gRec(10, "hpc", "jdoe")}}, both)
		wantOps(t, p,
			"add-member "+gdn("hpc")+" member="+udn("jdoe"),
			"add-member "+gdn("hpc")+" memberUid=jdoe")
	})
	t.Run("member removed, record not updated", func(t *testing.T) {
		p := build(t, cfg(t), world{ad: []*model.ADObject{jdoe, mkGroup(10, "hpc")},
			target: []*model.Entry{tUser(1, "jdoe"), uRec(1, "jdoe"), tGroup(10, "hpc", "local1"), gRec(10, "hpc", "jdoe")}}, both)
		wantOps(t, p, "update-record rec:a delete roleOccupant="+udn("jdoe"))
	})
	t.Run("record added, entry not created", func(t *testing.T) {
		p := build(t, cfg(t), world{ad: []*model.ADObject{jdoe}, target: []*model.Entry{uRec(1, "jdoe")}}, Options{Users: true})
		wantOps(t, p, "add-entry "+udn("jdoe")+" loginShell=/bin/bash")
	})
	t.Run("rename recorded, modrdn not applied", func(t *testing.T) {
		u := mkUser(1, "jdoe2")
		p := build(t, cfg(t), world{ad: []*model.ADObject{u, mkGroup(10, "hpc", u.DN)},
			target: []*model.Entry{tUser(1, "jdoe"), uRec(1, "jdoe2", "renaming-from="+udn("jdoe")),
				tGroup(10, "hpc", "jdoe"), gRec(10, "hpc", "jdoe")}}, both)
		wantOps(t, p,
			"modrdn "+udn("jdoe")+" -> uid=jdoe2",
			"rename-member "+gdn("hpc")+" member "+udn("jdoe")+" -> "+udn("jdoe2"),
			"rename-member "+gdn("hpc")+" memberUid jdoe -> jdoe2",
			"update-record rec:a delete roleOccupant="+udn("jdoe")+"; add roleOccupant="+udn("jdoe2"),
			"update-record rec:1 replace description=",
			"modify-entry "+udn("jdoe2")+" replace cn=jdoe2",
		)
	})
	t.Run("modrdn applied, group values not yet renamed", func(t *testing.T) {
		u := mkUser(1, "jdoe2")
		e := tUser(1, "jdoe2")
		p := build(t, cfg(t), world{ad: []*model.ADObject{u, mkGroup(10, "hpc", u.DN)},
			target: []*model.Entry{e, uRec(1, "jdoe2", "renaming-from="+udn("jdoe")),
				tGroup(10, "hpc", "jdoe"), gRec(10, "hpc", "jdoe")}}, both)
		wantOps(t, p,
			"rename-member "+gdn("hpc")+" member "+udn("jdoe")+" -> "+udn("jdoe2"),
			"rename-member "+gdn("hpc")+" memberUid jdoe -> jdoe2",
			"update-record rec:a delete roleOccupant="+udn("jdoe")+"; add roleOccupant="+udn("jdoe2"),
			"update-record rec:1 replace description=",
		)
	})
}

// The mass-deletion guard counts removals across the run, separately for
// memberships and users, against max_delete_min and max_delete_percent.
func TestGuard(t *testing.T) {
	// 100 owned memberships in one group; AD keeps `keep` of them.
	guardWorld := func(keep int) world {
		var ad []*model.ADObject
		var target []*model.Entry
		var uids, members []string
		for i := 1; i <= 100; i++ {
			uid := "u" + strconv.Itoa(i)
			u := mkUser(i, uid)
			u.GUID = guid(1000 + i)
			ad = append(ad, u)
			target = append(target, tUser(i, uid), (&model.Record{Kind: model.UserRecord, GUID: u.GUID, SeeAlso: udn(uid)}).Entry(state))
			uids = append(uids, uid)
			if i <= keep {
				members = append(members, u.DN)
			}
		}
		ad = append(ad, mkGroup(10, "big", members...))
		target = append(target, tGroup(10, "big", uids...), gRec(10, "big", uids...))
		return world{ad: ad, target: target}
	}
	for _, tc := range []struct {
		name    string
		keep    int
		min     int
		pct     float64
		tripped bool
	}{
		{"above min and percent", 70, 25, 10, true},
		{"at min", 75, 25, 10, false},
		{"below percent", 70, 25, 50, false},
		{"min zero, percent zero", 99, 0, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := cfg(t, func(c *config.Config) { c.Sync.MaxDeleteMin = tc.min; c.Sync.MaxDeletePercent = tc.pct })
			p := build(t, c, guardWorld(tc.keep), both)
			if p.Guard.MembershipRemovals != 100-tc.keep || p.Guard.OwnedMemberships != 100 {
				t.Fatalf("counts: %+v", p.Guard)
			}
			if p.Guard.Tripped != tc.tripped {
				t.Errorf("tripped = %v, want %v (%v)", p.Guard.Tripped, tc.tripped, p.Guard.Reasons)
			}
		})
	}
	t.Run("user removals", func(t *testing.T) {
		var target []*model.Entry
		for i := 1; i <= 30; i++ {
			uid := "u" + strconv.Itoa(i)
			target = append(target, tUser(i, uid), uRec(i, uid, "missing-since=2020-01-01"))
		}
		p := build(t, cfg(t, func(c *config.Config) { c.Sync.PruneUsers = true }),
			world{ad: []*model.ADObject{mkUser(99, "x"), mkGroup(10, "g")}, target: target}, Options{Users: true})
		if p.Guard.UserRemovals != 30 || !p.Guard.Tripped {
			t.Errorf("guard: %+v", p.Guard)
		}
	})
	// Local memberships removed by a prune are reported, but they aren't
	// owned, so they count neither toward the percentage nor the minimum.
	t.Run("local removals by prune don't count", func(t *testing.T) {
		target := []*model.Entry{tUser(1, "jdoe"), uRec(1, "jdoe", "missing-since=2020-01-01")}
		for i := 0; i < 50; i++ {
			target = append(target, tGroup(20+i, "local"+strconv.Itoa(i), "jdoe", "other"))
		}
		p := build(t, cfg(t, func(c *config.Config) { c.Sync.PruneUsers = true }),
			world{ad: []*model.ADObject{mkUser(2, "bob"), mkGroup(10, "g", mkUser(2, "bob").DN)}, target: target}, both)
		if p.Guard.LocalRemovals != 50 || p.Guard.MembershipRemovals != 0 || p.Guard.UserRemovals != 1 || p.Guard.Tripped {
			t.Errorf("guard: %+v", p.Guard)
		}
		var b strings.Builder
		p.Print(&b)
		for _, want := range []string{"plus 50 local memberships removed by prune (not counted by the guard)", "(50 of them local, removed by a prune;"} {
			if !strings.Contains(b.String(), want) {
				t.Errorf("plan output lacks %q:\n%s", want, b.String())
			}
		}
	})
	t.Run("AD returned nothing", func(t *testing.T) {
		p := build(t, cfg(t), world{}, both)
		if !p.Guard.Tripped || len(p.Guard.Reasons) != 2 {
			t.Errorf("guard: %+v", p.Guard)
		}
	})
}

// Plans are deterministic regardless of the order AD returns objects in.
func TestDeterministic(t *testing.T) {
	var ad []*model.ADObject
	var dns []string
	for i := 1; i <= 30; i++ {
		u := mkUser(i, "user"+strconv.Itoa(i))
		ad = append(ad, u)
		dns = append(dns, u.DN)
	}
	for i := 0; i < 5; i++ {
		ad = append(ad, mkGroup(10+i, "group"+strconv.Itoa(i), dns[i*5:i*5+10]...))
	}
	c := cfg(t)
	want := strings.Join(opStrs(build(t, c, world{ad: ad}, both)), "\n")
	r := rand.New(rand.NewSource(1))
	for n := 0; n < 5; n++ {
		shuffled := append([]*model.ADObject(nil), ad...)
		r.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		if got := strings.Join(opStrs(build(t, c, world{ad: shuffled}, both)), "\n"); got != want {
			t.Fatalf("plan depends on input order")
		}
	}
}

// The planner must not modify its inputs.
func TestInputsUnchanged(t *testing.T) {
	jdoe := mkUser(1, "jdoe2")
	g := tGroup(10, "hpc", "jdoe")
	target := []*model.Entry{tUser(1, "jdoe"), uRec(1, "jdoe"), g, gRec(10, "hpc", "jdoe")}
	build(t, cfg(t), world{ad: []*model.ADObject{jdoe, mkGroup(10, "hpc", jdoe.DN)}, target: target}, both)
	if g.Get("memberUid")[0] != "jdoe" || target[0].DN != udn("jdoe") {
		t.Error("planner modified the target snapshot")
	}
}

// A stale renaming-from note (a crash after the modrdn, before the note was
// cleared) must never touch an entry Dolly doesn't manage for that record,
// even when the old name has since been reused.
func TestRuleStaleRenamingFromNote(t *testing.T) {
	t.Run("user: local entry reuses the old name", func(t *testing.T) {
		u := mkUser(1, "jdoe2")
		p := build(t, cfg(t), world{
			ad: []*model.ADObject{u, mkGroup(10, "hpc", u.DN)},
			target: []*model.Entry{tUser(1, "jdoe2"), uRec(1, "jdoe2", "renaming-from="+udn("jdoe")),
				tGroup(10, "hpc", "jdoe2"), gRec(10, "hpc", "jdoe2"),
				tUser(5, "jdoe"), tGroup(11, "lab", "jdoe")}, // local jdoe, created after the crash
		}, both)
		wantOps(t, p, "update-record rec:1 replace description=")
		wantWarning(t, p, WarnConflict, udn("jdoe")+": record "+guid(1)+" still lists this DN as renaming-from")
	})
	t.Run("user: another record owns the old name", func(t *testing.T) {
		// guid(1) was jdoe, renamed to jdoe2, and its entry is gone; guid(2)
		// is the new jdoe. The note must not make jdoe look like guid(1)'s.
		u1, u2 := mkUser(1, "jdoe2"), mkUser(2, "jdoe")
		p := build(t, cfg(t), world{
			ad: []*model.ADObject{u1, u2, mkGroup(10, "hpc", u2.DN)},
			target: []*model.Entry{uRec(1, "jdoe2", "renaming-from="+udn("jdoe")),
				tUser(2, "jdoe"), uRec(2, "jdoe"), tGroup(10, "hpc", "jdoe"), gRec(10, "hpc", "jdoe")},
		}, Options{Users: true})
		wantOps(t, p,
			"update-record rec:1 replace description=",
			"add-entry "+udn("jdoe2")+" loginShell=/bin/bash",
		)
		wantWarning(t, p, WarnConflict, udn("jdoe")+": record "+guid(1)+" still lists this DN as renaming-from")
	})
	grec := func(n int, cn string, notes ...string) *model.Entry {
		return (&model.Record{Kind: model.GroupRecord, GUID: guid(n), SeeAlso: gdn(cn), Notes: notes}).Entry(state)
	}
	t.Run("group: local group reuses the old name", func(t *testing.T) {
		p := build(t, cfg(t, uidOnly), world{
			ad: []*model.ADObject{mkUser(1, "x"), mkGroup(10, "hpc-new")},
			target: []*model.Entry{tGroupUID(10, "hpc-new"), grec(10, "hpc-new", "renaming-from="+gdn("hpc")),
				tGroupUID(11, "hpc", "local1")},
		}, Options{Groups: true})
		wantOps(t, p, "update-record rec:a replace description=")
		wantWarning(t, p, WarnConflict, gdn("hpc")+": record "+guid(10)+" still lists this DN as renaming-from")
	})
	t.Run("group: another record owns the old name", func(t *testing.T) {
		p := build(t, cfg(t, uidOnly), world{
			ad: []*model.ADObject{mkUser(1, "x"), mkGroup(10, "hpc-new"), mkGroup(11, "hpc")},
			target: []*model.Entry{grec(10, "hpc-new", "renaming-from="+gdn("hpc")),
				tGroupUID(11, "hpc"), gRec(11, "hpc")},
		}, Options{Groups: true})
		wantOps(t, p,
			"update-record rec:a replace description=",
			"add-entry "+gdn("hpc-new"),
		)
		if hasOp(p, "modrdn") {
			t.Error("renamed another record's group")
		}
	})
}

// An owned entry that isn't directly under its base (moved to a sub-OU by
// an admin) is a conflict: Dolly renames in place and never moves entries.
func TestRuleOwnedEntryInSubOU(t *testing.T) {
	sub := "uid=jdoe,ou=staff," + people
	e := tUser(1, "jdoe")
	e.DN = sub
	r := (&model.Record{Kind: model.UserRecord, GUID: guid(1), SeeAlso: sub}).Entry(state)
	staff := &model.Entry{DN: "ou=staff," + people, Attrs: map[string][]string{"objectClass": {"organizationalUnit"}}}
	u := mkUser(1, "jdoe")
	u.Attrs["uidNumber"] = []string{"2001"} // a change that must not be applied either
	p := build(t, cfg(t), world{ad: []*model.ADObject{u}, target: []*model.Entry{staff, e, r}}, Options{Users: true})
	wantOps(t, p)
	wantWarning(t, p, WarnConflict, sub+": Dolly-owned entry is not directly under users_base")
}

// CLAUDE.md: duplicate group names are warned about and one wins (lowest
// DN); the current owner of the target group wins over a newcomer.
func TestRuleDuplicateGroupCN(t *testing.T) {
	jdoe := mkUser(1, "jdoe")
	a, b := mkGroup(10, "hpc", jdoe.DN), mkGroup(11, "hpc")
	b.DN = "CN=hpc,OU=Other," + adGroups
	users := []*model.Entry{tUser(1, "jdoe"), uRec(1, "jdoe")}
	p := build(t, cfg(t), world{ad: []*model.ADObject{jdoe, a, b}, target: users}, Options{Groups: true})
	wantOps(t, p,
		"add-record rec:a seeAlso="+gdn("hpc")+" roleOccupant="+udn("jdoe"),
		"add-entry "+gdn("hpc")+" member="+udn("jdoe")+" memberUid=jdoe")
	wantWarning(t, p, WarnDuplicate, `cn "hpc" is also used by CN=hpc,`+adGroups)

	p = build(t, cfg(t), world{ad: []*model.ADObject{jdoe, a, b},
		target: append(users, tGroup(11, "hpc"), gRec(11, "hpc"))}, Options{Groups: true})
	if hasOp(p, "add-") || hasOp(p, "update-record rec:a") {
		t.Errorf("the duplicate took over the owned group:\n%s", strings.Join(opStrs(p), "\n"))
	}
	wantWarning(t, p, WarnDuplicate, `cn "hpc" is also used by `+b.DN)
}

// A user whose target entry is a conflict (no record) is still added to an
// owned group by DN: the member value names the entry that exists.
func TestRuleConflictUserAddedToOwnedGroup(t *testing.T) {
	jdoe := mkUser(1, "jdoe")
	p := build(t, cfg(t), world{
		ad:     []*model.ADObject{jdoe, mkGroup(10, "hpc", jdoe.DN)},
		target: []*model.Entry{tUser(1, "jdoe"), tGroup(10, "hpc", "local1"), gRec(10, "hpc")},
	}, both)
	wantOps(t, p,
		"update-record rec:a add roleOccupant="+udn("jdoe"),
		"add-member "+gdn("hpc")+" member="+udn("jdoe"),
		"add-member "+gdn("hpc")+" memberUid=jdoe",
	)
	wantWarning(t, p, WarnConflict, udn("jdoe")+": entry exists on the target without an ownership record")
}
