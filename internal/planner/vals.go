package planner

import (
	"sort"

	"github.com/dirkpetersen/dolly/internal/config"
	"github.com/dirkpetersen/dolly/internal/model"
)

// vals is an ordered list of attribute values (or roleOccupant values) with
// an index by key, so a lookup or a delete costs O(1) instead of a scan of
// the group. A delete leaves a tombstone until values() compacts the list,
// which keeps the order the plain slice operations had.
type vals struct {
	list []string
	dead []bool
	live int
	pos  map[string][]int // key -> ascending positions of the live values
	key  func(string) string
}

func newVals(vs []string, key func(string) string) *vals {
	x := &vals{key: key}
	x.reset(vs)
	return x
}

func (x *vals) reset(vs []string) {
	x.list, x.dead, x.live = append([]string(nil), vs...), make([]bool, len(vs)), len(vs)
	x.pos = make(map[string][]int, len(vs))
	for i, v := range vs {
		k := x.key(v)
		x.pos[k] = append(x.pos[k], i)
	}
}

// first returns the first live value with key k, or "".
func (x *vals) first(k string) string {
	if ps := x.pos[k]; len(ps) > 0 {
		return x.list[ps[0]]
	}
	return ""
}

// all returns the live values with key k, in order.
func (x *vals) all(k string) []string {
	var out []string
	for _, i := range x.pos[k] {
		out = append(out, x.list[i])
	}
	return out
}

func (x *vals) add(v string) {
	k := x.key(v)
	x.pos[k] = append(x.pos[k], len(x.list))
	x.list, x.dead = append(x.list, v), append(x.dead, false)
	x.live++
}

// del removes every live value equal to v.
func (x *vals) del(v string) {
	k := x.key(v)
	var keep []int
	for _, i := range x.pos[k] {
		if x.list[i] == v {
			x.dead[i] = true
			x.live--
		} else {
			keep = append(keep, i)
		}
	}
	x.setPos(k, keep)
}

// replace changes every live value equal to old to v, in place.
func (x *vals) replace(old, v string) {
	ok, nk := x.key(old), x.key(v)
	var keep, moved []int
	for _, i := range x.pos[ok] {
		if x.list[i] == old {
			x.list[i] = v
			moved = append(moved, i)
		} else {
			keep = append(keep, i)
		}
	}
	x.setPos(ok, keep)
	ps := append(x.pos[nk], moved...)
	sort.Ints(ps)
	x.setPos(nk, ps)
}

func (x *vals) setPos(k string, ps []int) {
	if len(ps) == 0 {
		delete(x.pos, k)
	} else {
		x.pos[k] = ps
	}
}

// values returns the live values in order.
func (x *vals) values() []string {
	if x.live < len(x.list) {
		live := make([]string, 0, x.live)
		for i, v := range x.list {
			if !x.dead[i] {
				live = append(live, v)
			}
		}
		x.reset(live)
	}
	return append([]string(nil), x.list...)
}

// lookupDN returns the first live value DN-equal to dn, or "". An invalid
// DN matches nothing, as with model.DNEqual.
func (x *vals) lookupDN(dn string) string {
	k, err := model.DNKey(dn)
	if err != nil {
		return ""
	}
	return x.first(k)
}

func exactKey(v string) string { return v }

type valsKey struct {
	e    *model.Entry
	attr string
}

// attrVals returns the index of a group's member or memberUid values, built
// on first use. From then on it, not the entry, holds the current values:
// read them through memberValues.
func (p *planner) attrVals(g *model.Entry, attr string) *vals {
	k := valsKey{g, attr}
	if x := p.attrIdx[k]; x != nil {
		return x
	}
	key := exactKey
	if attr == config.AttrMember {
		key = model.MustDNKey
	}
	x := newVals(g.Get(attr), key)
	p.attrIdx[k] = x
	return x
}

// memberValues returns the current values of a membership attribute and
// writes them back to the entry.
func (p *planner) memberValues(g *model.Entry, attr string) []string {
	if x := p.attrIdx[valsKey{g, attr}]; x != nil {
		g.Set(attr, x.values())
	}
	return g.Get(attr)
}

// occVals returns the index of a record's roleOccupant values, built on
// first use. From then on it, not r.Occupants, holds the current values:
// read them through occupants.
func (p *planner) occVals(r *model.Record) *vals {
	if x := p.occIdx[r]; x != nil {
		return x
	}
	x := newVals(r.Occupants, model.MustDNKey)
	p.occIdx[r] = x
	return x
}

// occupants returns the current roleOccupant values of r and writes them
// back to r.Occupants.
func (p *planner) occupants(r *model.Record) []string {
	if x := p.occIdx[r]; x != nil {
		r.Occupants = x.values()
	}
	return r.Occupants
}
