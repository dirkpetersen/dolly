package planner

import (
	"bytes"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dirkpetersen/dolly/internal/config"
	"github.com/dirkpetersen/dolly/internal/model"
)

// Options select what the planner plans.
type Options struct {
	// Users and Groups select the phases of a sync. Neither assumes the
	// other runs: a groups-only plan uses the target's current user DNs,
	// and a users-only plan changes group values only to keep them pointing
	// at renamed or pruned users.
	Users, Groups bool
	// Adopt plans `dolly adopt`: ownership records for existing entries
	// only. Users and Groups are ignored.
	Adopt bool
	// Now is the run's start time (for missing-since and pruning).
	Now time.Time
}

// Build computes the plan. It does no I/O. The snapshots are not modified.
func Build(ad *model.ADSnapshot, tgt *model.TargetSnapshot, recs *model.Records, cfg *config.Config, opt Options) (*Plan, error) {
	p, err := newPlanner(ad, tgt, recs, cfg, opt)
	if err != nil {
		return nil, err
	}
	if opt.Groups && !opt.Adopt && !tgt.UsersRead && tgt.ExistingUsers == nil {
		return nil, fmt.Errorf("planning groups needs the target's users: read users_base or look up the member uids (TargetSnapshot.ExistingUsers)")
	}
	p.prepare(ad)
	p.containers(tgt)
	switch {
	case opt.Adopt:
		p.plan.Mode = "adopt"
		p.planAdopt(ad)
	default:
		p.plan.Mode = "sync"
		if opt.Users {
			p.checkUserIDs()
			p.planUsers()
		}
		if opt.Groups {
			p.checkGroupIDs()
			p.planGroups()
		}
	}
	p.finish()
	return p.plan, nil
}

// adUser is an AD user with a mapped uid, mapped to its target form.
type adUser struct {
	obj      *model.ADObject
	attrs    map[string][]string // mapped target attributes
	uid      string
	dn       string // DN the entry should have
	disabled bool
}

// adGroup is a synced AD group, mapped to its target form.
type adGroup struct {
	obj   *model.ADObject
	attrs map[string][]string
	cn    string
	dn    string
}

// member is a desired group member.
type member struct{ dn, uid string }

type planner struct {
	cfg  *config.Config
	opt  Options
	plan *Plan

	userVals, groupVals   map[string]config.Value
	userAttrs, groupAttrs []string // sorted mapped attribute names
	memberMode, uidMode   bool
	placeholder           string

	// AD side
	byDN         map[string]*model.ADObject // DNKey -> object
	memberUsers  map[string]*adUser         // GUID -> users with a mapped uid (before dedup)
	validUsers   map[string]*adUser         // GUID -> users with all required attrs (before dedup)
	members      map[string]*adUser         // GUID -> users that can be group members (dedup winners)
	users        map[string]*adUser         // GUID -> managed user entries (dedup winners with all required attrs)
	groups       map[string]*adGroup        // GUID -> synced in-scope groups (dedup winners)
	groupMembers map[string][]*model.ADObject
	ignoredWhy   map[string]string // GUID -> why an object is not managed
	unresolved   map[string]bool
	filtered     map[string]bool // DNKey -> in AD, but matching neither search filter
	filteredSeen map[string]bool // filtered members referenced by a synced group
	skipped      map[string]bool
	reported     map[string]bool // GUIDs already listed as ignored

	// working copy of the target, updated as operations are planned
	userEntries, groupEntries   map[string]*model.Entry  // DNKey -> entry
	userRecs, groupRecs         map[string]*model.Record // GUID -> record
	userRecBySee, groupRecBySee map[string]string        // DNKey(seeAlso) -> GUID
	existing                    *model.UserSet           // users found by targeted lookups (groups-only runs)
	entryUIDs                   map[string]bool          // lowercased uids of userEntries, built on first use

	removedPairs, addedPairs map[string]bool
	localRemovedPairs        map[string]bool    // local memberships removed by a prune
	byUserDN                 map[string]*adUser // built on first use in the groups phase

	scopes []scope // active dependency scopes, see deps.go
}

func newPlanner(ad *model.ADSnapshot, tgt *model.TargetSnapshot, recs *model.Records, cfg *config.Config, opt Options) (*planner, error) {
	p := &planner{
		cfg:          cfg,
		opt:          opt,
		plan:         &Plan{Users: opt.Users || opt.Adopt, Groups: opt.Groups || opt.Adopt},
		userVals:     map[string]config.Value{},
		groupVals:    map[string]config.Value{},
		memberMode:   cfg.Mapping.Groups.HasMember(),
		uidMode:      cfg.Mapping.Groups.HasMemberUID(),
		placeholder:  cfg.Target.EmptyGroupMember,
		byDN:         map[string]*model.ADObject{},
		memberUsers:  map[string]*adUser{},
		validUsers:   map[string]*adUser{},
		members:      map[string]*adUser{},
		users:        map[string]*adUser{},
		groups:       map[string]*adGroup{},
		groupMembers: map[string][]*model.ADObject{},
		ignoredWhy:   map[string]string{},
		unresolved:   map[string]bool{},
		filtered:     map[string]bool{},
		filteredSeen: map[string]bool{},
		skipped:      map[string]bool{},
		reported:     map[string]bool{},
		userEntries:  map[string]*model.Entry{},
		groupEntries: map[string]*model.Entry{},
		userRecs:     map[string]*model.Record{},
		groupRecs:    map[string]*model.Record{},
		userRecBySee: map[string]string{}, groupRecBySee: map[string]string{},
		removedPairs: map[string]bool{}, addedPairs: map[string]bool{},
		localRemovedPairs: map[string]bool{},
		existing:          tgt.ExistingUsers,
	}
	for name, src := range cfg.Mapping.Users.Attributes {
		v, err := config.CompileValue(name, src)
		if err != nil {
			return nil, fmt.Errorf("mapping.users.attributes.%s: %w", name, err)
		}
		p.userVals[name] = v
	}
	for name, src := range cfg.Mapping.Groups.Attributes {
		v, err := config.CompileValue(name, src)
		if err != nil {
			return nil, fmt.Errorf("mapping.groups.attributes.%s: %w", name, err)
		}
		p.groupVals[name] = v
	}
	p.userAttrs = model.SortedKeys(p.userVals)
	p.groupAttrs = model.SortedKeys(p.groupVals)

	// Clone target entries so planning never mutates the caller's snapshot.
	// Config validation rejects users_base == groups_base; should an entry
	// still appear in both lists, one clone keeps both indexes consistent.
	clones := map[*model.Entry]*model.Entry{}
	clone := func(e *model.Entry) *model.Entry {
		if c, ok := clones[e]; ok {
			return c
		}
		c := e.Clone()
		clones[e] = c
		return c
	}
	for _, e := range tgt.Users {
		p.userEntries[model.MustDNKey(e.DN)] = clone(e)
	}
	for _, e := range tgt.Groups {
		p.groupEntries[model.MustDNKey(e.DN)] = clone(e)
	}
	for _, r := range recs.Users {
		if _, dup := p.userRecs[r.GUID]; dup {
			return nil, fmt.Errorf("two user ownership records for %s", r.GUID)
		}
		p.userRecs[r.GUID] = r.Clone()
		p.userRecBySee[model.MustDNKey(r.SeeAlso)] = r.GUID
		p.plan.Guard.OwnedUsers++
	}
	for _, r := range recs.Groups {
		if _, dup := p.groupRecs[r.GUID]; dup {
			return nil, fmt.Errorf("two group ownership records for %s", r.GUID)
		}
		p.groupRecs[r.GUID] = r.Clone()
		p.groupRecBySee[model.MustDNKey(r.SeeAlso)] = r.GUID
		p.plan.Guard.OwnedMemberships += len(r.Occupants)
	}
	p.plan.Counts.TargetUsers = len(tgt.Users)
	p.plan.Counts.TargetGroups = len(tgt.Groups)
	p.plan.TargetUsersRead = tgt.UsersRead
	p.plan.ADUsersRead = ad.UsersRead
	return p, nil
}

// prepare maps and validates AD objects, deduplicates names, flattens
// groups, and decides which users are managed.
func (p *planner) prepare(ad *model.ADSnapshot) {
	objs := append([]*model.ADObject(nil), ad.Objects...)
	sort.Slice(objs, func(i, j int) bool { return model.MustDNKey(objs[i].DN) < model.MustDNKey(objs[j].DN) })
	for _, o := range objs {
		p.byDN[model.MustDNKey(o.DN)] = o
		if !o.InScope {
			p.plan.Counts.Followed++
		}
	}
	for _, dn := range ad.Unresolved {
		p.unresolved[model.MustDNKey(dn)] = true
	}
	for _, dn := range ad.Filtered {
		p.filtered[model.MustDNKey(dn)] = true
	}
	p.plan.Counts.ADUsers = ad.Count(model.KindUser)
	p.plan.Counts.ADGroups = ad.Count(model.KindGroup)

	// spec: every user with a mapped uid can be a group member; only users
	// that also have every required attribute (uidNumber, gidNumber, ...)
	// get a user entry. A groups-only deployment therefore works with AD
	// users that have no Unix numbers at all.
	for _, o := range objs {
		if o.Kind != model.KindUser {
			continue
		}
		miss := o.Missing(p.cfg.Mapping.Users.Required)
		u, err := p.mapUser(o)
		switch {
		case errors.Is(err, errNoUID):
			why := "has no uid"
			if len(miss) > 0 {
				why = "missing required attribute " + strings.Join(miss, ", ")
			}
			p.ignoredWhy[o.GUID] = why
			if o.InScope {
				p.ignore(o, why)
			}
			continue
		case err != nil:
			p.ignoredWhy[o.GUID] = err.Error()
			p.reported[o.GUID] = true
			p.warn(WarnInvalid, o.DN, err.Error())
			continue
		}
		p.memberUsers[o.GUID] = u
		why := badIDs(u.attrs, "uidNumber", "gidNumber")
		if len(miss) > 0 {
			why = "missing required attribute " + strings.Join(miss, ", ")
		}
		if why != "" {
			p.ignoredWhy[o.GUID] = why
			if o.InScope {
				p.ignoreEntry(o)
			}
			continue
		}
		p.validUsers[o.GUID] = u
	}

	// Valid in-scope groups, deduplicated by cn.
	var cands []*adGroup
	for _, o := range objs {
		if o.Kind != model.KindGroup || !o.InScope {
			continue
		}
		if miss := o.Missing(p.cfg.Mapping.Groups.Required); len(miss) > 0 {
			p.ignoredWhy[o.GUID] = "missing required attribute " + strings.Join(miss, ", ")
			p.ignore(o, p.ignoredWhy[o.GUID])
			continue
		}
		g, err := p.mapGroup(o)
		if err != nil {
			p.ignoredWhy[o.GUID] = err.Error()
			p.warn(WarnInvalid, o.DN, err.Error())
			continue
		}
		if why := badIDs(g.attrs, "gidNumber"); why != "" {
			p.ignoredWhy[o.GUID] = why
			p.ignore(o, why)
			continue
		}
		cands = append(cands, g)
	}
	for _, g := range dedup(cands, func(g *adGroup) (string, string, string, int) {
		return g.cn, g.obj.GUID, g.dn, p.rank(p.groupRecs, p.groupRecBySee, g.obj.GUID, g.dn, true)
	}, func(loser, winner *adGroup) {
		p.ignoredWhy[loser.obj.GUID] = "duplicate cn " + loser.cn
		p.warn(WarnDuplicate, loser.obj.DN, fmt.Sprintf("cn %q is also used by %s, which wins", loser.cn, winner.obj.DN))
	}) {
		p.groups[g.obj.GUID] = g
	}

	// Flatten every synced group. Users reached this way are members, and
	// the valid ones are managed, also when they are outside the users
	// search base. Duplicate uids are resolved once over both sets, so a
	// name means the same AD user as a member and as an entry.
	byGUID := map[string]*adUser{}
	for guid, u := range p.validUsers {
		if u.obj.InScope {
			byGUID[guid] = u
		}
	}
	for _, guid := range model.SortedKeys(p.groups) {
		ms := p.flatten(p.groups[guid].obj, false)
		p.groupMembers[guid] = ms
		for _, m := range ms {
			byGUID[m.GUID] = p.memberUsers[m.GUID]
		}
	}
	var ucands []*adUser
	for _, guid := range model.SortedKeys(byGUID) {
		ucands = append(ucands, byGUID[guid])
	}
	for _, u := range dedup(ucands, func(u *adUser) (string, string, string, int) {
		return u.uid, u.obj.GUID, u.dn, p.rank(p.userRecs, p.userRecBySee, u.obj.GUID, u.dn, u.obj.InScope)
	}, func(loser, winner *adUser) {
		p.ignoredWhy[loser.obj.GUID] = "duplicate uid " + loser.uid
		p.warn(WarnDuplicate, loser.obj.DN, fmt.Sprintf("uid %q is also used by %s, which wins", loser.uid, winner.obj.DN))
	}) {
		p.members[u.obj.GUID] = u
		if p.validUsers[u.obj.GUID] != nil {
			p.users[u.obj.GUID] = u
		}
	}
	p.plan.Counts.UnresolvedMembers = len(p.unresolved)
	p.plan.Counts.FilteredMembers = len(p.filteredSeen)
	p.plan.Counts.SkippedMembers = len(p.skipped)
}

// rank orders duplicate candidates: the one that already owns the target
// entry wins, then any with a record, then in-scope objects.
//
// spec: "the first one wins" for duplicate uids, but AD returns objects in
// no stable order. Dolly prefers the current owner so a duplicate never
// takes over an existing entry, then falls back to the lowest DN.
func (p *planner) rank(recs map[string]*model.Record, bySee map[string]string, guid, dn string, inScope bool) int {
	r := 0
	if rec := recs[guid]; rec != nil {
		r += 2
		if bySee[model.MustDNKey(dn)] == guid {
			r += 4
		}
	}
	if inScope {
		r++
	}
	return r
}

// dedup keeps one candidate per case-insensitive name: highest rank, then
// lowest DN. Losers are reported through lost.
func dedup[T any](cands []T, key func(T) (name, guid, dn string, rank int), lost func(loser, winner T)) []T {
	byName := map[string][]T{}
	for _, c := range cands {
		n, _, _, _ := key(c)
		byName[strings.ToLower(n)] = append(byName[strings.ToLower(n)], c)
	}
	var out []T
	for _, n := range model.SortedKeys(byName) {
		list := byName[n]
		sort.SliceStable(list, func(i, j int) bool {
			_, _, di, ri := key(list[i])
			_, _, dj, rj := key(list[j])
			if ri != rj {
				return ri > rj
			}
			return model.MustDNKey(di) < model.MustDNKey(dj)
		})
		out = append(out, list[0])
		for _, l := range list[1:] {
			lost(l, list[0])
		}
	}
	return out
}

// flatten returns the users that are members of g, directly or through
// nested groups (if flatten_nested). A visited set guards against cycles.
// In strict mode only valid users and valid groups count, and invalid
// ones are reported as ignored. In loose mode (adopt) every user with a uid
// and every group is followed, mirroring what ad2openldap wrote.
func (p *planner) flatten(g *model.ADObject, loose bool) []*model.ADObject {
	visited := map[string]bool{model.MustDNKey(g.DN): true}
	seen := map[string]bool{}
	var out []*model.ADObject
	var walk func(*model.ADObject)
	walk = func(grp *model.ADObject) {
		for _, m := range grp.Members {
			k, err := model.DNKey(m)
			if err != nil {
				if !loose {
					p.unresolved["\x00"+m] = true
				}
				continue
			}
			o := p.byDN[k]
			switch {
			case o == nil && p.filtered[k]:
				if !loose {
					p.filteredSeen[k] = true
				}
			case o == nil:
				if !loose {
					p.unresolved[k] = true
				}
			case o.Kind == model.KindUser:
				ok := p.memberUsers[o.GUID] != nil
				if loose {
					ok = p.looseUID(o) != ""
				} else if !ok && p.ignoredWhy[o.GUID] != "" {
					p.ignore(o, p.ignoredWhy[o.GUID])
				} else if ok && p.validUsers[o.GUID] == nil {
					p.ignoreEntry(o)
				}
				if ok && !seen[o.GUID] {
					seen[o.GUID] = true
					out = append(out, o)
				}
			case o.Kind == model.KindGroup:
				if !p.cfg.Mapping.Groups.FlattenNested || visited[k] {
					continue
				}
				if !loose {
					if why := p.groupProblem(o); why != "" {
						p.ignore(o, why+"; not flattened into "+g.DN)
						continue
					}
				}
				visited[k] = true
				walk(o)
			default:
				if !loose {
					p.skipped[k] = true
				}
			}
		}
	}
	walk(g)
	sort.Slice(out, func(i, j int) bool { return out[i].GUID < out[j].GUID })
	return out
}

// groupProblem returns why a group can't be used (a missing required
// attribute or an invalid gidNumber), or "".
func (p *planner) groupProblem(o *model.ADObject) string {
	if miss := o.Missing(p.cfg.Mapping.Groups.Required); len(miss) > 0 {
		return "missing required attribute " + strings.Join(miss, ", ")
	}
	if attrs, err := p.mapAttrs(o, p.groupVals, p.groupAttrs); err == nil {
		return badIDs(attrs, "gidNumber")
	}
	return ""
}

// maxID is the largest valid uid or gid: Linux uid_t and gid_t are
// unsigned 32-bit, and 4294967295 is reserved ((uid_t)-1).
const maxID = 4294967294

// badIDs checks mapped ID attributes Dolly would write. It returns why one
// is invalid (not a single integer from 0 to maxID), or "". A missing
// attribute is the required list's business, not this check's.
//
// spec: an entry with an invalid uidNumber or gidNumber is ignored and
// listed once, like one missing a required attribute (AD may hold values
// such as 11-digit university IDs that no Unix system can use).
func badIDs(attrs map[string][]string, names ...string) string {
	for _, n := range names {
		var vs []string
		for k, v := range attrs {
			if strings.EqualFold(k, n) {
				vs = v
			}
		}
		switch {
		case len(vs) == 0:
			continue
		case len(vs) > 1:
			return fmt.Sprintf("%s has %d values, want one", n, len(vs))
		}
		if v, err := strconv.ParseUint(vs[0], 10, 64); err != nil || v > maxID {
			return fmt.Sprintf("%s %q is not a valid ID (an integer from 0 to %d)", n, vs[0], uint64(maxID))
		}
	}
	return ""
}

// looseUID returns the uid an AD user would have on the target, ignoring
// the required-attribute rule (for adopt).
func (p *planner) looseUID(o *model.ADObject) string {
	if u := p.memberUsers[o.GUID]; u != nil {
		return u.uid
	}
	attrs, err := p.mapAttrs(o, p.userVals, p.userAttrs)
	if err != nil {
		return ""
	}
	return first(attrs[p.cfg.Mapping.Users.RDN])
}

// errNoUID means the mapped uid (or RDN) is empty: the AD user lacks the
// attribute uid is mapped from and can't be a member or an entry.
var errNoUID = errors.New("mapped uid is empty")

func (p *planner) mapUser(o *model.ADObject) (*adUser, error) {
	attrs, err := p.mapAttrs(o, p.userVals, p.userAttrs)
	if err != nil {
		return nil, err
	}
	rdn := first(attrs[p.cfg.Mapping.Users.RDN])
	if rdn == "" || first(attrs["uid"]) == "" {
		return nil, errNoUID
	}
	return &adUser{obj: o, attrs: attrs, uid: rdn, dn: model.BuildDN(p.cfg.Mapping.Users.RDN, rdn, p.cfg.Target.UsersBase), disabled: o.Disabled()}, nil
}

func (p *planner) mapGroup(o *model.ADObject) (*adGroup, error) {
	attrs, err := p.mapAttrs(o, p.groupVals, p.groupAttrs)
	if err != nil {
		return nil, err
	}
	cn := first(attrs[p.cfg.Mapping.Groups.RDN])
	if cn == "" {
		return nil, fmt.Errorf("mapped %s is empty", p.cfg.Mapping.Groups.RDN)
	}
	return &adGroup{obj: o, attrs: attrs, cn: cn, dn: model.BuildDN(p.cfg.Mapping.Groups.RDN, cn, p.cfg.Target.GroupsBase)}, nil
}

// mapAttrs applies the attribute mapping. Plain names copy all values;
// templates get the first value of each AD attribute and yield one value
// (none if empty). IA5 attributes are transliterated to ASCII.
func (p *planner) mapAttrs(o *model.ADObject, vals map[string]config.Value, names []string) (map[string][]string, error) {
	data := make(map[string]string, len(o.Attrs))
	for k, v := range o.Attrs {
		if len(v) > 0 {
			data[k] = v[0]
		}
	}
	out := map[string][]string{}
	for _, name := range names {
		v := vals[name]
		var vs []string
		if v.Tmpl != nil {
			var b bytes.Buffer
			if err := v.Tmpl.Execute(&b, data); err != nil {
				return nil, fmt.Errorf("mapping %s: %v", name, err)
			}
			if s := b.String(); strings.TrimSpace(s) != "" {
				vs = []string{s}
			}
		} else {
			for _, x := range o.Get(v.Attr) {
				if x != "" {
					vs = append(vs, x)
				}
			}
		}
		if model.IsIA5(name) {
			for i := range vs {
				vs[i] = model.ToASCII(vs[i])
			}
		}
		if len(vs) > 0 {
			out[name] = vs
		}
	}
	return out, nil
}

// containers plans state_base and its record containers if missing. It
// never creates users_base or groups_base.
func (p *planner) containers(t *model.TargetSnapshot) {
	add := func(dn string) {
		attr, val, _ := model.RDN(dn)
		class := "organizationalUnit"
		if strings.EqualFold(attr, "cn") {
			class = "organizationalRole"
		}
		p.emit(Op{Kind: CreateContainer, Section: SectionContainers, DN: dn,
			Attrs:  []Attribute{{"objectClass", []string{class}}, {attr, []string{val}}},
			Reason: "Dolly's state container is missing"})
	}
	sb := p.cfg.Target.StateBase
	if !t.HasStateBase {
		add(sb)
	}
	if !t.HasUserRecords {
		add(model.RecordsBase(sb, model.UserRecord))
	}
	if !t.HasGroupRecords {
		add(model.RecordsBase(sb, model.GroupRecord))
	}
}

func (p *planner) finish() {
	g := &p.plan.Guard
	g.ADUsers, g.ADGroups = p.plan.Counts.ADUsers, p.plan.Counts.ADGroups
	g.MembershipRemovals = len(p.removedPairs)
	g.LocalRemovals = len(p.localRemovedPairs)
	g.MaxDeleteMin = p.cfg.Sync.MaxDeleteMin
	g.MaxDeletePercent = p.cfg.Sync.MaxDeletePercent
	g.UsersRead = p.plan.ADUsersRead
	if !p.opt.Adopt {
		g.evaluate()
	}
	order := map[WarningKind]int{WarnConflict: 0, WarnIDChanged: 1, WarnDuplicateID: 2, WarnDuplicate: 3, WarnInvalid: 4, WarnIgnored: 5, WarnPending: 6}
	sort.SliceStable(p.plan.Warnings, func(i, j int) bool {
		a, b := p.plan.Warnings[i], p.plan.Warnings[j]
		if order[a.Kind] != order[b.Kind] {
			return order[a.Kind] < order[b.Kind]
		}
		return strings.ToLower(a.Subject) < strings.ToLower(b.Subject)
	})
	sort.SliceStable(p.plan.MissingMembers, func(i, j int) bool {
		a, b := p.plan.MissingMembers[i], p.plan.MissingMembers[j]
		if ka, kb := model.MustDNKey(a.Group), model.MustDNKey(b.Group); ka != kb {
			return ka < kb
		}
		if la, lb := strings.ToLower(a.UID), strings.ToLower(b.UID); la != lb {
			return la < lb
		}
		return a.DN < b.DN
	})
	for _, w := range p.plan.Warnings {
		if w.Kind == WarnIgnored {
			p.plan.Counts.IgnoredEntries++
		}
	}
}

// evaluate applies the mass-deletion guard: a removal count trips it when
// it exceeds both max_delete_min and max_delete_percent of what Dolly owns.
// AD returning no users or no groups trips it too (no users only when the
// users base was read; a groups-only run doesn't read it). Local memberships
// removed by a prune (LocalRemovals) are reported but don't count: Dolly
// doesn't own them, so they are no part of the percentage's denominator.
func (g *Guard) evaluate() {
	check := func(what string, n, owned int) {
		if n > g.MaxDeleteMin && float64(n)*100 > g.MaxDeletePercent*float64(owned) {
			g.Reasons = append(g.Reasons, fmt.Sprintf("%d %s removals exceed max_delete_min (%d) and max_delete_percent (%g%% of %d owned)",
				n, what, g.MaxDeleteMin, g.MaxDeletePercent, owned))
		}
	}
	check("membership", g.MembershipRemovals, g.OwnedMemberships)
	check("user", g.UserRemovals, g.OwnedUsers)
	if g.UsersRead && g.ADUsers == 0 {
		g.Reasons = append(g.Reasons, "AD returned no users")
	}
	if g.ADGroups == 0 {
		g.Reasons = append(g.Reasons, "AD returned no groups")
	}
	g.Tripped = len(g.Reasons) > 0
}

// --- warnings ---

func (p *planner) warn(k WarningKind, subject, msg string) {
	p.plan.Warnings = append(p.plan.Warnings, Warning{Kind: k, Subject: subject, Message: msg})
}

func (p *planner) ignore(o *model.ADObject, why string) {
	if p.reported[o.GUID] {
		return
	}
	p.reported[o.GUID] = true
	p.warn(WarnIgnored, o.DN, fmt.Sprintf("%s %s", o.Kind, why))
}

func (p *planner) conflict(dn, msg string) { p.warn(WarnConflict, dn, msg) }

// ignoreEntry lists, in runs that manage user entries, a user that can be a
// group member but lacks a required attribute for its own entry.
func (p *planner) ignoreEntry(o *model.ADObject) {
	if p.plan.Users {
		p.ignore(o, p.ignoredWhy[o.GUID]+"; no user entry is managed for it (it can still be a group member)")
	}
}

// --- ID checks ---

// checkUserIDs reports duplicate uidNumbers among managed users and
// against local target entries. Duplicates never block the run.
func (p *planner) checkUserIDs() {
	nums := map[string][]string{}
	own := map[string]bool{}
	for _, u := range p.sortedUsers() {
		n := first(u.attrs["uidNumber"])
		if n != "" {
			nums[n] = append(nums[n], u.uid)
		}
		own[model.MustDNKey(u.dn)] = true
	}
	for k := range p.userRecBySee {
		own[k] = true
	}
	for _, k := range model.SortedKeys(p.userEntries) {
		e := p.userEntries[k]
		if n := e.First("uidNumber"); !own[k] && nums[n] != nil {
			nums[n] = append(nums[n], "local "+e.DN)
		}
	}
	for _, n := range model.SortedKeys(nums) {
		if len(nums[n]) > 1 {
			p.warn(WarnDuplicateID, "uidNumber "+n, "used by "+strings.Join(nums[n], ", "))
		}
	}
}

// checkGroupIDs is checkUserIDs for group gidNumbers.
func (p *planner) checkGroupIDs() {
	nums := map[string][]string{}
	own := map[string]bool{}
	for _, g := range p.sortedGroups() {
		if n := first(g.attrs["gidNumber"]); n != "" {
			nums[n] = append(nums[n], g.cn)
		}
		own[model.MustDNKey(g.dn)] = true
	}
	for k := range p.groupRecBySee {
		own[k] = true
	}
	for _, k := range model.SortedKeys(p.groupEntries) {
		e := p.groupEntries[k]
		if n := e.First("gidNumber"); !own[k] && nums[n] != nil {
			nums[n] = append(nums[n], "local "+e.DN)
		}
	}
	for _, n := range model.SortedKeys(nums) {
		if len(nums[n]) > 1 {
			p.warn(WarnDuplicateID, "gidNumber "+n, "used by "+strings.Join(nums[n], ", "))
		}
	}
}

// --- helpers ---

func (p *planner) sortedUsers() []*adUser {
	out := make([]*adUser, 0, len(p.users))
	for _, u := range p.users {
		out = append(out, u)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := strings.ToLower(out[i].uid), strings.ToLower(out[j].uid)
		if a != b {
			return a < b
		}
		return out[i].obj.GUID < out[j].obj.GUID
	})
	return out
}

func (p *planner) sortedGroups() []*adGroup {
	out := make([]*adGroup, 0, len(p.groups))
	for _, g := range p.groups {
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := strings.ToLower(out[i].cn), strings.ToLower(out[j].cn)
		if a != b {
			return a < b
		}
		return out[i].obj.GUID < out[j].obj.GUID
	})
	return out
}

// entryIndex returns the working entries and seeAlso index for a record kind.
func (p *planner) entryIndex(k model.RecordKind) (map[string]*model.Entry, map[string]string) {
	if k == model.GroupRecord {
		return p.groupEntries, p.groupRecBySee
	}
	return p.userEntries, p.userRecBySee
}

// findManaged returns the target entry a record points to: at its seeAlso,
// or, after an interrupted rename, at a renaming-from DN. A renaming-from DN
// is accepted only when nothing exists at seeAlso and no other record
// claims that DN, so a stale note never makes a local entry (or another
// record's entry) look managed.
func (p *planner) findManaged(rec *model.Record) *model.Entry {
	entries, bySee := p.entryIndex(rec.Kind)
	if e := entries[model.MustDNKey(rec.SeeAlso)]; e != nil {
		return e
	}
	for _, from := range rec.NoteValues(model.NoteRenamingFrom) {
		k := model.MustDNKey(from)
		if owner := bySee[k]; owner != "" && owner != rec.GUID {
			continue
		}
		if e := entries[k]; e != nil {
			return e
		}
	}
	return nil
}

// oldDNs lists the DNs other entries may still use for a managed entry
// that should now be at dn: renaming-from notes, the record's seeAlso, and
// the entry's current DN. An old DN where a target entry exists that isn't
// the managed entry cur, or that another record claims, is dropped and
// reported as a conflict: after a stale renaming-from note (a crash before
// the note was cleared), the name may since have been reused by a local
// entry, whose memberships must never be renamed.
func (p *planner) oldDNs(rec *model.Record, cur *model.Entry, dn string) []string {
	entries, bySee := p.entryIndex(rec.Kind)
	var out []string
	seen := map[string]bool{}
	add := func(s string) {
		if s == "" || model.DNExact(s, dn) || seen[s] {
			return
		}
		seen[s] = true
		k := model.MustDNKey(s)
		if e := entries[k]; e != nil && e != cur {
			p.conflict(s, fmt.Sprintf("record %s still lists this DN as renaming-from, but the entry here isn't the one Dolly manages for it; its values are left alone and the note is dropped", rec.GUID))
			return
		}
		if owner := bySee[k]; owner != "" && owner != rec.GUID {
			p.conflict(s, fmt.Sprintf("record %s still lists this DN as renaming-from, but it belongs to record %s; its values are left alone and the note is dropped", rec.GUID, owner))
			return
		}
		out = append(out, s)
	}
	for _, from := range rec.NoteValues(model.NoteRenamingFrom) {
		add(from)
	}
	add(rec.SeeAlso)
	if cur != nil {
		add(cur.DN)
	}
	return out
}

func first(v []string) string {
	if len(v) > 0 {
		return v[0]
	}
	return ""
}

func sameValues(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x := append([]string(nil), a...)
	y := append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

// findDN returns the stored value of attr equal to dn (DN matching), or "".
func findDN(e *model.Entry, attr, dn string) string {
	for _, v := range e.Get(attr) {
		if model.DNEqual(v, dn) {
			return v
		}
	}
	return ""
}

// findExact returns value if attr holds it exactly (memberUid uses
// caseExactIA5Match), or "".
func findExact(e *model.Entry, attr, value string) string {
	for _, v := range e.Get(attr) {
		if v == value {
			return v
		}
	}
	return ""
}

func exactEq(a, b string) bool { return a == b }

func pairKey(group, userDN string) string {
	return model.MustDNKey(group) + "|" + model.MustDNKey(userDN)
}
