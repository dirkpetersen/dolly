package source

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/dirkpetersen/dolly/internal/model"
)

func TestReadFollowsOutOfScopeMembers(t *testing.T) {
	user := &model.ADObject{GUID: "1", DN: "CN=a,OU=People,DC=x", Kind: model.KindUser}
	ext := &model.ADObject{GUID: "2", DN: "CN=ext,OU=Else,DC=x", Kind: model.KindUser}
	nested := &model.ADObject{GUID: "3", DN: "CN=deep,OU=Else,DC=x", Kind: model.KindUser}
	extGrp := &model.ADObject{GUID: "4", DN: "CN=grp,OU=Else,DC=x", Kind: model.KindGroup, Members: []string{nested.DN, "CN=a,OU=People,DC=x"}}
	grp := &model.ADObject{GUID: "5", DN: "CN=g,OU=Groups,DC=x", Kind: model.KindGroup,
		Members: []string{"cn=A,ou=people,dc=x", ext.DN, extGrp.DN, "CN=gone,OU=Else,DC=x"}}
	f := &Fake{InScopeUsers: []*model.ADObject{user}, InScopeGroups: []*model.ADObject{grp}, Others: []*model.ADObject{ext, nested, extGrp}}
	snap, err := Read(context.Background(), f, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Objects) != 5 || snap.Count(model.KindUser) != 1 || snap.Count(model.KindGroup) != 1 {
		t.Errorf("objects = %d", len(snap.Objects))
	}
	for _, o := range []*model.ADObject{ext, nested, extGrp} {
		if o.InScope {
			t.Errorf("%s marked in scope", o.DN)
		}
	}
	if !user.InScope || !grp.InScope {
		t.Error("in-scope objects not marked")
	}
	if len(snap.Unresolved) != 1 || snap.Unresolved[0] != "CN=gone,OU=Else,DC=x" {
		t.Errorf("unresolved = %v", snap.Unresolved)
	}
	if len(f.Lookups) != 2 {
		t.Errorf("lookups = %v (want one round per nesting level)", f.Lookups)
	}
}

func TestReadFailsOnError(t *testing.T) {
	f := &Fake{Err: errors.New("sizeLimitExceeded")}
	if _, err := Read(context.Background(), f, true); err == nil {
		t.Error("a failed read must fail the snapshot, never return partial data")
	}
}

// A groups-only read never searches the users base: every member, users
// and nested groups alike, is fetched by DN, and the users come back out
// of scope.
func TestReadGroupsOnlyUsesDNLookups(t *testing.T) {
	user := &model.ADObject{GUID: "1", DN: "CN=a,OU=People,DC=x", Kind: model.KindUser}
	deep := &model.ADObject{GUID: "2", DN: "CN=b,OU=People,DC=x", Kind: model.KindUser}
	child := &model.ADObject{GUID: "3", DN: "CN=child,OU=Groups,DC=x", Kind: model.KindGroup, Members: []string{deep.DN}}
	grp := &model.ADObject{GUID: "4", DN: "CN=g,OU=Groups,DC=x", Kind: model.KindGroup, Members: []string{user.DN, child.DN}}
	f := &usersForbidden{Fake: Fake{InScopeUsers: []*model.ADObject{user, deep}, InScopeGroups: []*model.ADObject{grp, child}}}
	snap, err := Read(context.Background(), f, false)
	if err != nil {
		t.Fatal(err)
	}
	if snap.UsersRead || snap.Count(model.KindUser) != 0 || snap.Count(model.KindGroup) != 2 || len(snap.Objects) != 4 {
		t.Errorf("UsersRead %v, users %d, groups %d, objects %d", snap.UsersRead, snap.Count(model.KindUser), snap.Count(model.KindGroup), len(snap.Objects))
	}
	if user.InScope || deep.InScope {
		t.Error("users fetched by DN must be out of scope")
	}
	// The child group is in scope, so both users are fetched in one round.
	if len(f.Lookups) != 1 || len(f.Lookups[0]) != 2 {
		t.Errorf("lookups = %v, want one round with both users", f.Lookups)
	}
}

// usersForbidden fails the test run if the users base is searched.
type usersForbidden struct{ Fake }

func (u *usersForbidden) Users(context.Context) ([]*model.ADObject, error) {
	return nil, errors.New("a groups-only read must not search the users base")
}

// Members found by DN must match the users or groups filter; one that
// doesn't (a user without uidNumber, a computer, a child group filtered
// out) is recorded as filtered, not as unresolved, and never followed.
func TestReadFilteredLookups(t *testing.T) {
	ok := &model.ADObject{GUID: "1", DN: "CN=ok,OU=People,DC=x", Kind: model.KindUser}
	noUID := &model.ADObject{GUID: "2", DN: "CN=nouid,OU=People,DC=x", Kind: model.KindUser}
	hidden := &model.ADObject{GUID: "5", DN: "CN=hidden,OU=People,DC=x", Kind: model.KindUser}
	childF := &model.ADObject{GUID: "3", DN: "CN=child,OU=Groups,DC=x", Kind: model.KindGroup, Members: []string{hidden.DN}}
	grp := &model.ADObject{GUID: "4", DN: "CN=g,OU=Groups,DC=x", Kind: model.KindGroup,
		Members: []string{ok.DN, noUID.DN, childF.DN, "CN=gone,OU=People,DC=x"}}
	f := &Fake{InScopeGroups: []*model.ADObject{grp}, Others: []*model.ADObject{ok}, Filtered: []*model.ADObject{noUID, childF, hidden}}
	for _, readUsers := range []bool{false, true} {
		snap, err := Read(context.Background(), f, readUsers)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(snap.Filtered, "|") != childF.DN+"|"+noUID.DN {
			t.Errorf("filtered = %v", snap.Filtered)
		}
		if strings.Join(snap.Unresolved, "|") != "CN=gone,OU=People,DC=x" {
			t.Errorf("unresolved = %v", snap.Unresolved)
		}
		if len(snap.Objects) != 2 {
			t.Errorf("objects = %d; a filtered child group must not be followed", len(snap.Objects))
		}
	}
}
