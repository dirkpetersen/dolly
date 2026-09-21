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

// Rule (sync.require_member_on_target, default on): a member is added only
// if an entry with its uid exists under users_base, Dolly-owned or local.
// Otherwise it is skipped and listed once per run as missing-on-target.
func TestRequireMemberOnTarget(t *testing.T) {
	jdoe, bob := noNumbers(1, "jdoe"), noNumbers(2, "bob")
	ad := []*model.ADObject{jdoe, bob, mkGroup(10, "hpc", jdoe.DN, bob.DN), mkGroup(11, "lab", bob.DN)}
	localJdoe := &model.Entry{DN: udn("jdoe"), Attrs: map[string][]string{"objectClass": {"account", "posixAccount"}, "uid": {"jdoe"}}}

	t.Run("on, memberUid", func(t *testing.T) {
		p := build(t, cfg(t, uidOnly), world{ad: ad, target: []*model.Entry{localJdoe}}, Options{Groups: true})
		wantOps(t, p,
			"add-record rec:a seeAlso="+gdn("hpc")+" roleOccupant="+udn("jdoe"),
			"add-entry "+gdn("hpc")+" memberUid=jdoe",
			"add-record rec:b seeAlso="+gdn("lab"),
			"add-entry "+gdn("lab"))
		if w := warnings(p, WarnMissingOnTarget); len(w) != 1 || !strings.HasPrefix(w[0], "bob: no entry with this uid") {
			t.Errorf("missing-on-target = %v, want bob once", w)
		}
	})
	t.Run("on, member DN must exist", func(t *testing.T) {
		// An entry with uid bob exists, but not at the member DN.
		elsewhere := &model.Entry{DN: "uid=bob,ou=staff," + people, Attrs: map[string][]string{"uid": {"bob"}}}
		p := build(t, cfg(t), world{ad: ad, target: []*model.Entry{localJdoe, elsewhere}}, Options{Groups: true})
		wantOps(t, p,
			"add-record rec:a seeAlso="+gdn("hpc")+" roleOccupant="+udn("jdoe"),
			"add-entry "+gdn("hpc")+" member="+udn("jdoe")+" memberUid=jdoe")
		wantWarning(t, p, WarnMissingOnTarget, udn("bob")+": no entry at this DN")
		wantWarning(t, p, WarnPending, gdn("lab")+": AD group has no resolvable members")
	})
	t.Run("off", func(t *testing.T) {
		p := build(t, cfg(t, uidOnly, lax), world{ad: ad}, Options{Groups: true})
		wantOps(t, p,
			"add-record rec:a seeAlso="+gdn("hpc")+" roleOccupant="+udn("bob")+"|"+udn("jdoe"),
			"add-entry "+gdn("hpc")+" memberUid=bob|jdoe",
			"add-record rec:b seeAlso="+gdn("lab")+" roleOccupant="+udn("bob"),
			"add-entry "+gdn("lab")+" memberUid=bob")
		if len(warnings(p, WarnMissingOnTarget)) != 0 {
			t.Errorf("off must not check: %v", p.Warnings)
		}
	})
	t.Run("entries created in the same run count", func(t *testing.T) {
		a, b := mkUser(1, "jdoe"), mkUser(2, "bob")
		p := build(t, cfg(t), world{ad: []*model.ADObject{a, b, mkGroup(10, "hpc", a.DN, b.DN)}}, both)
		index(t, p, "add-entry "+gdn("hpc")+" member="+udn("bob")+"|"+udn("jdoe"))
		if len(warnings(p, WarnMissingOnTarget)) != 0 {
			t.Errorf("created users are on the target: %v", p.Warnings)
		}
	})
	t.Run("owned member whose entry vanished is kept, not re-added", func(t *testing.T) {
		p := build(t, cfg(t, uidOnly), world{ad: ad, target: []*model.Entry{localJdoe,
			tGroupUID(10, "hpc", "jdoe", "bob"), gRec(10, "hpc", "jdoe", "bob"), tGroupUID(11, "lab", "bob"), gRec(11, "lab", "bob")}}, Options{Groups: true})
		wantOps(t, p)
		wantWarning(t, p, WarnMissingOnTarget, "bob: ")
		if p.Guard.MembershipRemovals != 0 {
			t.Errorf("removals = %d", p.Guard.MembershipRemovals)
		}
	})
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
	wantWarning(t, p, WarnMissingOnTarget, "bob: no entry with this uid")
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
	for _, want := range []string{"users base not read, 1 members fetched by DN", "Target: users_base not read", "missing-on-target jdoe"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, b.String())
		}
	}
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
	c := cfg(t, uidOnly, lax)
	tgt, recs, err := model.Classify(containers(), model.Bases{Users: c.Target.UsersBase, Groups: c.Target.GroupsBase, State: c.Target.StateBase})
	if err != nil {
		t.Fatal(err)
	}
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
	// big gets no user entry (so, with require_member_on_target, it isn't
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
		p := build(t, cfg(t, uidOnly, lax), world{ad: []*model.ADObject{big, mkGroup(10, "hpc", big.DN)}}, Options{Groups: true})
		wantOps(t, p,
			"add-record rec:a seeAlso="+gdn("hpc")+" roleOccupant="+udn("big"),
			"add-entry "+gdn("hpc")+" memberUid=big")
	})
}
