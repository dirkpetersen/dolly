package planner

import (
	"fmt"
	"strings"
	"time"

	"github.com/dirkpetersen/dolly/internal/config"
	"github.com/dirkpetersen/dolly/internal/model"
)

// spec: a users-only run changes group values only to keep them valid
// (rename fix-ups and prune). Adding and removing owned memberships,
// including those of disabled or vanished users, happens in any run that
// includes groups.

func (p *planner) planUsers() {
	// Users gone from AD first, so a pruned entry frees its name for an AD
	// user created or renamed in the same run.
	// Each user's operations form one crash-safe sequence (see deps.go).
	for _, guid := range model.SortedKeys(p.userRecs) {
		if rec := p.userRecs[guid]; rec != nil && p.users[guid] == nil {
			p.in(func() { p.goneUser(rec) }, p.userSeq(guid))
		}
	}
	for _, u := range p.sortedUsers() {
		p.in(func() { p.syncUser(u) }, p.userSeq(u.obj.GUID))
	}
}

// goneWhy explains why a GUID with a record is not a managed user.
func (p *planner) goneWhy(guid string) string {
	if why := p.ignoredWhy[guid]; why != "" {
		return why
	}
	return "deleted from AD or out of scope"
}

// goneUser handles a user record whose AD user is gone: start the prune
// clock, and prune once prune_after_days have passed.
func (p *planner) goneUser(rec *model.Record) {
	why := p.goneWhy(rec.GUID)
	since, ok := rec.MissingSince()
	if !ok {
		since = p.opt.Now
		p.updateRecord(rec, "user gone from AD ("+why+"); starting the prune clock", func(r *model.Record) {
			r.SetNote(model.NoteMissingSince, p.opt.Now.UTC().Format(time.RFC3339))
		})
		p.plan.Counts.UsersMissing++
	}
	days := p.cfg.Sync.PruneAfterDays
	if !p.cfg.Sync.PruneUsers || p.opt.Now.Sub(since) < time.Duration(days)*24*time.Hour {
		return
	}
	p.pruneUser(rec, fmt.Sprintf("gone from AD (%s) since %s, longer than prune_after_days (%d)", why, since.UTC().Format("2006-01-02"), days))
}

// pruneUser deletes a managed user: every membership (owned and local, so
// no group points at a missing entry), then the entry, then the record.
func (p *planner) pruneUser(rec *model.Record, reason string) {
	cur := p.findManaged(rec)
	dns := []string{rec.SeeAlso}
	if cur != nil && !model.DNEqual(cur.DN, rec.SeeAlso) {
		dns = append(dns, cur.DN)
	}
	for _, dn := range dns {
		uid := model.RDNValue(dn)
		for _, gk := range model.SortedKeys(p.groupEntries) {
			g := p.groupEntries[gk]
			grec := p.groupRecs[p.groupRecBySee[gk]]
			p.removeMembership(g, grec, dn, uid, true, false, "user pruned: "+reason)
		}
		// Records of groups whose entry no longer exists.
		for _, guid := range model.SortedKeys(p.groupRecs) {
			grec := p.groupRecs[guid]
			if p.findManaged(grec) != nil {
				continue
			}
			if occ := occupant(grec, dn); occ != "" {
				p.recDelOccupant(grec, occ, "user pruned; group entry is missing")
			}
		}
	}
	if cur != nil {
		p.deleteEntry(SectionUsers, p.userEntries, cur, reason)
		p.plan.Counts.UsersDeleted++
	}
	p.plan.Guard.UserRemovals++
	p.deleteRecord(rec, "after deleting the user entry")
}

func (p *planner) syncUser(u *adUser) {
	guid := u.obj.GUID
	key := model.MustDNKey(u.dn)
	rec := p.userRecs[guid]
	if rec == nil {
		if e := p.userEntries[key]; e != nil {
			p.userConflict(e, guid)
			return
		}
		p.createUser(u, nil, "new AD user")
		return
	}

	cur := p.findManaged(rec)
	if cur != nil && model.ParentKey(cur.DN) != model.ParentKey(u.dn) {
		// spec: Dolly renames in place (modrdn without newSuperior); it never moves entries.
		p.conflict(cur.DN, "Dolly-owned entry is not directly under users_base "+p.cfg.Target.UsersBase+"; skipped")
		return
	}
	if other := p.userEntries[key]; other != nil && other != cur {
		if cur != nil {
			p.conflict(u.dn, fmt.Sprintf("AD user %s was renamed, but %s already exists on the target; skipped", cur.DN, other.DN))
		} else {
			p.userConflict(other, guid)
		}
		return
	}
	if olds := p.oldDNs(rec, cur, u.dn); len(olds) > 0 || len(rec.NoteValues(model.NoteRenamingFrom)) > 0 {
		p.renameUser(u, rec, cur, olds)
	}
	if cur == nil {
		p.createUser(u, rec, "Dolly-owned entry is missing on the target; re-creating it")
		return
	}
	p.updateUser(u, rec, cur)
}

// userConflict reports a target entry that matches an AD user by name but
// isn't owned by that user's record.
func (p *planner) userConflict(e *model.Entry, guid string) {
	if owner := p.userRecBySee[model.MustDNKey(e.DN)]; owner != "" && owner != guid {
		p.conflict(e.DN, fmt.Sprintf("entry is owned by AD object %s, not %s; skipped", owner, guid))
		return
	}
	p.conflict(e.DN, "entry exists on the target without an ownership record; skipped (run `dolly adopt` to claim existing entries)")
}

// renameUser renames a managed user in crash-safe order: record the new DN
// and the old one (renaming-from), modrdn, update member and memberUid
// values in every group and roleOccupant values in group records, then
// drop the renaming-from note. A crash at any point leaves enough in the
// record for the next run to finish the rename.
func (p *planner) renameUser(u *adUser, rec *model.Record, cur *model.Entry, olds []string) {
	p.updateRecord(rec, "record the rename to "+u.dn+" before applying it", func(r *model.Record) {
		r.SeeAlso = u.dn
		for _, o := range olds {
			r.AddNote(model.NoteRenamingFrom, o)
		}
	})
	if cur != nil && !model.DNExact(cur.DN, u.dn) {
		why := "uid changed in AD"
		if model.DNEqual(cur.DN, u.dn) {
			why = "uid case changed in AD"
		}
		p.modRDN(SectionUsers, p.userEntries, cur, u.dn, why)
		p.plan.Counts.UsersRenamed++
	}
	for _, o := range olds {
		p.fixUserRefs(o, u.dn)
	}
	p.updateRecord(rec, "rename complete", func(r *model.Record) { r.DelNote(model.NoteRenamingFrom) })
}

// fixUserRefs points every group value and roleOccupant that names old at
// newDN. Local memberships are renamed too, since the entry moved.
func (p *planner) fixUserRefs(old, newDN string) {
	oldUID, newUID := model.RDNValue(old), model.RDNValue(newDN)
	caseOnly := model.DNEqual(old, newDN)
	reason := "user renamed from " + old
	for _, gk := range model.SortedKeys(p.groupEntries) {
		g := p.groupEntries[gk]
		if p.memberMode {
			for _, v := range g.Get(config.AttrMember) {
				if !model.DNEqual(v, old) || model.DNExact(v, newDN) {
					continue
				}
				if !caseOnly && findDN(g, config.AttrMember, newDN) != "" {
					p.deleteMember(g, config.AttrMember, v, reason+"; new DN already present")
				} else {
					p.renameMember(g, config.AttrMember, v, newDN, reason)
				}
			}
		}
		if p.uidMode && oldUID != newUID && findExact(g, config.AttrMemberUID, oldUID) != "" {
			if findExact(g, config.AttrMemberUID, newUID) != "" {
				p.deleteMember(g, config.AttrMemberUID, oldUID, reason+"; new uid already present")
			} else {
				p.renameMember(g, config.AttrMemberUID, oldUID, newUID, reason)
			}
		}
	}
	for _, guid := range model.SortedKeys(p.groupRecs) {
		grec := p.groupRecs[guid]
		for _, o := range append([]string(nil), grec.Occupants...) {
			if !model.DNEqual(o, old) || model.DNExact(o, newDN) {
				continue
			}
			if !caseOnly && grec.HasOccupant(newDN) {
				p.recDelOccupant(grec, o, reason+"; new DN already recorded")
			} else {
				p.recRenameOccupant(grec, o, newDN, reason)
			}
		}
	}
}

// createUser adds a user entry, writing the ownership record first.
func (p *planner) createUser(u *adUser, rec *model.Record, reason string) {
	e := &model.Entry{DN: u.dn, Attrs: map[string][]string{}}
	e.Set("objectClass", p.cfg.Mapping.Users.ObjectClasses)
	for _, name := range p.userAttrs {
		e.Set(name, u.attrs[name])
	}
	ds := p.cfg.Sync.DisabledShell
	disable := u.disabled && ds != ""
	saved := first(e.Get("loginShell"))
	if disable {
		e.Set("loginShell", []string{ds})
		reason += "; account disabled in AD, loginShell set to " + ds
	}
	note := func(r *model.Record) {
		r.SeeAlso = u.dn
		r.DelNote(model.NoteMissingSince)
		if disable {
			if _, ok := r.Note(model.NoteSavedShell); !ok {
				r.SetNote(model.NoteSavedShell, saved)
			}
		} else {
			r.DelNote(model.NoteSavedShell)
		}
	}
	if rec == nil {
		rec = &model.Record{Kind: model.UserRecord, GUID: u.obj.GUID}
		note(rec)
		p.addRecord(rec, "record ownership before creating "+u.dn)
	} else {
		p.updateRecord(rec, "update the record before re-creating "+u.dn, note)
	}
	p.addEntry(SectionUsers, p.userEntries, e, reason)
	p.plan.Counts.UsersAdded++
	if disable {
		p.plan.Counts.UsersDisabled++
	}
}

// updateUser plans attribute changes for an existing managed user.
func (p *planner) updateUser(u *adUser, rec *model.Record, cur *model.Entry) {
	var changes []Change
	var changed []string
	for _, name := range p.userAttrs {
		if p.cfg.Mapping.Users.IsCreateOnly(name) {
			continue
		}
		want, have := u.attrs[name], cur.Get(name)
		if sameValues(want, have) {
			continue
		}
		changes = append(changes, Change{Replace, name, want})
		changed = append(changed, name)
		if strings.EqualFold(name, "uidNumber") || strings.EqualFold(name, "gidNumber") {
			p.warn(WarnIDChanged, cur.DN, fmt.Sprintf("%s changes from %s to %s", name, strings.Join(have, ","), strings.Join(want, ",")))
		}
	}
	shellWhy := ""

	var pre, post []func(*model.Record)
	var preWhy, postWhy []string
	if _, ok := rec.Note(model.NoteMissingSince); ok {
		pre = append(pre, func(r *model.Record) { r.DelNote(model.NoteMissingSince) })
		preWhy = append(preWhy, "user is back in AD")
	}

	ds := p.cfg.Sync.DisabledShell
	have := cur.Get("loginShell")
	setShell := func(vals []string, why string) {
		changes, changed = dropChange(changes, changed, "loginShell")
		if !sameValues(vals, have) {
			changes = append(changes, Change{Replace, "loginShell", vals})
			shellWhy = why
		}
	}
	savedShell, hasSaved := rec.Note(model.NoteSavedShell)
	switch {
	case u.disabled && ds != "":
		// disabled_shell overrides create_only: an SSH key would still work.
		if !(len(have) == 1 && have[0] == ds) {
			if !hasSaved {
				prev := first(have)
				pre = append(pre, func(r *model.Record) { r.SetNote(model.NoteSavedShell, prev) })
				preWhy = append(preWhy, "save loginShell before disabling")
			}
			setShell([]string{ds}, "account disabled in AD, loginShell set to "+ds)
			p.plan.Counts.UsersDisabled++
		} else {
			changes, changed = dropChange(changes, changed, "loginShell")
		}
	case !u.disabled && hasSaved:
		// spec: restore only if the shell is still disabled_shell; if an
		// admin changed it meanwhile, local wins and the note is dropped.
		if ds != "" && len(have) == 1 && have[0] == ds {
			var vals []string
			if savedShell != "" {
				vals = []string{savedShell}
			}
			setShell(vals, "account re-enabled in AD, loginShell restored")
			p.plan.Counts.UsersReenabled++
		}
		post = append(post, func(r *model.Record) { r.DelNote(model.NoteSavedShell) })
		postWhy = append(postWhy, "saved shell no longer needed")
	}

	if len(pre) > 0 {
		p.updateRecord(rec, strings.Join(preWhy, "; "), func(r *model.Record) {
			for _, f := range pre {
				f(r)
			}
		})
	}
	if len(changes) > 0 {
		reason := shellWhy
		if len(changed) > 0 {
			reason = joinReason("changed in AD: "+strings.Join(changed, ", "), shellWhy)
		}
		p.modifyEntry(SectionUsers, cur, changes, reason)
		p.plan.Counts.UsersModified++
	}
	if len(post) > 0 {
		p.updateRecord(rec, strings.Join(postWhy, "; "), func(r *model.Record) {
			for _, f := range post {
				f(r)
			}
		})
	}
}

// dropChange removes attr from a change list and its name list.
func dropChange(ch []Change, names []string, attr string) ([]Change, []string) {
	var out []Change
	for _, c := range ch {
		if !strings.EqualFold(c.Attr, attr) {
			out = append(out, c)
		}
	}
	var n []string
	for _, x := range names {
		if !strings.EqualFold(x, attr) {
			n = append(n, x)
		}
	}
	return out, n
}

func joinReason(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}
