package source

import (
	"context"
	"errors"
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
	snap, err := Read(context.Background(), f)
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
	if _, err := Read(context.Background(), f); err == nil {
		t.Error("a failed read must fail the snapshot, never return partial data")
	}
}
