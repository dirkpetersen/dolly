package planner

import (
	"context"
	"strings"
	"testing"

	"github.com/dirkpetersen/dolly/internal/config"
	"github.com/dirkpetersen/dolly/internal/model"
	"github.com/dirkpetersen/dolly/internal/source"
)

// buildGroupsOnly runs a groups-only plan the way `dolly sync --groups`
// does against a real target: AD users are found only by DN (the users base
// isn't read), users_base isn't read either, and the member candidates are
// looked up among the world's user entries, like the target's batched uid
// lookups. It returns the plan and the uids that were looked up.
func buildGroupsOnly(t *testing.T, c *config.Config, w world) (*Plan, []string) {
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
	snap, err := source.Read(context.Background(), fake, false)
	if err != nil {
		t.Fatal(err)
	}
	tgt, recs, err := model.Classify(append(containers(), w.target...), model.Bases{Users: c.Target.UsersBase, Groups: c.Target.GroupsBase, State: c.Target.StateBase})
	if err != nil {
		t.Fatal(err)
	}
	userEntries := tgt.Users
	tgt.Users, tgt.UsersRead = nil, false
	cands, err := MemberCandidates(snap, tgt, recs, c)
	if err != nil {
		t.Fatal(err)
	}
	var uids []string
	set := model.NewUserSet()
	for _, cand := range cands {
		uids = append(uids, cand.UID)
		for _, e := range userEntries {
			for _, v := range e.Get("uid") {
				if strings.EqualFold(v, cand.UID) {
					set.Add(e.DN, e.Get("uid")...)
				}
			}
		}
	}
	tgt.ExistingUsers = set
	p, err := Build(snap, tgt, recs, c, Options{Groups: true, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	return p, uids
}

// noNumbers is an AD user with only the attribute uid is mapped from, like
// the users of a groups-only deployment whose Unix numbers live on the
// target only.
func noNumbers(n int, uid string) *model.ADObject {
	u := mkUser(n, uid)
	delete(u.Attrs, "uidNumber")
	delete(u.Attrs, "gidNumber")
	return u
}

// Rule: to be a group member, an AD user needs only the attribute its uid is
// mapped from. uidNumber and gidNumber (the rest of the users required list)
// are needed only for a user entry Dolly creates or manages.
func TestRuleRelaxedMemberRequirements(t *testing.T) {
	jdoe := noNumbers(1, "jdoe")
	nouid := mkUser(2, "nouid")
	delete(nouid.Attrs, "uid")
	ad := []*model.ADObject{jdoe, nouid, mkGroup(10, "hpc", jdoe.DN, nouid.DN)}
	local := &model.Entry{DN: udn("jdoe"), Attrs: map[string][]string{"objectClass": {"account", "posixAccount"}, "uid": {"jdoe"}}}

	t.Run("groups only", func(t *testing.T) {
		p := build(t, cfg(t, uidOnly), world{ad: ad, target: []*model.Entry{local}}, Options{Groups: true})
		wantOps(t, p,
			"add-record rec:a seeAlso="+gdn("hpc")+" roleOccupant="+udn("jdoe"),
			"add-entry "+gdn("hpc")+" memberUid=jdoe")
		wantWarning(t, p, WarnIgnored, nouid.DN+": user missing required attribute uid")
		// A groups-only run manages no user entries, so a user without Unix
		// numbers is not listed as ignored.
		if n := len(warnings(p, WarnIgnored)); n != 1 {
			t.Errorf("ignored = %v, want only the user without a uid", warnings(p, WarnIgnored))
		}
	})
	t.Run("users and groups", func(t *testing.T) {
		p := build(t, cfg(t), world{ad: ad, target: []*model.Entry{local}}, both)
		// No user entry is created for jdoe (the local entry would be a
		// conflict anyway), but jdoe is added to the group.
		wantOps(t, p,
			"add-record rec:a seeAlso="+gdn("hpc")+" roleOccupant="+udn("jdoe"),
			"add-entry "+gdn("hpc")+" member="+udn("jdoe")+" memberUid=jdoe")
		wantWarning(t, p, WarnIgnored, jdoe.DN+": user missing required attribute uidNumber, gidNumber; no user entry is managed for it (it can still be a group member)")
		if p.Counts.IgnoredEntries != 2 {
			t.Errorf("ignored = %d, want 2 (each listed once)", p.Counts.IgnoredEntries)
		}
		if hasOp(p, "add-entry "+udn("jdoe")) || len(warnings(p, WarnConflict)) != 0 {
			t.Errorf("a user without Unix numbers must not be created or conflict:\n%s\n%v", strings.Join(opStrs(p), "\n"), p.Warnings)
		}
	})
	t.Run("losing uidNumber keeps owned memberships", func(t *testing.T) {
		// The user entry is treated as gone (the prune clock starts), but the
		// user is still a valid member, so the owned membership stays.
		p := build(t, cfg(t), world{ad: []*model.ADObject{jdoe, mkGroup(10, "hpc", jdoe.DN)},
			target: []*model.Entry{tUser(1, "jdoe"), uRec(1, "jdoe"), tGroup(10, "hpc", "jdoe"), gRec(10, "hpc", "jdoe")}}, both)
		wantOps(t, p, "update-record rec:1 replace description=missing-since=2024-06-01T12:00:00Z")
	})
}

// Rule (always on): the users to be added to a target group must exist on
// the target. A member is added only if an entry for it exists under
// users_base, Dolly-owned or local: by uid with memberUid only, at the
// member DN with member. Otherwise it is skipped: not a warning, only a
// summary count and a --debug line per (group, uid) pair.
func TestRuleMembersMustExistOnTarget(t *testing.T) {
	jdoe, bob := noNumbers(1, "jdoe"), noNumbers(2, "bob")
	ad := []*model.ADObject{jdoe, bob, mkGroup(10, "hpc", jdoe.DN, bob.DN), mkGroup(11, "lab", bob.DN)}
	localJdoe := localUID(udn("jdoe"), "jdoe")

	t.Run("memberUid", func(t *testing.T) {
		p := build(t, cfg(t, uidOnly), world{ad: ad, target: []*model.Entry{localJdoe}}, Options{Groups: true})
		wantOps(t, p,
			"add-record rec:a seeAlso="+gdn("hpc")+" roleOccupant="+udn("jdoe"),
			"add-entry "+gdn("hpc")+" memberUid=jdoe",
			"add-record rec:b seeAlso="+gdn("lab"),
			"add-entry "+gdn("lab"))
		// One entry per (group, uid) pair, sorted by group, then uid.
		wantMissing(t, p,
			"bob in "+gdn("hpc")+": no entry on the target under "+people,
			"bob in "+gdn("lab")+": no entry on the target under "+people)
		if len(p.Warnings) != 0 {
			t.Errorf("skipped members must not be warnings: %v", p.Warnings)
		}
	})
	t.Run("member DN must exist", func(t *testing.T) {
		// An entry with uid bob exists, but not at the member DN.
		elsewhere := localUID("uid=bob,ou=staff,"+people, "bob")
		p := build(t, cfg(t), world{ad: ad, target: []*model.Entry{localJdoe, elsewhere}}, Options{Groups: true})
		wantOps(t, p,
			"add-record rec:a seeAlso="+gdn("hpc")+" roleOccupant="+udn("jdoe"),
			"add-entry "+gdn("hpc")+" member="+udn("jdoe")+" memberUid=jdoe")
		wantMissing(t, p,
			"bob in "+gdn("hpc")+": no entry on the target at "+udn("bob"),
			"bob in "+gdn("lab")+": no entry on the target at "+udn("bob"))
		wantWarning(t, p, WarnPending, gdn("lab")+": AD group has no resolvable members")
	})
	t.Run("entries created in the same run count", func(t *testing.T) {
		a, b := mkUser(1, "jdoe"), mkUser(2, "bob")
		p := build(t, cfg(t), world{ad: []*model.ADObject{a, b, mkGroup(10, "hpc", a.DN, b.DN)}}, both)
		index(t, p, "add-entry "+gdn("hpc")+" member="+udn("bob")+"|"+udn("jdoe"))
		wantMissing(t, p)
	})
	t.Run("owned member whose entry vanished is kept, not re-added", func(t *testing.T) {
		p := build(t, cfg(t, uidOnly), world{ad: ad, target: []*model.Entry{localJdoe,
			tGroupUID(10, "hpc", "jdoe", "bob"), gRec(10, "hpc", "jdoe", "bob"), tGroupUID(11, "lab", "bob"), gRec(11, "lab", "bob")}}, Options{Groups: true})
		wantOps(t, p)
		wantMissing(t, p,
			"bob in "+gdn("hpc")+": no entry on the target under "+people,
			"bob in "+gdn("lab")+": no entry on the target under "+people)
		if p.Guard.MembershipRemovals != 0 {
			t.Errorf("removals = %d", p.Guard.MembershipRemovals)
		}
	})
	t.Run("owned member whose entry vanished and whose value was removed stays out", func(t *testing.T) {
		// The record still names bob, but neither the value nor the entry is
		// there: the value isn't restored while the entry is missing.
		p := build(t, cfg(t, uidOnly), world{ad: ad, target: []*model.Entry{localJdoe,
			tGroupUID(10, "hpc", "jdoe"), gRec(10, "hpc", "jdoe", "bob")}}, Options{Groups: true})
		if hasOp(p, "add-member "+gdn("hpc")) || hasOp(p, "delete-member") || hasOp(p, "update-record rec:a") {
			t.Errorf("unexpected ops:\n  %s", strings.Join(opStrs(p), "\n  "))
		}
	})
}

// Rule: a skipped member is re-evaluated on every run. Run 1 skips bob
// because his entry doesn't exist; once someone creates it, run 2 adds him
// and records ownership (record first).
func TestRuleMissingMemberSelfHeals(t *testing.T) {
	jdoe, bob := noNumbers(1, "jdoe"), noNumbers(2, "bob")
	ad := []*model.ADObject{jdoe, bob, mkGroup(10, "hpc", jdoe.DN, bob.DN)}
	for _, tc := range []struct {
		name string
		mod  func(*config.Config)
		grp  func(uids ...string) *model.Entry
		add  []string
	}{
		{"memberUid", uidOnly, func(u ...string) *model.Entry { return tGroupUID(10, "hpc", u...) },
			[]string{"add-member " + gdn("hpc") + " memberUid=bob"}},
		{"member", func(*config.Config) {}, func(u ...string) *model.Entry { return tGroup(10, "hpc", u...) },
			[]string{"add-member " + gdn("hpc") + " member=" + udn("bob"), "add-member " + gdn("hpc") + " memberUid=bob"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Run 1: bob has no entry yet. The group already holds jdoe.
			w := world{ad: ad, target: []*model.Entry{localUID(udn("jdoe"), "jdoe"), tc.grp("jdoe"), gRec(10, "hpc", "jdoe")}}
			p, _ := buildGroupsOnly(t, cfg(t, tc.mod), w)
			wantOps(t, p)
			if len(p.MissingMembers) != 1 || p.MissingMembers[0].UID != "bob" {
				t.Fatalf("run 1: missing = %v, want bob", missing(p))
			}
			// Run 2: bob's entry now exists (created by someone else).
			w.target = append(w.target, localUID(udn("bob"), "bob"))
			p, _ = buildGroupsOnly(t, cfg(t, tc.mod), w)
			wantOps(t, p, append([]string{"update-record rec:a add roleOccupant=" + udn("bob")}, tc.add...)...)
			wantMissing(t, p)
		})
	}
}

// Regression: a user pruned in a run must not be added back to its groups
// in the same run. Pruning also removes the user's local memberships; if
// the groups phase then re-added the user (still a valid member through its
// uid), former local memberships would turn into Dolly-owned ones. The
// pruned entry is deleted, so it isn't on the target and is skipped.
func TestRulePrunedUserNotReAdded(t *testing.T) {
	jdoe := noNumbers(1, "jdoe") // lost uidNumber: no user entry, but still a valid member
	prune := func(c *config.Config) { c.Sync.PruneUsers = true; c.Sync.PruneAfterDays = 30 }
	for _, tc := range []struct {
		name string
		mod  func(*config.Config)
		grp  *model.Entry
		del  []string
	}{
		{"member", prune, tGroup(10, "hpc", "jdoe", "local1"),
			[]string{"delete-member " + gdn("hpc") + " member=" + udn("jdoe"), "delete-member " + gdn("hpc") + " memberUid=jdoe"}},
		{"memberUid", func(c *config.Config) { uidOnly(c); prune(c) }, tGroupUID(10, "hpc", "jdoe", "local1"),
			[]string{"delete-member " + gdn("hpc") + " memberUid=jdoe"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// jdoe is a local member of hpc (no roleOccupant), and has been
			// gone as a user entry for longer than prune_after_days.
			p := build(t, cfg(t, tc.mod), world{
				ad:     []*model.ADObject{jdoe, mkGroup(10, "hpc", jdoe.DN)},
				target: []*model.Entry{tUser(1, "jdoe"), uRec(1, "jdoe", "missing-since=2024-01-01T00:00:00Z"), tc.grp, gRec(10, "hpc")},
			}, both)
			wantOps(t, p, append(tc.del, "delete-entry "+udn("jdoe"), "delete-record rec:1")...)
			if hasOp(p, "add-member") || hasOp(p, "update-record rec:a") {
				t.Errorf("a pruned user must not be re-added or claimed:\n  %s", strings.Join(opStrs(p), "\n  "))
			}
			if len(p.MissingMembers) != 1 || p.MissingMembers[0].UID != "jdoe" {
				t.Errorf("missing = %v, want jdoe", missing(p))
			}
		})
	}
}

// A groups-only run reads neither the AD users base nor users_base: AD
// members come from DN lookups and the target's users from uid lookups of
// just the member candidates.
func TestGroupsOnlyWithLookups(t *testing.T) {
	jdoe, bob, carol := noNumbers(1, "jdoe"), noNumbers(2, "bob"), noNumbers(3, "carol")
	notMember := mkUser(4, "dave")
	carol.UserAccountControl = 514 // disabled: not a candidate
	ad := []*model.ADObject{jdoe, bob, carol, notMember, mkGroup(10, "hpc", jdoe.DN, bob.DN, carol.DN)}
	target := []*model.Entry{
		{DN: udn("JDoe"), Attrs: map[string][]string{"uid": {"JDoe"}}}, // uid matching ignores case
		{DN: udn("dave"), Attrs: map[string][]string{"uid": {"dave"}}},
		tGroupUID(10, "hpc", "local1"), gRec(10, "hpc"),
	}
	p, looked := buildGroupsOnly(t, cfg(t, uidOnly), world{ad: ad, target: target})
	if strings.Join(looked, ",") != "bob,jdoe" {
		t.Errorf("looked up %v, want only the enabled members bob,jdoe", looked)
	}
	wantOps(t, p,
		"update-record rec:a add roleOccupant="+udn("jdoe"),
		"add-member "+gdn("hpc")+" memberUid=jdoe")
	wantMissing(t, p, "bob in "+gdn("hpc")+": no entry on the target under "+people)
	if p.ADUsersRead || p.TargetUsersRead || p.Counts.ADUsers != 0 || p.Counts.Followed != 3 {
		t.Errorf("read flags %v %v, AD users %d, followed %d", p.ADUsersRead, p.TargetUsersRead, p.Counts.ADUsers, p.Counts.Followed)
	}
	// The users base wasn't read, so zero AD users must not trip the guard.
	if p.Guard.Tripped {
		t.Errorf("guard tripped: %v", p.Guard.Reasons)
	}

	t.Run("member mode checks the DN", func(t *testing.T) {
		p, _ := buildGroupsOnly(t, cfg(t), world{ad: ad, target: append(target[:2:2], tGroup(10, "hpc", "local1"), gRec(10, "hpc"))})
		wantOps(t, p,
			"update-record rec:a add roleOccupant="+udn("jdoe"),
			"add-member "+gdn("hpc")+" member="+udn("jdoe"),
			"add-member "+gdn("hpc")+" memberUid=jdoe")
	})
	t.Run("without lookups the planner refuses", func(t *testing.T) {
		c := cfg(t, uidOnly)
		snap, err := source.Read(context.Background(), &source.Fake{InScopeGroups: []*model.ADObject{mkGroup(10, "hpc")}}, false)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Build(snap, &model.TargetSnapshot{}, &model.Records{}, c, Options{Groups: true, Now: now}); err == nil {
			t.Error("a groups plan without the target's users or lookups must fail, never guess")
		}
	})
}

func TestPrintGroupsOnlyHeader(t *testing.T) {
	jdoe := noNumbers(1, "jdoe")
	p, _ := buildGroupsOnly(t, cfg(t, uidOnly), world{ad: []*model.ADObject{jdoe, mkGroup(10, "hpc", jdoe.DN)}})
	var b strings.Builder
	p.Print(&b)
	for _, want := range []string{"users base not read, 1 members fetched by DN", "Target: users_base not read",
		"  members skipped: 1 not on the target (use --debug to list them)\n"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, b.String())
		}
	}
	if strings.Contains(b.String(), "Warnings") || strings.Contains(b.String(), "debug:") {
		t.Errorf("a skipped member is no warning, and debug lines go only to PrintDebug:\n%s", b.String())
	}
	var d strings.Builder
	p.PrintDebug(&d)
	if want := "debug: skip jdoe in " + gdn("hpc") + ": no entry on the target under " + people + "\n"; d.String() != want {
		t.Errorf("debug = %q, want %q", d.String(), want)
	}

	t.Run("count line omitted when zero", func(t *testing.T) {
		p, _ := buildGroupsOnly(t, cfg(t, uidOnly), world{ad: []*model.ADObject{jdoe, mkGroup(10, "hpc", jdoe.DN)},
			target: []*model.Entry{localUID(udn("jdoe"), "jdoe")}})
		var b, d strings.Builder
		p.Print(&b)
		p.PrintDebug(&d)
		if strings.Contains(b.String(), "  members skipped:") || d.Len() != 0 {
			t.Errorf("no skipped members, yet:\n%s\n%s", b.String(), d.String())
		}
	})
}

// A member that exists in AD but matches neither search filter is skipped
// like an out-of-scope object and counted as filtered, not unresolved.
func TestFilteredMembers(t *testing.T) {
	jdoe := noNumbers(1, "jdoe")
	hidden := noNumbers(2, "hidden")
	child := mkGroup(11, "child", jdoe.DN)
	g := mkGroup(10, "hpc", jdoe.DN, hidden.DN, child.DN)
	fake := &source.Fake{InScopeGroups: []*model.ADObject{g}, Others: []*model.ADObject{jdoe}, Filtered: []*model.ADObject{hidden, child}}
	snap, err := source.Read(context.Background(), fake, false)
	if err != nil {
		t.Fatal(err)
	}
	c := cfg(t, uidOnly)
	tgt, recs, err := model.Classify(containers(), model.Bases{Users: c.Target.UsersBase, Groups: c.Target.GroupsBase, State: c.Target.StateBase})
	if err != nil {
		t.Fatal(err)
	}
	tgt.ExistingUsers = model.NewUserSet()
	tgt.ExistingUsers.Add(udn("jdoe"), "jdoe")
	p, err := Build(snap, tgt, recs, c, Options{Groups: true, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	wantOps(t, p,
		"add-record rec:a seeAlso="+gdn("hpc")+" roleOccupant="+udn("jdoe"),
		"add-entry "+gdn("hpc")+" memberUid=jdoe")
	if p.Counts.FilteredMembers != 2 || p.Counts.UnresolvedMembers != 0 {
		t.Errorf("filtered %d, unresolved %d; want 2 and 0", p.Counts.FilteredMembers, p.Counts.UnresolvedMembers)
	}
}

// Rule: a uidNumber or gidNumber Dolly would write must be an integer from
// 0 to 4294967294 (uid_t and gid_t are unsigned 32-bit, and -1 is
// reserved). Otherwise the entry is ignored and listed once, like one
// missing a required attribute.
func TestRuleIDRange(t *testing.T) {
	for _, tc := range []struct {
		value string
		ok    bool
	}{
		{"0", true}, {"4294967294", true}, {"4294967295", false}, {"10000535573", false},
		{"-1", false}, {"12a", false}, {"", true}, // "" is missing, the required list's business
	} {
		got := badIDs(map[string][]string{"uidNumber": {tc.value}}, "uidNumber") == ""
		if tc.value == "" {
			got = badIDs(map[string][]string{}, "uidNumber") == ""
		}
		if got != tc.ok {
			t.Errorf("%q: valid = %v, want %v", tc.value, got, tc.ok)
		}
	}
	if badIDs(map[string][]string{"gidnumber": {"1", "2"}}, "gidNumber") == "" {
		t.Error("two gidNumber values must be invalid")
	}

	big := mkUser(1, "big")
	big.Attrs["uidNumber"] = []string{"10000535573"}
	ok := mkUser(2, "ok")
	badGrp := mkGroup(11, "badgid", ok.DN)
	badGrp.Attrs["gidNumber"] = []string{"4294967295"}
	child := mkGroup(12, "child", ok.DN)
	child.Attrs["gidNumber"] = []string{"99999999999"}
	child.InScope = false
	child.DN = "CN=child," + adOther
	hpc := mkGroup(10, "hpc", big.DN, child.DN)
	p := build(t, cfg(t), world{ad: []*model.ADObject{big, ok, badGrp, child, hpc}}, both)
	// big gets no user entry (so, having no entry on the target, it isn't
	// added to hpc either), badgid isn't created, and child isn't
	// flattened, which leaves hpc without members for now.
	wantOps(t, p,
		"add-record rec:2 seeAlso="+udn("ok"),
		"add-entry "+udn("ok")+" loginShell=/bin/bash",
	)
	wantWarning(t, p, WarnIgnored, big.DN+`: user uidNumber "10000535573" is not a valid ID (an integer from 0 to 4294967294)`)
	wantWarning(t, p, WarnIgnored, badGrp.DN+`: group gidNumber "4294967295" is not a valid ID`)
	wantWarning(t, p, WarnIgnored, child.DN+`: group gidNumber "99999999999" is not a valid ID (an integer from 0 to 4294967294); not flattened into `+hpc.DN)
	if p.Counts.IgnoredEntries != 3 {
		t.Errorf("ignored = %d, want 3 (each listed once): %v", p.Counts.IgnoredEntries, p.Warnings)
	}

	t.Run("still a member with memberUid", func(t *testing.T) {
		// A user whose uidNumber can't be written is still a valid member:
		// memberUid doesn't carry the number.
		p := build(t, cfg(t, uidOnly), world{ad: []*model.ADObject{big, mkGroup(10, "hpc", big.DN)}, target: []*model.Entry{localUID(udn("big"), "big")}}, Options{Groups: true})
		wantOps(t, p,
			"add-record rec:a seeAlso="+gdn("hpc")+" roleOccupant="+udn("big"),
			"add-entry "+gdn("hpc")+" memberUid=big")
	})
}
