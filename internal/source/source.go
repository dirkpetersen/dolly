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
	// Lookup fetches objects by DN (out-of-scope group members). DNs that
	// don't exist in AD are left out of the result without an error.
	Lookup(ctx context.Context, dns []string) ([]*model.ADObject, error)
}

// Read builds a complete AD snapshot: all in-scope users and groups, then
// every out-of-scope object reachable through group membership, followed
// transitively through out-of-scope groups. Users and groups are always
// both read, whatever the run syncs: a groups-only run needs the users to
// resolve members, and a users-only run needs the groups to know which
// out-of-scope users are still managed (otherwise it would treat them as
// gone from AD).
func Read(ctx context.Context, src Source) (*model.ADSnapshot, error) {
	users, err := src.Users(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading AD users: %w", err)
	}
	groups, err := src.Groups(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading AD groups: %w", err)
	}
	snap := &model.ADSnapshot{}
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

	unresolved := map[string]string{}
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
		found, err := src.Lookup(ctx, dns)
		if err != nil {
			return nil, fmt.Errorf("looking up out-of-scope members: %w", err)
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
	for _, dn := range unresolved {
		snap.Unresolved = append(snap.Unresolved, dn)
	}
	sort.Strings(snap.Unresolved)
	return snap, nil
}
