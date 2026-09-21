package planner

import (
	"fmt"
	"sort"
	"strings"

	"github.com/dirkpetersen/dolly/internal/config"
	"github.com/dirkpetersen/dolly/internal/model"
)

func (p *planner) planGroups() {
	for _, g := range p.sortedGroups() {
		p.syncGroup(g)
	}
	// Groups are never deleted. A group gone from AD loses its Dolly-owned
	// members and its record; the entry and its local members stay.
	for _, guid := range model.SortedKeys(p.groupRecs) {
		if rec := p.groupRecs[guid]; rec != nil && p.groups[guid] == nil {
			p.goneGroup(rec)
		}
	}
}

// userDN is the DN a managed user has on the target: its record's seeAlso
// if Dolly owns it (so a groups-only run follows the target, not a rename
// the users phase hasn't applied yet), otherwise the DN it would get.
func (p *planner) userDN(u *adUser) string {
	if rec := p.userRecs[u.obj.GUID]; rec != nil {
		return rec.SeeAlso
	}
	return u.dn
}

// notOnTarget reports whether a groups-only run in member mode must skip u:
// the user has no ownership record and no entry on the target, so a member
// value would be a dangling DN. With memberUid only, members are bare uids
// and need no user entry. The skip is reported once per user as pending.
func (p *planner) notOnTarget(u *adUser) bool {
	if p.opt.Users || !p.memberMode || p.userRecs[u.obj.GUID] != nil || p.userEntries[model.MustDNKey(u.dn)] != nil {
		return false
	}
	if !p.pendingUsers[u.obj.GUID] {
		p.pendingUsers[u.obj.GUID] = true
		p.warn(WarnPending, u.dn, "user not yet created; run a users sync (its group memberships are skipped until then)")
	}
	return true
}

// desired returns the members a group should have: its flattened AD
// members that are managed and enabled, sorted by DN.
func (p *planner) desired(g *adGroup) []member {
	var out []member
	for _, o := range p.groupMembers[g.obj.GUID] {
		u := p.users[o.GUID]
		if u == nil || u.disabled || p.notOnTarget(u) {
			continue
		}
		dn := p.userDN(u)
		out = append(out, member{dn: dn, uid: model.RDNValue(dn)})
	}
	sort.Slice(out, func(i, j int) bool { return model.MustDNKey(out[i].dn) < model.MustDNKey(out[j].dn) })
	return out
}

func (p *planner) syncGroup(g *adGroup) {
	guid := g.obj.GUID
	key := model.MustDNKey(g.dn)
	want := p.desired(g)
	rec := p.groupRecs[guid]
	if rec == nil {
		if e := p.groupEntries[key]; e != nil {
			p.groupConflict(e, guid)
			return
		}
		p.createGroup(g, nil, want, "new AD group")
		return
	}

	cur := p.findManaged(rec)
	if cur != nil && model.ParentKey(cur.DN) != model.ParentKey(g.dn) {
		p.conflict(cur.DN, "Dolly-owned group is not directly under groups_base "+p.cfg.Target.GroupsBase+"; skipped")
		return
	}
	if other := p.groupEntries[key]; other != nil && other != cur {
		if cur != nil {
			p.conflict(g.dn, fmt.Sprintf("AD group %s was renamed, but %s already exists on the target; skipped", cur.DN, other.DN))
		} else {
			p.groupConflict(other, guid)
		}
		return
	}
	if olds := p.oldDNs(rec, cur, g.dn); len(olds) > 0 || len(rec.NoteValues(model.NoteRenamingFrom)) > 0 {
		p.updateRecord(rec, "record the rename to "+g.dn+" before applying it", func(r *model.Record) {
			r.SeeAlso = g.dn
			for _, o := range olds {
				r.AddNote(model.NoteRenamingFrom, o)
			}
		})
		if cur != nil && !model.DNExact(cur.DN, g.dn) {
			why := "name changed in AD"
			if model.DNEqual(cur.DN, g.dn) {
				why = "name case changed in AD"
			}
			p.modRDN(SectionGroups, p.groupEntries, cur, g.dn, why)
			p.plan.Counts.GroupsRenamed++
		}
		p.updateRecord(rec, "rename complete", func(r *model.Record) { r.DelNote(model.NoteRenamingFrom) })
	}
	if cur == nil {
		p.createGroup(g, rec, want, "Dolly-owned group is missing on the target; re-creating it")
		return
	}
	p.updateGroupAttrs(g, cur)
	p.syncMembers(cur, rec, want)
}

func (p *planner) groupConflict(e *model.Entry, guid string) {
	if owner := p.groupRecBySee[model.MustDNKey(e.DN)]; owner != "" && owner != guid {
		p.conflict(e.DN, fmt.Sprintf("group is owned by AD object %s, not %s; skipped", owner, guid))
		return
	}
	p.conflict(e.DN, "group exists on the target without an ownership record; skipped (run `dolly adopt` to claim existing entries)")
}

// createGroup adds a group with its initial members. The record, with a
// roleOccupant for every member, is written before the entry. With member
// in the membership list, a group with no members isn't created yet.
func (p *planner) createGroup(g *adGroup, rec *model.Record, want []member, reason string) {
	if p.memberMode && len(want) == 0 {
		p.warn(WarnPending, g.dn, "AD group has no resolvable members; groupOfNames needs one, so it isn't created yet")
		if rec != nil {
			for _, o := range append([]string(nil), rec.Occupants...) {
				p.recDelOccupant(rec, o, "group isn't on the target and the member is no longer in the AD group")
			}
		}
		return
	}
	e := &model.Entry{DN: g.dn, Attrs: map[string][]string{}}
	e.Set("objectClass", p.cfg.Mapping.Groups.ObjectClasses)
	for _, name := range p.groupAttrs {
		e.Set(name, g.attrs[name])
	}
	var dns, uids []string
	for _, m := range want {
		dns = append(dns, m.dn)
		uids = append(uids, m.uid)
	}
	sort.Strings(uids)
	if p.memberMode {
		e.Set(config.AttrMember, dns)
	}
	if p.uidMode {
		e.Set(config.AttrMemberUID, uids)
	}
	if rec == nil {
		rec = &model.Record{Kind: model.GroupRecord, GUID: g.obj.GUID, SeeAlso: g.dn, Occupants: dns}
		p.addRecord(rec, fmt.Sprintf("record ownership of %s and its %d members before creating it", g.dn, len(dns)))
	} else {
		for _, m := range want {
			if occupant(rec, m.dn) == "" {
				p.recAddOccupant(rec, m.dn, "record ownership before re-creating the group with "+m.uid)
			}
		}
	}
	p.addEntry(SectionGroups, p.groupEntries, e, fmt.Sprintf("%s with %d members", reason, len(dns)))
	p.plan.Counts.GroupsAdded++
	for _, m := range want {
		p.countAdd(g.dn, m.dn)
	}
	wantKeys := memberKeys(want)
	for _, o := range append([]string(nil), rec.Occupants...) {
		if !wantKeys[model.MustDNKey(o)] {
			p.recDelOccupant(rec, o, "no longer in the AD group")
		}
	}
}

func (p *planner) updateGroupAttrs(g *adGroup, cur *model.Entry) {
	var changes []Change
	var names []string
	for _, name := range p.groupAttrs {
		want, have := g.attrs[name], cur.Get(name)
		if sameValues(want, have) {
			continue
		}
		changes = append(changes, Change{Replace, name, want})
		names = append(names, name)
		if strings.EqualFold(name, "gidNumber") {
			p.warn(WarnIDChanged, cur.DN, fmt.Sprintf("gidNumber changes from %s to %s", strings.Join(have, ","), strings.Join(want, ",")))
		}
	}
	if len(changes) > 0 {
		p.modifyEntry(SectionGroups, cur, changes, "changed in AD: "+strings.Join(names, ", "))
		p.plan.Counts.GroupsModified++
	}
}

// syncMembers adds members newly in the AD group and removes Dolly-owned
// members that left it. A member already present without a roleOccupant is
// local: it is never removed and never claimed, even if it is also in AD.
func (p *planner) syncMembers(g *model.Entry, rec *model.Record, want []member) {
	// Index the current values once; each wanted member is looked up once
	// and additions never affect another member's lookup.
	occ, mem, uids := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, o := range rec.Occupants {
		occ[model.MustDNKey(o)] = true
	}
	for _, v := range g.Get(config.AttrMember) {
		mem[model.MustDNKey(v)] = true
	}
	for _, v := range g.Get(config.AttrMemberUID) {
		uids[v] = true
	}
	for _, m := range want {
		k := model.MustDNKey(m.dn)
		hasM := p.memberMode && mem[k]
		hasU := p.uidMode && uids[m.uid]
		if occ[k] {
			// Owned: restore values that went missing (e.g. after a crash
			// between writing the record and adding the member).
			if p.memberMode && !hasM {
				p.addMember(g, config.AttrMember, m.dn, "Dolly-owned member "+m.uid+" is missing its member value")
			}
			if p.uidMode && !hasU {
				p.addMember(g, config.AttrMemberUID, m.uid, "Dolly-owned member "+m.uid+" is missing its memberUid value")
			}
			continue
		}
		if hasM || hasU {
			continue // local member
		}
		p.recAddOccupant(rec, m.dn, "record ownership before adding "+m.uid)
		if p.memberMode {
			p.addMember(g, config.AttrMember, m.dn, m.uid+" is in the AD group")
		}
		if p.uidMode {
			p.addMember(g, config.AttrMemberUID, m.uid, m.uid+" is in the AD group")
		}
		p.countAdd(g.DN, m.dn)
	}

	wantKeys := memberKeys(want)
	for _, o := range append([]string(nil), rec.Occupants...) {
		if wantKeys[model.MustDNKey(o)] {
			continue
		}
		p.removeMembership(g, rec, o, model.RDNValue(o), false, false, p.leftWhy(o))
	}
	// After the removals, so a placeholder next to a leaving member stays
	// put instead of being deleted and added back.
	p.dropPlaceholder(g)
}

// leftWhy explains why an owned member is no longer wanted.
func (p *planner) leftWhy(dn string) string {
	if p.byUserDN == nil {
		p.byUserDN = map[string]*adUser{}
		for _, u := range p.users {
			p.byUserDN[model.MustDNKey(p.userDN(u))] = u
		}
	}
	if u := p.byUserDN[model.MustDNKey(dn)]; u != nil {
		if u.disabled {
			return u.uid + " is disabled in AD"
		}
		return u.uid + " left the AD group"
	}
	return model.RDNValue(dn) + " is gone from AD or no longer managed"
}

// goneGroup handles a group record whose AD group is gone: remove the
// Dolly-owned members, then the record. The group entry stays.
func (p *planner) goneGroup(rec *model.Record) {
	why := p.goneWhy(rec.GUID)
	if cur := p.findManaged(rec); cur != nil {
		for _, o := range append([]string(nil), rec.Occupants...) {
			p.removeMembership(cur, rec, o, model.RDNValue(o), false, true, "group gone from AD ("+why+")")
		}
	}
	p.deleteRecord(rec, "group gone from AD ("+why+"); the group entry and its local members stay")
}

func (p *planner) countAdd(group, user string) {
	k := pairKey(group, user)
	if !p.addedPairs[k] {
		p.addedPairs[k] = true
		p.plan.Counts.MembersAdded++
	}
}

func memberKeys(ms []member) map[string]bool {
	out := make(map[string]bool, len(ms))
	for _, m := range ms {
		out[model.MustDNKey(m.dn)] = true
	}
	return out
}
