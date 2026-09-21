// Package fixture loads AD and target snapshots from a YAML or JSON file.
// It stands in for the real AD and target readers until they exist, so
// `dolly sync --dry-run --fixture FILE` can be exercised end to end.
//
// Format:
//
//	now: 2024-06-01T00:00:00Z      # optional run time, default: the real clock
//	ad:
//	  users:  [ {guid, dn, attrs: {uid: [jdoe], ...}, userAccountControl} ]
//	  groups: [ {guid, dn, attrs: {name: [...], gidNumber: [...]}, members: [dn, ...]} ]
//	  others: [ {guid, dn, kind: user|group|other, attrs, members} ]  # out of scope, found by DN only
//	target:
//	  entries: [ {dn, attrs} ]     # everything under users_base, groups_base, and state_base
package fixture

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/dirkpetersen/dolly/internal/config"
	"github.com/dirkpetersen/dolly/internal/model"
	"github.com/dirkpetersen/dolly/internal/source"
)

// File is the fixture file layout.
type File struct {
	Now *time.Time `yaml:"now"`
	AD  struct {
		Users  []*model.ADObject `yaml:"users"`
		Groups []*model.ADObject `yaml:"groups"`
		Others []*model.ADObject `yaml:"others"`
	} `yaml:"ad"`
	Target struct {
		Entries []*model.Entry `yaml:"entries"`
	} `yaml:"target"`
}

// Snapshots is what a fixture yields: the inputs of the planner.
type Snapshots struct {
	AD      *model.ADSnapshot
	Target  *model.TargetSnapshot
	Records *model.Records
	Now     time.Time // zero if the fixture doesn't set it
}

// Load reads a fixture and builds the snapshots the planner needs, reading
// AD through source.Read on a source.Fake like a real run would.
func Load(path string, cfg *config.Config) (*Snapshots, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f File
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("fixture %s: %w", path, err)
	}
	for _, list := range []struct {
		objs []*model.ADObject
		kind model.Kind
	}{{f.AD.Users, model.KindUser}, {f.AD.Groups, model.KindGroup}, {f.AD.Others, model.KindOther}} {
		for _, o := range list.objs {
			g, err := model.ParseGUID(o.GUID)
			if err != nil {
				return nil, fmt.Errorf("fixture %s: %s: %w", path, o.DN, err)
			}
			o.GUID = g
			if o.Kind == "" {
				o.Kind = list.kind
			}
		}
	}
	fake := &source.Fake{InScopeUsers: f.AD.Users, InScopeGroups: f.AD.Groups, Others: f.AD.Others}
	ad, err := source.Read(context.Background(), fake)
	if err != nil {
		return nil, err
	}
	tgt, recs, err := model.Classify(f.Target.Entries, model.Bases{
		Users: cfg.Target.UsersBase, Groups: cfg.Target.GroupsBase, State: cfg.Target.StateBase,
	})
	if err != nil {
		return nil, fmt.Errorf("fixture %s: %w", path, err)
	}
	s := &Snapshots{AD: ad, Target: tgt, Records: recs}
	if f.Now != nil {
		s.Now = *f.Now
	}
	return s, nil
}
