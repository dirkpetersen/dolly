package main

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/dirkpetersen/dolly/internal/config"
	"github.com/dirkpetersen/dolly/internal/ldapfake"
	"github.com/dirkpetersen/dolly/internal/planner"
	"github.com/dirkpetersen/dolly/internal/source"
	"github.com/dirkpetersen/dolly/internal/target"
)

// readWorld serves readLive from two ldapfakes: AD with 3 users and 2
// groups (each group holding a user outside the users base), and a target
// with a few entries. slow, if set, delays the searches of one base each.
func readWorld(t *testing.T, slow map[string]time.Duration) *config.Config {
	t.Helper()
	cfg, err := config.Load(templateCopy(t))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	ad, tgt := ldapfake.New(), ldapfake.New()
	guid := func(i int) string { return fmt.Sprintf("%016d", i) }
	for _, dn := range []string{cfg.Source.Users.Base, cfg.Source.Groups.Base} {
		ad.Put(dn, map[string][]string{"objectClass": {"organizationalUnit"}}, now)
	}
	for i := 1; i <= 3; i++ {
		ad.Put(fmt.Sprintf("CN=u%d,%s", i, cfg.Source.Users.Base), map[string][]string{"objectClass": {"user"}, "objectCategory": {"person"},
			"objectGUID": {guid(i)}, "sAMAccountName": {fmt.Sprintf("u%d", i)}}, now)
	}
	ad.Put("CN=x,OU=Elsewhere,DC=example,DC=edu", map[string][]string{"objectClass": {"user"}, "objectCategory": {"person"},
		"objectGUID": {guid(9)}, "sAMAccountName": {"x"}}, now)
	for i := 1; i <= 2; i++ {
		ad.Put(fmt.Sprintf("CN=g%d,%s", i, cfg.Source.Groups.Base), map[string][]string{"objectClass": {"group"},
			"objectGUID": {guid(10 + i)}, "name": {fmt.Sprintf("g%d", i)}, "gidNumber": {"100"}, "member": {"CN=x,OU=Elsewhere,DC=example,DC=edu"}}, now)
	}
	for _, dn := range []string{cfg.Target.GroupsBase, cfg.Target.UsersBase, "uid=x," + cfg.Target.UsersBase} {
		tgt.Put(dn, map[string][]string{"objectClass": {"top"}, "uid": {"x"}}, now)
	}
	for _, d := range []*ldapfake.Dir{ad, tgt} {
		d.Before = func(op, dn string) {
			if op == "search" {
				time.Sleep(slow[dn])
			}
		}
	}
	oldAD, oldTgt := dialAD, dialTarget
	dialAD = func(context.Context, *config.Config) (*source.AD, error) {
		return source.NewAD(ad, "ldaps://dc", cfg), nil
	}
	dialTarget = func(context.Context, *config.Config) (*target.Reader, error) {
		return target.NewReader(tgt, "ldaps://tgt", cfg), nil
	}
	oldFirst, oldEvery := progressFirst, readProgressEvery
	progressFirst, readProgressEvery = 20*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { dialAD, dialTarget, progressFirst, readProgressEvery = oldAD, oldTgt, oldFirst, oldEvery })
	return cfg
}

// Read progress lines carry the counts so far and go to the progress
// writer only; a fast read prints none.
func TestReadProgress(t *testing.T) {
	cfg := readWorld(t, nil)
	var log, prog bytes.Buffer
	if _, err := readLive(context.Background(), cfg, planner.Options{Users: true, Groups: true}, &log, &prog); err != nil {
		t.Fatal(err)
	}
	if prog.Len() != 0 || !strings.Contains(log.String(), "read AD: 3 users and 2 groups") {
		t.Errorf("fast read:\nprogress %q\nlog:\n%s", prog.String(), log.String())
	}

	// Slow groups search: the users are counted, the groups not yet.
	cfg = readWorld(t, map[string]time.Duration{cfg.Source.Groups.Base: 70 * time.Millisecond, cfg.Target.StateBase: 70 * time.Millisecond})
	log.Reset()
	if _, err := readLive(context.Background(), cfg, planner.Options{Users: true, Groups: true}, &log, &prog); err != nil {
		t.Fatal(err)
	}
	out := prog.String()
	if !strings.Contains(out, "dolly: reading AD: 3 users, 0 groups so far, ") || !strings.Contains(out, "dolly: reading target: 3 entries so far, ") ||
		strings.Contains(log.String(), "reading ") || strings.Contains(out, "\r") {
		t.Errorf("progress:\n%s\nlog:\n%s", out, log.String())
	}

	// Groups only: members fetched by DN, then the uid lookups.
	cfg = readWorld(t, map[string]time.Duration{"CN=x,OU=Elsewhere,DC=example,DC=edu": 70 * time.Millisecond, cfg.Target.UsersBase: 70 * time.Millisecond})
	prog.Reset()
	if _, err := readLive(context.Background(), cfg, planner.Options{Groups: true}, &log, &prog); err != nil {
		t.Fatal(err)
	}
	out = prog.String()
	if !strings.Contains(out, "dolly: reading AD: 2 groups, 0 members fetched by DN so far, ") || !strings.Contains(out, "dolly: looking up member uids: 0 of 1, ") {
		t.Errorf("groups-only progress:\n%s", out)
	}
}
