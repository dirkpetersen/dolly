package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-ldap/ldap/v3"

	"github.com/dirkpetersen/dolly/internal/config"
	"github.com/dirkpetersen/dolly/internal/fixture"
	"github.com/dirkpetersen/dolly/internal/ldapfake"
	"github.com/dirkpetersen/dolly/internal/planner"
)

// The template's bases: state_base ou=dolly,dc=local.
const lockDN = "cn=lock,ou=dolly,dc=local"

// withFake makes the commands use dir instead of dialing the target, and
// sets the confirmation input.
func withFake(t *testing.T, dir *ldapfake.Dir, input string, tty bool) {
	t.Helper()
	oldDial, oldIn, oldTTY := dialWriter, stdin, stdinIsTTY
	dialWriter = func(*config.Config) (writerConn, error) { return dir, nil }
	stdin = strings.NewReader(input)
	stdinIsTTY = func() bool { return tty }
	t.Cleanup(func() { dialWriter, stdin, stdinIsTTY = oldDial, oldIn, oldTTY })
}

func fakeWithLock(age time.Duration) *ldapfake.Dir {
	dir := ldapfake.New()
	dir.Put("ou=dolly,dc=local", map[string][]string{"objectClass": {"organizationalUnit"}, "ou": {"dolly"}}, time.Now())
	dir.Put(lockDN, map[string][]string{"objectClass": {"organizationalRole"}, "cn": {"lock"},
		"description": {"host=other", "pid=42", "command=dolly sync"}}, time.Now().Add(-age))
	return dir
}

func TestUnlock(t *testing.T) {
	t.Run("no lock", func(t *testing.T) {
		dir := ldapfake.New()
		withFake(t, dir, "", false)
		code, out, errs := runCLI("unlock", "--config", template)
		if code != 0 || !strings.Contains(out, "No run lock is held") {
			t.Errorf("exit %d, %q %q", code, out, errs)
		}
	})
	t.Run("not a terminal without --yes", func(t *testing.T) {
		dir := fakeWithLock(time.Minute)
		withFake(t, dir, "y\n", false)
		code, out, errs := runCLI("unlock", "--config", template)
		if code != 1 || !strings.Contains(errs, "stdin is not a terminal") || !strings.Contains(out, "host=other pid=42") || !dir.Has(lockDN) {
			t.Errorf("exit %d, %q %q, lock kept: %v", code, out, errs, dir.Has(lockDN))
		}
	})
	t.Run("declined", func(t *testing.T) {
		dir := fakeWithLock(time.Minute)
		withFake(t, dir, "n\n", true)
		code, out, _ := runCLI("unlock", "--config", template)
		if code != 0 || !strings.Contains(out, "Lock left in place") || !dir.Has(lockDN) {
			t.Errorf("exit %d, %q", code, out)
		}
	})
	t.Run("confirmed", func(t *testing.T) {
		dir := fakeWithLock(2 * time.Hour)
		withFake(t, dir, "yes\n", true)
		code, out, _ := runCLI("unlock", "--config", template)
		if code != 0 || !strings.Contains(out, "Removed "+lockDN) || !strings.Contains(out, "stale?:") || dir.Has(lockDN) {
			t.Errorf("exit %d, %q", code, out)
		}
	})
	t.Run("--yes", func(t *testing.T) {
		dir := fakeWithLock(time.Minute)
		withFake(t, dir, "", false)
		code, out, _ := runCLI("unlock", "--yes", "--config", template)
		if code != 0 || dir.Has(lockDN) || strings.Contains(out, "stale?:") {
			t.Errorf("exit %d, %q", code, out)
		}
	})
}

// A real run that finds the lock held by another run exits 0 quietly: one
// line on stderr, nothing on stdout, no reads, no writes.
func TestRealRunLockHeld(t *testing.T) {
	for _, cmd := range []string{"sync", "adopt"} {
		dir := fakeWithLock(time.Minute)
		withFake(t, dir, "", false)
		code, out, errs := runCLI(cmd, "--config", template)
		if code != 0 || out != "" || !strings.Contains(errs, "another run holds the lock (host=other") || strings.Count(errs, "\n") != 1 {
			t.Errorf("%s: exit %d, stdout %q, stderr %q", cmd, code, out, errs)
		}
		for _, c := range dir.Calls {
			if !strings.HasPrefix(c, "search ") && c != "add "+lockDN {
				t.Errorf("%s: unexpected call %q", cmd, c)
			}
		}
		if got := dir.Values(lockDN, "description"); len(got) != 3 || got[0] != "host=other" {
			t.Errorf("%s: the other run's lock changed: %v", cmd, got)
		}
	}
}

// withSnapshots makes a real run read the fixture instead of AD and the
// target.
func withSnapshots(t *testing.T, fix string) {
	t.Helper()
	old := readSnapshots
	readSnapshots = func(_ context.Context, cfg *config.Config, opt planner.Options, _ io.Writer) (*fixture.Snapshots, error) {
		return fixture.Load(fix, cfg, opt.Users || opt.Adopt)
	}
	t.Cleanup(func() { readSnapshots = old })
}

func statusNotes(dir *ldapfake.Dir) string {
	return strings.Join(dir.Values("cn=status,ou=dolly,dc=local", "description"), "\n")
}

// A real run end to end against the fake target: lock, read, plan, apply,
// status, unlock; per-entry errors exit 1; the guard exits 2 and writes
// nothing but the lock and cn=status.
func TestRealRun(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		dir := ldapfake.New()
		withFake(t, dir, "", false)
		withSnapshots(t, "testdata/fixture.yaml")
		code, out, errs := runCLI("sync", "--config", template)
		// The fixture has no state_base, so the plan creates it, but the run
		// created it before taking the lock: already in place.
		if code != 0 || !strings.Contains(out, "Result: 6 applied, 1 already in place, 0 skipped because an earlier step failed, 0 failed") {
			t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out, errs)
		}
		if !strings.Contains(errs, "created state_base ou=dolly,dc=local") || dir.Has(lockDN) || !dir.Has("uid=jdoe,ou=people,dc=local") {
			t.Errorf("stderr %q; lock left: %v", errs, dir.Has(lockDN))
		}
		if st := statusNotes(dir); !strings.Contains(st, "last-success=") || strings.Contains(st, "failure") {
			t.Errorf("status:\n%s", st)
		}
		if dir.Calls[0] != "search ou=dolly,dc=local" || dir.Calls[1] != "add ou=dolly,dc=local" || dir.Calls[2] != "add "+lockDN {
			t.Errorf("state_base and lock must come first: %v", dir.Calls[:3])
		}
	})
	t.Run("per-entry error", func(t *testing.T) {
		dir := ldapfake.New()
		dir.Fail = func(op, dn string, _ any) error {
			if op == "add" && dn == "uid=jdoe,ou=people,dc=local" {
				return ldapfake.Error(ldap.LDAPResultObjectClassViolation, "injected")
			}
			return nil
		}
		withFake(t, dir, "", false)
		withSnapshots(t, "testdata/fixture.yaml")
		code, out, errs := runCLI("sync", "--config", template)
		if code != 1 || !strings.Contains(out, "1 failed") || !strings.Contains(errs, "failed: add uid=jdoe,ou=people,dc=local") {
			t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out, errs)
		}
		if st := statusNotes(dir); !strings.Contains(st, "failure=run incomplete: 7 operations: 5 applied, 1 already in place, 0 skipped, 1 failed") || dir.Has(lockDN) {
			t.Errorf("status:\n%s", st)
		}
	})
	t.Run("guard", func(t *testing.T) {
		fix := filepath.Join(t.TempDir(), "empty.yaml")
		if err := os.WriteFile(fix, []byte("ad: {}\ntarget: {}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		dir := ldapfake.New()
		withFake(t, dir, "", false)
		withSnapshots(t, fix)
		code, _, errs := runCLI("sync", "--config", template)
		if code != 2 || !strings.Contains(errs, "mass-deletion guard tripped, nothing was changed") {
			t.Fatalf("exit %d, stderr %s", code, errs)
		}
		for _, c := range dir.Calls {
			if !strings.HasPrefix(c, "search ") && !strings.HasSuffix(c, lockDN) && !strings.HasSuffix(c, "cn=status,ou=dolly,dc=local") &&
				c != "add ou=dolly,dc=local" {
				t.Errorf("guard run wrote %q", c)
			}
		}
		if st := statusNotes(dir); !strings.Contains(st, "failure=mass-deletion guard tripped") {
			t.Errorf("status:\n%s", st)
		}
		code, out, _ := runCLI("sync", "--force", "--config", template)
		if code != 0 || !strings.Contains(out, "--force: applying the plan despite the guard") {
			t.Errorf("--force: exit %d\n%s", code, out)
		}
		if st := statusNotes(dir); strings.Contains(st, "failure") {
			t.Errorf("status after --force:\n%s", st)
		}
	})
}
