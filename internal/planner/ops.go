package planner

import (
	"sort"
	"strings"

	"github.com/dirkpetersen/dolly/internal/config"
	"github.com/dirkpetersen/dolly/internal/model"
)

// Every helper here appends one operation to the plan and applies it to the
// planner's working copy of the target, so later decisions see its effect.

func (p *planner) emit(op Op) {
	p.deps(&op)
	p.plan.Ops = append(p.plan.Ops, op)
}

// entryAttrs renders an entry as an ordered attribute list, objectClass first.
func entryAttrs(e *model.Entry) []Attribute {
	var out []Attribute
	if oc := e.Get("objectClass"); len(oc) > 0 {
		out = append(out, Attribute{"objectClass", oc})
	}
	for _, k := range model.SortedKeys(e.Attrs) {
		if strings.EqualFold(k, "objectClass") {
			continue
		}
		out = append(out, Attribute{k, append([]string(nil), e.Attrs[k]...)})
	}
	return out
}

func (p *planner) addEntry(sec Section, entries map[string]*model.Entry, e *model.Entry, reason string) {
	p.emit(Op{Kind: AddEntry, Section: sec, DN: e.DN, Attrs: entryAttrs(e), Reason: reason})
	entries[model.MustDNKey(e.DN)] = e
}

func (p *planner) modifyEntry(sec Section, e *model.Entry, changes []Change, reason string) {
	p.emit(Op{Kind: ModifyEntry, Section: sec, DN: e.DN, Changes: changes, Reason: reason})
	for _, c := range changes {
		e.Set(c.Attr, c.Values)
	}
}

// modRDN renames e in place to newDN (same parent, deleteoldrdn).
func (p *planner) modRDN(sec Section, entries map[string]*model.Entry, e *model.Entry, newDN, reason string) {
	attr, oldVal, _ := model.RDN(e.DN)
	_, newVal, _ := model.RDN(newDN)
	p.emit(Op{Kind: ModRDN, Section: sec, DN: e.DN, NewRDN: model.RDNString(attr, newVal), Reason: reason})
	delete(entries, model.MustDNKey(e.DN))
	e.DeleteValue(attr, oldVal, strings.EqualFold)
	e.AddValue(attr, newVal)
	e.DN = newDN
	entries[model.MustDNKey(newDN)] = e
}

func (p *planner) deleteEntry(sec Section, entries map[string]*model.Entry, e *model.Entry, reason string) {
	p.emit(Op{Kind: DeleteEntry, Section: sec, DN: e.DN, Reason: reason})
	delete(entries, model.MustDNKey(e.DN))
}

func (p *planner) addMember(g *model.Entry, attr, value, reason string) {
	p.emit(Op{Kind: AddMember, Section: SectionMemberships, DN: g.DN, Attr: attr, Value: value, Reason: reason})
	g.AddValue(attr, value)
}

func (p *planner) deleteMember(g *model.Entry, attr, value, reason string) {
	p.emit(Op{Kind: DeleteMember, Section: SectionMemberships, DN: g.DN, Attr: attr, Value: value, Reason: reason})
	g.DeleteValue(attr, value, exactEq)
}

func (p *planner) renameMember(g *model.Entry, attr, old, value, reason string) {
	p.emit(Op{Kind: RenameMember, Section: SectionMemberships, DN: g.DN, Attr: attr, Old: old, Value: value, Reason: reason})
	g.DeleteValue(attr, old, exactEq)
	g.AddValue(attr, value)
	p.plan.Counts.MembersRenamed++
}

// realMembers counts member values other than the placeholder.
func (p *planner) realMembers(g *model.Entry) int {
	n := 0
	for _, v := range g.Get(config.AttrMember) {
		if !model.DNEqual(v, p.placeholder) {
			n++
		}
	}
	return n
}

// ensurePlaceholder adds empty_group_member before the last real member of
// a groupOfNames group is removed.
func (p *planner) ensurePlaceholder(g *model.Entry) {
	if !p.memberMode || p.realMembers(g) != 1 || findDN(g, config.AttrMember, p.placeholder) != "" {
		return
	}
	p.emit(Op{Kind: AddPlaceholder, Section: SectionMemberships, DN: g.DN, Attr: config.AttrMember, Value: p.placeholder,
		Reason: "groupOfNames needs a member; placeholder added before the last real member is removed"})
	g.AddValue(config.AttrMember, p.placeholder)
}

// dropPlaceholder removes empty_group_member once a real member exists.
func (p *planner) dropPlaceholder(g *model.Entry) {
	if !p.memberMode || p.realMembers(g) == 0 {
		return
	}
	if v := findDN(g, config.AttrMember, p.placeholder); v != "" {
		p.emit(Op{Kind: DeletePlaceholder, Section: SectionMemberships, DN: g.DN, Attr: config.AttrMember, Value: v,
			Reason: "the group has a real member again"})
		g.DeleteValue(config.AttrMember, v, exactEq)
	}
}

// --- ownership records ---

func (p *planner) recIndex(k model.RecordKind) (map[string]*model.Record, map[string]string) {
	if k == model.GroupRecord {
		return p.groupRecs, p.groupRecBySee
	}
	return p.userRecs, p.userRecBySee
}

func (p *planner) addRecord(r *model.Record, reason string) {
	sort.Strings(r.Notes)
	e := r.Entry(p.cfg.Target.StateBase)
	p.emit(Op{Kind: AddRecord, Section: SectionRecords, DN: e.DN, Attrs: entryAttrs(e), Reason: reason})
	recs, bySee := p.recIndex(r.Kind)
	recs[r.GUID] = r
	bySee[model.MustDNKey(r.SeeAlso)] = r.GUID
	p.plan.Counts.RecordsAdded++
}

// updateRecord applies mutate to r and plans replace operations for a
// changed seeAlso or description. roleOccupant changes use the occupant
// helpers so they stay single-value.
func (p *planner) updateRecord(r *model.Record, reason string, mutate func(*model.Record)) {
	before := r.Clone()
	mutate(r)
	sort.Strings(r.Notes)
	var ch []Change
	if r.SeeAlso != before.SeeAlso {
		ch = append(ch, Change{Replace, "seeAlso", []string{r.SeeAlso}})
		_, bySee := p.recIndex(r.Kind)
		if bySee[model.MustDNKey(before.SeeAlso)] == r.GUID {
			delete(bySee, model.MustDNKey(before.SeeAlso))
		}
		bySee[model.MustDNKey(r.SeeAlso)] = r.GUID
	}
	if !sameValues(r.Notes, before.Notes) {
		ch = append(ch, Change{Replace, "description", append([]string(nil), r.Notes...)})
	}
	if len(ch) == 0 {
		return
	}
	p.emit(Op{Kind: UpdateRecord, Section: SectionRecords, DN: r.DN(p.cfg.Target.StateBase), Changes: ch, Reason: reason})
	p.plan.Counts.RecordsUpdated++
}

func (p *planner) recAddOccupant(r *model.Record, dn, reason string) {
	p.emit(Op{Kind: UpdateRecord, Section: SectionRecords, DN: r.DN(p.cfg.Target.StateBase),
		Changes: []Change{{Add, "roleOccupant", []string{dn}}}, Reason: reason})
	r.Occupants = append(r.Occupants, dn)
	p.plan.Counts.RecordsUpdated++
}

func (p *planner) recDelOccupant(r *model.Record, stored, reason string) {
	p.emit(Op{Kind: UpdateRecord, Section: SectionRecords, DN: r.DN(p.cfg.Target.StateBase),
		Changes: []Change{{Delete, "roleOccupant", []string{stored}}}, Reason: reason})
	var keep []string
	for _, o := range r.Occupants {
		if o != stored {
			keep = append(keep, o)
		}
	}
	r.Occupants = keep
	p.plan.Counts.RecordsUpdated++
}

// recRenameOccupant replaces one roleOccupant value in a single atomic modify.
func (p *planner) recRenameOccupant(r *model.Record, stored, dn, reason string) {
	p.emit(Op{Kind: UpdateRecord, Section: SectionRecords, DN: r.DN(p.cfg.Target.StateBase),
		Changes: []Change{{Delete, "roleOccupant", []string{stored}}, {Add, "roleOccupant", []string{dn}}}, Reason: reason})
	for i, o := range r.Occupants {
		if o == stored {
			r.Occupants[i] = dn
		}
	}
	p.plan.Counts.RecordsUpdated++
}

func (p *planner) deleteRecord(r *model.Record, reason string) {
	p.emit(Op{Kind: DeleteRecord, Section: SectionRecords, DN: r.DN(p.cfg.Target.StateBase), Reason: reason})
	recs, bySee := p.recIndex(r.Kind)
	delete(recs, r.GUID)
	if bySee[model.MustDNKey(r.SeeAlso)] == r.GUID {
		delete(bySee, model.MustDNKey(r.SeeAlso))
	}
	p.plan.Counts.RecordsDeleted++
}

// occupant returns the stored roleOccupant value equal to dn, or "".
func occupant(r *model.Record, dn string) string {
	if r == nil {
		return ""
	}
	for _, o := range r.Occupants {
		if model.DNEqual(o, dn) {
			return o
		}
	}
	return ""
}

// removeMembership removes one user (by DN and uid) from group g in
// crash-safe order: placeholder if needed, member values, then the
// roleOccupant. Local memberships (no roleOccupant) are removed only when
// includeLocal is set (pruning). With keepOccupant the roleOccupant is left
// for a following record delete.
//
// The operations form the membership's sequence (pair key), so a failed
// member delete skips the roleOccupant delete: the member stays owned and is
// removed on the next run instead of turning into a local member.
func (p *planner) removeMembership(g *model.Entry, rec *model.Record, dn, uid string, includeLocal, keepOccupant bool, reason string) {
	p.in(func() { p.removeMembershipOps(g, rec, dn, uid, includeLocal, keepOccupant, reason) }, pairDepKey(g.DN, dn))
}

func (p *planner) removeMembershipOps(g *model.Entry, rec *model.Record, dn, uid string, includeLocal, keepOccupant bool, reason string) {
	occ := occupant(rec, dn)
	if occ == "" && !includeLocal {
		return
	}
	var mv, uv string
	if p.memberMode {
		mv = findDN(g, config.AttrMember, dn)
	}
	if p.uidMode && uid != "" {
		uv = findExact(g, config.AttrMemberUID, uid)
	}
	if occ == "" {
		reason += " (local member, removed because the user is pruned)"
	}
	if mv != "" {
		p.ensurePlaceholder(g)
		p.deleteMember(g, config.AttrMember, mv, reason)
	}
	if uv != "" {
		p.deleteMember(g, config.AttrMemberUID, uv, reason)
	}
	if mv != "" || uv != "" {
		// Owned and local removals are counted apart: the guard compares
		// only owned removals with what Dolly owns.
		k, pairs := pairKey(g.DN, dn), p.removedPairs
		if occ == "" {
			pairs = p.localRemovedPairs
		}
		if !pairs[k] {
			pairs[k] = true
			p.plan.Counts.MembersRemoved++
		}
	}
	if occ != "" && !keepOccupant {
		p.recDelOccupant(rec, occ, "after removing the member: "+reason)
	}
}
