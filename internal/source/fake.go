package source

import (
	"context"

	"github.com/dirkpetersen/dolly/internal/model"
)

// Fake is an in-memory Source for tests and fixtures. InScopeUsers and
// InScopeGroups are what the searches return; Others are reachable only
// through Lookup. Err, if set, is returned by every call.
type Fake struct {
	InScopeUsers  []*model.ADObject
	InScopeGroups []*model.ADObject
	Others        []*model.ADObject
	Err           error
	// Lookups records the DNs passed to Lookup, for tests.
	Lookups [][]string
}

// Users implements Source.
func (f *Fake) Users(context.Context) ([]*model.ADObject, error) {
	return f.InScopeUsers, f.Err
}

// Groups implements Source.
func (f *Fake) Groups(context.Context) ([]*model.ADObject, error) {
	return f.InScopeGroups, f.Err
}

// Lookup implements Source. It searches all three lists, like a base-scoped
// search by DN would.
func (f *Fake) Lookup(_ context.Context, dns []string) ([]*model.ADObject, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	f.Lookups = append(f.Lookups, dns)
	var out []*model.ADObject
	for _, dn := range dns {
		for _, list := range [][]*model.ADObject{f.Others, f.InScopeUsers, f.InScopeGroups} {
			for _, o := range list {
				if model.DNEqual(o.DN, dn) {
					out = append(out, o)
				}
			}
		}
	}
	return out, nil
}
