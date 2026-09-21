// Package source reads Active Directory. The Source interface hides the
// LDAP access so the rest of Dolly can be tested with the in-memory Fake.
package source

import (
	"context"
	"fmt"
	"sort"

	"github.com/dirkpetersen/dolly/internal/model"
)

// Source reads AD. Every method must return complete data or an error:
// a truncated or failed read (sizeLimitExceeded, a paging error, a ranged
// member fetch that stops early) is an error, never a partial result.
type Source interface {
	// Users returns every in-scope user (the configured users base and filter).
	Users(ctx context.Context) ([]*model.ADObject, error)
	// Groups returns every in-scope group with all its member DNs.
	Groups(ctx context.Context) ([]*model.ADObject, error)
	// Lookup fetches objects by DN (group members not found by the
	// searches). An object must match the users filter (source.users.filter)
	// or the groups filter (source.groups.filter), exactly like the
	// searches; one that exists but matches neither is returned in
	// filtered. DNs that don't exist in AD are in neither list, without an
	// error.
	Lookup(ctx context.Context, dns []string) (found []*model.ADObject, filtered []string, err error)
}

// Read builds a complete AD snapshot: all in-scope groups, all in-scope
// users if readUsers is set, then every object reachable through group
// membership, fetched by DN and followed transitively through child groups.
//
// spec: a run that syncs users (and adopt) reads the users base, because
// users are managed whether or not a group references them, and a
// users-only run still reads the groups to know which out-of-scope users
// are still managed (otherwise it would treat them as gone from AD). A
// groups-only run does not read the users base: members are resolved by
// DN lookups only, so its cost follows the size of the synced groups, not
// the size of the users base (which may hold hundreds of thousands of
// accounts). Its users are all InScope false.
func Read(ctx context.Context, src Source, readUsers bool) (*model.ADSnapshot, error) {
	var users []*model.ADObject
	if readUsers {
		var err error
		if users, err = src.Users(ctx); err != nil {
			return nil, fmt.Errorf("reading AD users: %w", err)
		}
	}
	groups, err := src.Groups(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading AD groups: %w", err)
	}
	snap := &model.ADSnapshot{UsersRead: readUsers}
	byDN := map[string]*model.ADObject{}
	addObj := func(o *model.ADObject, inScope bool) error {
		k, err := model.DNKey(o.DN)
		if err != nil {
			return fmt.Errorf("AD object: %w", err)
		}
		if _, dup := byDN[k]; dup {
			return nil
		}
		o.InScope = inScope
		byDN[k] = o
		snap.Objects = append(snap.Objects, o)
		return nil
	}
	for _, o := range append(append([]*model.ADObject(nil), users...), groups...) {
		if err := addObj(o, true); err != nil {
			return nil, err
		}
	}

	// spec: the users and groups filters apply to members found by DN too,
	// so a member that doesn't match is skipped as if it were out of scope,
	// and counted as filtered, not as unresolved.
	unresolved := map[string]string{}
	filteredOut := map[string]string{}
	pending := groups
	for len(pending) > 0 {
		want := map[string]string{}
		for _, g := range pending {
			for _, m := range g.Members {
				k, err := model.DNKey(m)
				if err != nil {
					unresolved["\x00"+m] = m
					continue
				}
				if byDN[k] == nil && unresolved[k] == "" {
					want[k] = m
				}
			}
		}
		if len(want) == 0 {
			break
		}
		dns := make([]string, 0, len(want))
		for _, k := range model.SortedKeys(want) {
			dns = append(dns, want[k])
		}
		found, filtered, err := src.Lookup(ctx, dns)
		if err != nil {
			return nil, fmt.Errorf("looking up group members by DN: %w", err)
		}
		for _, dn := range filtered {
			k := model.MustDNKey(dn)
			if _, ok := want[k]; ok {
				delete(want, k)
				unresolved[k] = dn // never looked up again
				filteredOut[k] = dn
			}
		}
		pending = nil
		for _, o := range found {
			if err := addObj(o, false); err != nil {
				return nil, err
			}
			if o.Kind == model.KindGroup {
				pending = append(pending, o)
			}
			delete(want, model.MustDNKey(o.DN))
		}
		for k, dn := range want {
			unresolved[k] = dn
		}
	}
	for k, dn := range unresolved {
		if _, f := filteredOut[k]; f {
			snap.Filtered = append(snap.Filtered, dn)
		} else {
			snap.Unresolved = append(snap.Unresolved, dn)
		}
	}
	sort.Strings(snap.Unresolved)
	sort.Strings(snap.Filtered)
	return snap, nil
}
