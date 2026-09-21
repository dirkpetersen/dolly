package planner

import (
	"strings"
	"testing"

	"github.com/dirkpetersen/dolly/internal/config"
	"github.com/dirkpetersen/dolly/internal/model"
)

// simulate applies the dependency rules the way the applier does: the ops
// whose rendering starts with one of fail fail, and every op that needs a
// key a failed or skipped op provides is skipped. It returns the ops that
// run (including the failed ones) and the skipped ones.
func simulate(p *Plan, fail ...string) (ran, skipped []string) {
	broken := map[string]bool{}
	for _, op := range p.Ops {
		s := strings.TrimSpace(opStr(op))
		skip := false
		for _, k := range op.Needs {
			if broken[k] {
				skip = true
			}
		}
		failed := false
		for _, f := range fail {
			if strings.HasPrefix(s, f) {
				failed = true
			}
		}
		if skip || failed {
			for _, k := range op.Provides {
				broken[k] = true
			}
		}
		if skip {
			skipped = append(skipped, s)
		} else {
			ran = append(ran, s)
		}
	}
	return ran, skipped
}

func wantSkipped(t *testing.T, p *Plan, fail string, want ...string) {
	t.Helper()
	_, got := simulate(p, fail)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("after %q fails, skipped:\n  %s\nwant:\n  %s\nplan:\n  %s", fail,
			strings.Join(got, "\n  "), strings.Join(want, "\n  "), strings.Join(opStrs(p), "\n  "))
	}
}

// Record first: a failed ownership record skips the entry add, and a user
// that wasn't created joins no group (neither a new group nor an existing
// one). Other users and groups carry on.
func TestDepsRecordBeforeEntry(t *testing.T) {
	jdoe, bob := mkUser(1, "jdoe"), mkUser(2, "bob")
	w := world{
		ad:     []*model.ADObject{jdoe, bob, mkGroup(10, "hpc", jdoe.DN, bob.DN), mkGroup(11, "lab", jdoe.DN, bob.DN)},
		target: []*model.Entry{tUser(2, "bob"), uRec(2, "bob"), tGroup(11, "lab"), gRec(11, "lab")},
	}
	p := build(t, cfg(t), w, both)
	wantSkipped(t, p, "add-record rec:1",
		"add-entry "+udn("jdoe")+" loginShell=/bin/bash",
		"add-record rec:a seeAlso="+gdn("hpc")+" roleOccupant="+udn("bob")+"|"+udn("jdoe"),
		"add-entry "+gdn("hpc")+" member="+udn("bob")+"|"+udn("jdoe")+" memberUid=bob|jdoe",
		"update-record rec:b add roleOccupant="+udn("jdoe"),
		"add-member "+gdn("lab")+" member="+udn("jdoe"),
		"add-member "+gdn("lab")+" memberUid=jdoe",
	)
	// A failed group record skips only that group's entry.
	wantSkipped(t, p, "add-record rec:a", "add-entry "+gdn("hpc")+" member="+udn("bob")+"|"+udn("jdoe")+" memberUid=bob|jdoe")
	// A failed group entry add breaks nothing else.
	wantSkipped(t, p, "add-entry "+gdn("hpc"))
}

// A failed modrdn skips the rest of the rename (reference fix-ups and the
// "rename complete" note, so the next run can finish it) and the user's
// later changes, but not other users or groups.
func TestDepsRename(t *testing.T) {
	u, bob := mkUser(1, "jdoe2"), mkUser(2, "bob")
	p := build(t, cfg(t), world{
		ad:     []*model.ADObject{u, bob, mkGroup(10, "hpc", u.DN, bob.DN)},
		target: []*model.Entry{tUser(1, "jdoe"), uRec(1, "jdoe"), tUser(2, "bob"), uRec(2, "bob"), tGroup(10, "hpc", "jdoe"), gRec(10, "hpc", "jdoe")},
	}, both)
	wantSkipped(t, p, "modrdn "+udn("jdoe"),
		"rename-member "+gdn("hpc")+" member "+udn("jdoe")+" -> "+udn("jdoe2"),
		"rename-member "+gdn("hpc")+" memberUid jdoe -> jdoe2",
		"update-record rec:a delete roleOccupant="+udn("jdoe")+"; add roleOccupant="+udn("jdoe2"),
		"update-record rec:1 replace description=",
		"modify-entry "+udn("jdoe2")+" replace cn=jdoe2",
	)
	// A failed fix-up keeps the renaming-from note for the next run.
	wantSkipped(t, p, "rename-member "+gdn("hpc")+" memberUid",
		"update-record rec:a delete roleOccupant="+udn("jdoe")+"; add roleOccupant="+udn("jdoe2"),
		"update-record rec:1 replace description=",
		"modify-entry "+udn("jdoe2")+" replace cn=jdoe2",
	)
	// The record update before the modrdn failing skips the modrdn too.
	ran, _ := simulate(p, "update-record rec:1 replace seeAlso")
	for _, s := range ran {
		if strings.HasPrefix(s, "modrdn") {
			t.Errorf("modrdn ran after its record update failed")
		}
	}
}

// Members: the roleOccupant add goes before the member values, the member
// values before the roleOccupant delete. A failure in one membership
// doesn't stop the group's other memberships, and a failed attribute
// change on the group doesn't stop its members.
func TestDepsMembership(t *testing.T) {
	jdoe, bob, carol := mkUser(1, "jdoe"), mkUser(2, "bob"), mkUser(3, "carol")
	g := mkGroup(10, "hpc", jdoe.DN, bob.DN)
	g.Attrs["gidNumber"] = []string{"6000"}
	p := build(t, cfg(t), world{
		ad: []*model.ADObject{jdoe, bob, carol, g},
		target: []*model.Entry{tUser(1, "jdoe"), uRec(1, "jdoe"), tUser(2, "bob"), uRec(2, "bob"), tUser(3, "carol"), uRec(3, "carol"),
			tGroup(10, "hpc", "carol", "local1"), gRec(10, "hpc", "carol")},
	}, both)
	wantSkipped(t, p, "update-record rec:a add roleOccupant="+udn("bob"),
		"add-member "+gdn("hpc")+" member="+udn("bob"),
		"add-member "+gdn("hpc")+" memberUid=bob")
	wantSkipped(t, p, "delete-member "+gdn("hpc")+" member="+udn("carol"),
		"delete-member "+gdn("hpc")+" memberUid=carol",
		"update-record rec:a delete roleOccupant="+udn("carol"))
	wantSkipped(t, p, "delete-member "+gdn("hpc")+" memberUid=carol",
		"update-record rec:a delete roleOccupant="+udn("carol"))
	wantSkipped(t, p, "modify-entry "+gdn("hpc"))
}

// The placeholder: a failed placeholder add skips the member deletes on
// that group (the last member stays until the next run), and a failed
// member add keeps the placeholder.
func TestDepsPlaceholder(t *testing.T) {
	jdoe, bob := mkUser(1, "jdoe"), mkUser(2, "bob")
	p := build(t, cfg(t), world{
		ad:     []*model.ADObject{jdoe, bob, mkGroup(10, "hpc")},
		target: []*model.Entry{tUser(1, "jdoe"), uRec(1, "jdoe"), tGroup(10, "hpc", "jdoe"), gRec(10, "hpc", "jdoe")},
	}, both)
	wantSkipped(t, p, "add-placeholder "+gdn("hpc"),
		"delete-member "+gdn("hpc")+" member="+udn("jdoe"),
		"delete-member "+gdn("hpc")+" memberUid=jdoe",
		"update-record rec:a delete roleOccupant="+udn("jdoe"))

	p = build(t, cfg(t), world{
		ad: []*model.ADObject{jdoe, bob, mkGroup(10, "hpc", bob.DN)},
		target: []*model.Entry{tUser(1, "jdoe"), uRec(1, "jdoe"), tUser(2, "bob"), uRec(2, "bob"),
			func() *model.Entry { e := tGroup(10, "hpc"); e.AddValue("member", empty); return e }(), gRec(10, "hpc")},
	}, both)
	wantSkipped(t, p, "add-member "+gdn("hpc")+" member="+udn("bob"),
		"add-member "+gdn("hpc")+" memberUid=bob", // the rest of the membership's sequence
		"delete-placeholder "+gdn("hpc")+" member="+empty)
}

// A group gone from AD keeps its record until every owned member is gone;
// a pruned user keeps its entry and record until every membership is gone.
func TestDepsGoneAndPrune(t *testing.T) {
	jdoe := mkUser(1, "jdoe")
	p := build(t, cfg(t), world{
		ad:     []*model.ADObject{jdoe, mkGroup(11, "other", jdoe.DN)},
		target: []*model.Entry{tUser(1, "jdoe"), uRec(1, "jdoe"), tGroup(10, "hpc", "jdoe", "local1"), gRec(10, "hpc", "jdoe"), tGroup(11, "other", "jdoe"), gRec(11, "other", "jdoe")},
	}, both)
	wantSkipped(t, p, "delete-member "+gdn("hpc")+" member="+udn("jdoe"),
		"delete-member "+gdn("hpc")+" memberUid=jdoe",
		"delete-record rec:a")

	bob := mkUser(2, "bob")
	prune := func(c *config.Config) { c.Sync.PruneUsers = true; c.Sync.PruneAfterDays = 30 }
	p = build(t, cfg(t, prune), world{
		ad: []*model.ADObject{bob, mkGroup(10, "hpc", bob.DN)},
		target: []*model.Entry{tUser(1, "jdoe"), uRec(1, "jdoe", "missing-since=2024-04-01T00:00:00Z"), tUser(2, "bob"), uRec(2, "bob"),
			tGroup(10, "hpc", "bob", "jdoe"), gRec(10, "hpc", "bob", "jdoe"), tGroup(11, "lab", "jdoe", "local1")},
	}, both)
	wantSkipped(t, p, "delete-member "+gdn("lab")+" member="+udn("jdoe"),
		"delete-member "+gdn("lab")+" memberUid=jdoe",
		"delete-entry "+udn("jdoe"),
		"delete-record rec:1")
	wantSkipped(t, p, "delete-entry "+udn("jdoe"), "delete-record rec:1")
}

// A failed state container skips everything below it: the record
// containers, the records, and so the entries.
func TestDepsContainers(t *testing.T) {
	c := cfg(t)
	jdoe := mkUser(1, "jdoe")
	snap := &model.ADSnapshot{Objects: []*model.ADObject{jdoe, mkGroup(10, "hpc", jdoe.DN)}, UsersRead: true}
	tgt, recs, err := model.Classify([]*model.Entry{
		{DN: people, Attrs: map[string][]string{"objectClass": {"organizationalUnit"}}},
		{DN: groupsOU, Attrs: map[string][]string{"objectClass": {"organizationalUnit"}}},
	}, model.Bases{Users: people, Groups: groupsOU, State: state})
	if err != nil {
		t.Fatal(err)
	}
	tgt.UsersRead = true
	p, err := Build(snap, tgt, recs, c, Options{Users: true, Groups: true, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	ran, _ := simulate(p, "create-container ou=dolly")
	if strings.Join(ran, "\n") != "create-container "+state {
		t.Errorf("ran after the state container failed:\n  %s", strings.Join(ran, "\n  "))
	}
	_, skipped := simulate(p, "create-container ou=groups")
	for _, s := range skipped {
		if strings.HasPrefix(s, "add-entry "+udn("jdoe")) {
			t.Errorf("the user entry was skipped although only the group records container failed:\n  %s", strings.Join(skipped, "\n  "))
		}
	}
	if len(skipped) != 2 { // the group's record and entry
		t.Errorf("skipped after ou=groups failed:\n  %s", strings.Join(skipped, "\n  "))
	}
}

// A new AD user takes the uid of a user pruned in the same run: if the
// prune's delete fails, the add of the same DN is skipped too.
func TestDepsAddAfterFailedPrune(t *testing.T) {
	newJdoe := mkUser(3, "jdoe")
	prune := func(c *config.Config) { c.Sync.PruneUsers = true; c.Sync.PruneAfterDays = 30 }
	p := build(t, cfg(t, prune), world{
		ad:     []*model.ADObject{newJdoe},
		target: []*model.Entry{tUser(1, "jdoe"), uRec(1, "jdoe", "missing-since=2024-04-01T00:00:00Z")},
	}, both)
	t.Logf("plan:\n  %s", strings.Join(opStrs(p), "\n  "))
	_, skipped := simulate(p, "delete-entry "+udn("jdoe"))
	var addSkipped bool
	for _, s := range skipped {
		if strings.HasPrefix(s, "add-entry "+udn("jdoe")) {
			addSkipped = true
		}
	}
	if !addSkipped {
		t.Errorf("add of %s ran after its prune failed; skipped: %v", udn("jdoe"), skipped)
	}
}
