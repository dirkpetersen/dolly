package planner

import (
	"fmt"
	"testing"
	"time"

	"github.com/dirkpetersen/dolly/internal/model"
)

// scaleWorld has n users, all owned, and three large groups: "gone" (n owned
// members, its AD group is gone), "half" (n owned members, half of them left
// the AD group), and "new" (an AD group of n members, not on the target).
func scaleWorld(n int) world {
	var w world
	var uids, dns, adDNs []string
	for i := 1; i <= n; i++ {
		uid := fmt.Sprintf("u%06d", i)
		u := mkUser(i, uid)
		w.ad = append(w.ad, u)
		w.target = append(w.target, tUser(i, uid), uRec(i, uid))
		uids, dns, adDNs = append(uids, uid), append(dns, udn(uid)), append(adDNs, u.DN)
	}
	big := func(gn int, cn string) *model.Entry {
		e := tGroup(gn, cn)
		e.Attrs["member"] = append([]string(nil), dns...)
		e.Attrs["memberUid"] = append([]string(nil), uids...)
		return e
	}
	owns := func(gn int, cn string) *model.Entry {
		r := &model.Record{Kind: model.GroupRecord, GUID: guid(gn), SeeAlso: gdn(cn), Occupants: append([]string(nil), dns...)}
		return r.Entry(state)
	}
	const gone, half, fresh = 1 << 30, 1<<30 + 1, 1<<30 + 2
	w.target = append(w.target, big(gone, "gone"), owns(gone, "gone"), big(half, "half"), owns(half, "half"))
	w.ad = append(w.ad, mkGroup(half, "half", adDNs[:n/2]...), mkGroup(fresh, "new", adDNs...))
	return w
}

// TestLargeGroupsScale is the regression test for a planner that was
// quadratic in group size: it ran for hours on groups of tens of thousands
// of members. The bound is generous (race detector, slow CI machines); a
// quadratic planner takes far longer at this size.
func TestLargeGroupsScale(t *testing.T) {
	const n = 20000 // a quadratic planner needs minutes here; this one under 2 s with -race
	c := cfg(t)
	// Users too, so the users are read in full: a groups-only run looks up
	// every member, and the fake's lookup is a linear scan.
	snap, tgt, recs := snapshots(t, c, scaleWorld(n), both)
	start := time.Now()
	p, err := Build(snap, tgt, recs, c, Options{Users: true, Groups: true, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 30*time.Second {
		t.Errorf("planning took %s", d)
	}
	if got, want := p.Counts.MembersRemoved, n+n/2; got != want {
		t.Errorf("MembersRemoved = %d, want %d", got, want)
	}
	if got := p.Counts.MembersAdded; got != n {
		t.Errorf("MembersAdded = %d, want %d", got, n)
	}
	kinds := map[Kind]int{}
	for _, op := range p.Ops {
		kinds[op.Kind]++
	}
	// gone: member and memberUid per member, no roleOccupant deletes (the
	// record goes); half: the same plus one roleOccupant delete each.
	if got, want := kinds[DeleteMember], 2*(n+n/2); got != want {
		t.Errorf("DeleteMember ops = %d, want %d", got, want)
	}
	if got, want := kinds[UpdateRecord], n/2; got != want {
		t.Errorf("UpdateRecord ops = %d, want %d", got, want)
	}
	if kinds[AddPlaceholder] != 1 || kinds[DeleteRecord] != 1 || kinds[AddRecord] != 1 || kinds[AddEntry] != 1 {
		t.Errorf("op kinds = %v", kinds)
	}
	last := p.Ops[len(p.Ops)-1]
	if last.Kind != DeleteRecord || last.DN != (&model.Record{Kind: model.GroupRecord, GUID: guid(1 << 30)}).DN(state) {
		t.Errorf("last op = %s", opStr(last))
	}
}

// BenchmarkLargeGroups plans the scale world (go test -bench LargeGroups
// -run '^$' ./internal/planner).
func BenchmarkLargeGroups(b *testing.B) {
	c := cfg(b)
	snap, tgt, recs := snapshots(b, c, scaleWorld(50000), both)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Build(snap, tgt, recs, c, Options{Users: true, Groups: true, Now: now}); err != nil {
			b.Fatal(err)
		}
	}
}
