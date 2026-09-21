package planner

import (
	"fmt"
	"sort"
	"strings"

	"github.com/dirkpetersen/dolly/internal/config"
	"github.com/dirkpetersen/dolly/internal/model"
)

// adoptUser is an AD user considered by adopt.
type adoptUser struct {
	obj *model.ADObject
	uid string
	dn  string
}

// planAdopt claims an existing tree (for example one written by
// ad2openldap): it creates ownership records for target entries that match
// an AD user by uid or an AD group by cn, and in each group marks the
// members that are also in the AD group as Dolly-owned. It writes records
// only; it never changes users or groups.
//
// spec: users are matched loosely, like ad2openldap wrote them: any AD user
// with a uid, even without uidNumber or gidNumber, and group membership is
// flattened through every child group and includes disabled accounts. That
// way users sync ignores are claimed too, and the first sync removes their
// memberships and (with prune_users) the entries, as the README describes.
// Groups must pass the required-attribute check, since an ignored group
// would lose its members on the first sync for no reason.
func (p *planner) planAdopt(ad *model.ADSnapshot) {
	rdn := p.cfg.Mapping.Users.RDN
	var cands []*adoptUser
	for _, o := range ad.Objects {
		if o.Kind != model.KindUser {
			continue
		}
		if uid := p.looseUID(o); uid != "" {
			cands = append(cands, &adoptUser{obj: o, uid: uid, dn: model.BuildDN(rdn, uid, p.cfg.Target.UsersBase)})
		}
	}
	winners := dedup(cands, func(c *adoptUser) (string, string, string, int) {
		r := p.rank(p.userRecs, p.userRecBySee, c.obj.GUID, c.dn, c.obj.InScope)
		if p.users[c.obj.GUID] != nil {
			r += 8
		}
		return c.uid, c.obj.GUID, c.dn, r
	}, func(loser, winner *adoptUser) {
		if !strings.HasPrefix(p.ignoredWhy[loser.obj.GUID], "duplicate") {
			p.warn(WarnDuplicate, loser.obj.DN, fmt.Sprintf("uid %q is also used by %s, which wins", loser.uid, winner.obj.DN))
		}
	})

	userDN := map[string]string{} // GUID -> target DN, for winners
	for _, c := range winners {
		guid := c.obj.GUID
		if rec := p.userRecs[guid]; rec != nil {
			userDN[guid] = rec.SeeAlso
			continue
		}
		userDN[guid] = c.dn
		e := p.userEntries[model.MustDNKey(c.dn)]
		if e == nil {
			continue
		}
		if owner := p.userRecBySee[model.MustDNKey(e.DN)]; owner != "" {
			p.conflict(e.DN, fmt.Sprintf("already owned by AD object %s; not adopted for %s", owner, guid))
			continue
		}
		userDN[guid] = e.DN
		reason := "adopt: target entry matches AD user " + c.uid
		if p.users[guid] == nil {
			reason += " (sync ignores this user: " + p.goneWhy(guid) + "; the first sync removes its memberships)"
		}
		p.addRecord(&model.Record{Kind: model.UserRecord, GUID: guid, SeeAlso: e.DN}, reason)
	}

	for _, g := range p.sortedGroups() {
		if p.groupRecs[g.obj.GUID] != nil {
			continue
		}
		e := p.groupEntries[model.MustDNKey(g.dn)]
		if e == nil {
			continue
		}
		if owner := p.groupRecBySee[model.MustDNKey(e.DN)]; owner != "" {
			p.conflict(e.DN, fmt.Sprintf("already owned by AD object %s; not adopted for %s", owner, g.obj.GUID))
			continue
		}
		var owned []string
		for _, o := range p.flatten(g.obj, true) {
			dn := userDN[o.GUID]
			if dn == "" {
				continue // a duplicate-uid loser
			}
			uid := model.RDNValue(dn)
			if (p.memberMode && findDN(e, config.AttrMember, dn) != "") || (p.uidMode && findExact(e, config.AttrMemberUID, uid) != "") {
				owned = append(owned, dn)
			}
		}
		sort.Slice(owned, func(i, j int) bool { return model.MustDNKey(owned[i]) < model.MustDNKey(owned[j]) })
		total := p.realMembers(e)
		if !p.memberMode {
			total = len(e.Get(config.AttrMemberUID))
		}
		p.addRecord(&model.Record{Kind: model.GroupRecord, GUID: g.obj.GUID, SeeAlso: e.DN, Occupants: owned},
			fmt.Sprintf("adopt: target group matches AD group %s; %d of %d members are in the AD group and become Dolly-owned, the rest stay local",
				g.cn, len(owned), total))
	}
}
