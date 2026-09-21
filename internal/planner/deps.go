package planner

import (
	"github.com/go-ldap/ldap/v3"

	"github.com/dirkpetersen/dolly/internal/config"
	"github.com/dirkpetersen/dolly/internal/model"
)

// Dependency keys tell the applier which later operations to skip when one
// fails. An operation is skipped when any key in its Needs is broken, and
// when an operation fails or is skipped, every key in its Provides becomes
// broken. The applier never reorders operations; it only skips.
//
// Keys:
//
//	entry:<DN>        the entry at DN is in place. Provided by the operations
//	                  that create, rename, or delete it (a failed modrdn breaks
//	                  the old and the new DN); needed by every operation on DN.
//	seq:<record DN>   one entity's crash-safe sequence: a user's (or group's)
//	                  record update, entry add, modrdn, reference fix-ups, and
//	                  "rename complete" note. The first failure skips the rest,
//	                  exactly as if the run had crashed at that point.
//	pair:<group>|<user> one membership: roleOccupant before the member values
//	                  (adds), member values before the roleOccupant (removals).
//	                  Adding a member also needs entry:<member DN>, so a user
//	                  whose creation failed this run joins no group.
//	placeholder:<DN>  empty_group_member was added before removing the last
//	                  real member; member deletes on DN need it.
//	adds:<DN>         the member values added to DN this run; removing the
//	                  placeholder needs them.
//
// A group's member operations need its seq key (a failed rename or create
// skips them) but don't provide it: one rejected member doesn't stop the
// group's other members.
const (
	keyEntry       = "entry:"
	keySeq         = "seq:"
	keyPair        = "pair:"
	keyPlaceholder = "placeholder:"
	keyAdds        = "adds:"
)

func entryKey(dn string) string        { return keyEntry + model.MustDNKey(dn) }
func seqKey(recDN string) string       { return keySeq + model.MustDNKey(recDN) }
func pairDepKey(g, user string) string { return keyPair + pairKey(g, user) }

// scope adds dependency keys to every operation emitted while it is active.
// A detached scope stops the provides of the scopes outside it: operations
// inside still need the outer keys, but their failure doesn't break them.
type scope struct {
	needs, provides []string
	detached        bool
}

// scoped runs f with scope s active.
func (p *planner) scoped(s scope, f func()) {
	p.scopes = append(p.scopes, s)
	defer func() { p.scopes = p.scopes[:len(p.scopes)-1] }()
	f()
}

// in runs f in a sequence: a scope that needs and provides keys.
func (p *planner) in(f func(), keys ...string) { p.scoped(scope{needs: keys, provides: keys}, f) }

// detached runs f in a scope that keeps the outer needs but drops the outer
// provides (see scope).
func (p *planner) detached(f func()) { p.scoped(scope{detached: true}, f) }

// userSeq and groupSeq name the sequence key of an AD object by the DN its
// ownership record has (or will have).
func (p *planner) userSeq(guid string) string {
	return seqKey((&model.Record{Kind: model.UserRecord, GUID: guid}).DN(p.cfg.Target.StateBase))
}

func (p *planner) groupSeq(guid string) string {
	return seqKey((&model.Record{Kind: model.GroupRecord, GUID: guid}).DN(p.cfg.Target.StateBase))
}

// deps fills in op's Needs and Provides: the active scopes plus the keys
// implied by the operation itself.
func (p *planner) deps(op *Op) {
	var needs, provides []string
	outerProvides := true
	for i := len(p.scopes) - 1; i >= 0; i-- {
		s := p.scopes[i]
		needs = append(needs, s.needs...)
		if outerProvides {
			provides = append(provides, s.provides...)
		}
		if s.detached {
			outerProvides = false
		}
	}
	self := entryKey(op.DN)
	switch op.Kind {
	// Adds and renames also need their own target DN key: keys enter the
	// broken set only after a failure, so this skips an add or rename only
	// when an earlier op on the same DN (a prune, a delete, a rename away)
	// failed or was skipped in this run.
	case CreateContainer, AddRecord:
		needs = append(needs, entryKey(parentDN(op.DN)), self)
		provides = append(provides, self)
	case AddEntry:
		needs = append(needs, self)
		provides = append(provides, self)
	case ModRDN:
		needs = append(needs, self, entryKey(op.NewRDN+","+parentDN(op.DN)))
		provides = append(provides, self, entryKey(op.NewRDN+","+parentDN(op.DN)))
	case DeleteEntry, DeleteRecord:
		needs = append(needs, self)
		provides = append(provides, self)
	case ModifyEntry, UpdateRecord, RenameMember:
		needs = append(needs, self)
	case AddMember:
		needs = append(needs, self)
		if op.Attr == config.AttrMember {
			provides = append(provides, keyAdds+model.MustDNKey(op.DN))
		}
	case DeleteMember:
		needs = append(needs, self)
		if op.Attr == config.AttrMember {
			needs = append(needs, keyPlaceholder+model.MustDNKey(op.DN))
		}
	case AddPlaceholder:
		needs = append(needs, self)
		provides = append(provides, keyPlaceholder+model.MustDNKey(op.DN))
	case DeletePlaceholder:
		needs = append(needs, self, keyAdds+model.MustDNKey(op.DN))
	}
	op.Needs = uniq(needs)
	op.Provides = uniq(provides)
}

// parentDN returns dn without its first RDN, or "" for an invalid DN.
func parentDN(dn string) string {
	d, err := ldap.ParseDN(dn)
	if err != nil || len(d.RDNs) == 0 {
		return ""
	}
	return (&ldap.DN{RDNs: d.RDNs[1:]}).String()
}

func uniq(in []string) []string {
	seen := make(map[string]bool, len(in))
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
