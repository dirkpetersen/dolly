package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/dirkpetersen/dolly/internal/check"
	"github.com/dirkpetersen/dolly/internal/install"
	"github.com/dirkpetersen/dolly/internal/ldapconn"
)

// templateSrc is the checked-in config template.
const templateSrc = "../../dolly.yaml.template"

// templateCopy copies the template into a temp dir and returns its path.
// Its relative paths (password_file: ad.secret, ...) then resolve against
// that empty dir, never against the repo root, where a git-ignored secret
// may or may not exist.
func templateCopy(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(templateSrc)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "dolly.yaml")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestMain makes sure no test can touch the real environment: install and
// uninstall get an environment that fails, and systemctl a runner that
// fails, unless a test sets its own (temp HOME and XDG dirs, fake
// systemctl). SMTP and LDAP are only ever faked per test.
func TestMain(m *testing.M) {
	installEnv = func() (install.Env, error) {
		return install.Env{}, errors.New("test guard: set installEnv to a temp environment")
	}
	systemctl = refuseSystemctl{}
	checkDial = func(context.Context, ldapconn.Options) (check.Conn, error) {
		return nil, errors.New("test guard: set checkDial to a fake")
	}
	notifyOptions.Dial = localOnly
	os.Exit(m.Run())
}

// localOnly lets notifications reach only a fake SMTP server on 127.0.0.1;
// the template's mx.example.edu is never contacted.
func localOnly(ctx context.Context, network, addr string) (net.Conn, error) {
	if host, _, _ := net.SplitHostPort(addr); host != "127.0.0.1" {
		return nil, fmt.Errorf("test guard: no SMTP connection to %s", addr)
	}
	return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, addr)
}

type refuseSystemctl struct{}

func (refuseSystemctl) Run([]string, ...string) (string, error) {
	return "", errors.New("test guard: set systemctl to a fake")
}

func runCLI(args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := run(args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestVersion(t *testing.T) {
	version, commit, date = "v1.2.3", "abc123", "2024-06-01"
	defer func() { version, commit, date = "", "", "" }()
	code, out, _ := runCLI("version")
	if code != 0 || strings.TrimSpace(out) != "dolly v1.2.3 (commit abc123, built 2024-06-01, "+runtime.Version()+")" {
		t.Errorf("version: %d %q", code, out)
	}
}

func TestDryRunWithFixture(t *testing.T) {
	template := templateCopy(t)
	code, out, errs := runCLI("sync", "--dry-run", "--config", template, "--fixture", "testdata/fixture.yaml")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	for _, want := range []string{
		"Plan: dolly sync (users and groups)",
		"add uid=jdoe,ou=people,dc=local",
		"gecos: Jane Doe",                             // transliterated
		"loginShell: /sbin/nologin",                   // disabled account
		"conflict     cn=hpc-users,ou=group,dc=local", // group exists without a record
		"ignored      CN=No Gid,OU=People",
		"Guard: ok",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}

	code, out, _ = runCLI("adopt", "--dry-run", "--config", template, "--fixture", "testdata/fixture.yaml")
	if code != 0 || !strings.Contains(out, "add record cn=00000000-0000-0000-0000-0000000000a1,ou=groups,ou=dolly,dc=local") {
		t.Errorf("adopt: %d\n%s", code, out)
	}

	code, out, _ = runCLI("sync", "--dry-run", "--groups", "--config", template, "--fixture", "testdata/fixture.yaml")
	if code != 0 || !strings.Contains(out, "(groups)") || strings.Contains(out, "add uid=") {
		t.Errorf("groups only: %d\n%s", code, out)
	}

	code, out, _ = runCLI("sync", "--dry-run", "--users", "--config", template, "--fixture", "testdata/fixture.yaml")
	if code != 0 || !strings.Contains(out, "Plan: dolly sync (users)") {
		t.Errorf("users only: %d\n%s", code, out)
	}
}

// --users and --groups together mean both, the same as neither.
func TestUsersAndGroupsFlagsTogether(t *testing.T) {
	template := templateCopy(t)
	code, out, errs := runCLI("sync", "--dry-run", "--users", "--groups", "--config", template, "--fixture", "testdata/fixture.yaml")
	if code != 0 || !strings.Contains(out, "Plan: dolly sync (users and groups)") {
		t.Errorf("--users --groups: exit %d, stderr %q\n%s", code, errs, out)
	}
	_, neither, _ := runCLI("sync", "--dry-run", "--config", template, "--fixture", "testdata/fixture.yaml")
	if out != neither {
		t.Errorf("--users --groups differs from neither flag")
	}
}

// --debug lists each member skipped for having no entry on the target on
// stderr; without it, the plan shows only the count, and never a warning.
func TestDebugListsMissingMembers(t *testing.T) {
	template := templateCopy(t)
	fix := filepath.Join(t.TempDir(), "missing.yaml")
	data := `now: 2024-06-01T00:00:00Z
ad:
  users:
    - guid: 00000000-0000-0000-0000-000000000001
      dn: CN=jdoe,OU=People,DC=example,DC=edu
      attrs: {uid: [jdoe]}
      userAccountControl: 512
    - guid: 00000000-0000-0000-0000-000000000002
      dn: CN=bob,OU=People,DC=example,DC=edu
      attrs: {uid: [bob]}
      userAccountControl: 512
  groups:
    - guid: 00000000-0000-0000-0000-0000000000a1
      dn: CN=lab,OU=Groups,DC=example,DC=edu
      attrs: {name: [lab], gidNumber: ["5000"]}
      members:
        - CN=jdoe,OU=People,DC=example,DC=edu
        - CN=bob,OU=People,DC=example,DC=edu
target:
  entries:
    - dn: ou=people,dc=local
      attrs: {objectClass: [organizationalUnit], ou: [people]}
    - dn: ou=group,dc=local
      attrs: {objectClass: [organizationalUnit], ou: [group]}
    - dn: uid=jdoe,ou=people,dc=local
      attrs: {objectClass: [account], uid: [jdoe]}
`
	if err := os.WriteFile(fix, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	args := []string{"sync", "--dry-run", "--groups", "--config", template, "--fixture", fix}
	code, out, errs := runCLI(args...)
	if code != 0 || !strings.Contains(out, "  members skipped: 1 not on the target (use --debug to list them)") ||
		strings.Contains(out, "Warnings") || strings.Contains(errs, "debug:") {
		t.Errorf("without --debug: exit %d, stderr %q\n%s", code, errs, out)
	}
	code, out2, errs := runCLI(append(args, "--debug")...)
	want := "debug: skip bob in cn=lab,ou=group,dc=local: no entry on the target at uid=bob,ou=people,dc=local\n"
	if code != 0 || !strings.Contains(errs, want) || out2 != out {
		t.Errorf("--debug: exit %d, stderr %q, want %q; stdout changed: %v", code, errs, want, out2 != out)
	}
	if code, _, errs := runCLI("adopt", "--dry-run", "--debug", "--config", template, "--fixture", fix); code != 0 {
		t.Errorf("adopt --debug: exit %d, %s", code, errs)
	}
}

func TestGuardExitCode(t *testing.T) {
	template := templateCopy(t)
	dir := t.TempDir()
	fix := filepath.Join(dir, "empty.yaml")
	if err := os.WriteFile(fix, []byte("ad: {}\ntarget: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, _ := runCLI("sync", "--dry-run", "--config", template, "--fixture", fix)
	if code != 2 || !strings.Contains(out, "AD returned no users") {
		t.Errorf("guard: exit %d\n%s", code, out)
	}
	if code, _, _ := runCLI("sync", "--dry-run", "--force", "--config", template, "--fixture", fix); code != 0 {
		t.Errorf("--force: exit %d", code)
	}
}

func TestExitCodes(t *testing.T) {
	template := templateCopy(t)
	for _, tc := range []struct {
		args []string
		code int
		msg  string
	}{
		{[]string{"sync", "--config", template}, 1, "reading password file"}, // dials the target for real
		{[]string{"sync", "--config", template, "--fixture", "testdata/fixture.yaml"}, 1, "--fixture is only valid with --dry-run"},
		{[]string{"sync", "--dry-run", "--config", template}, 1, "reading password file"}, // reads AD for real; the template names no existing secret
		{[]string{"adopt", "--config", template}, 1, "reading password file"},
		{[]string{"unlock", "--yes", "--config", template}, 1, "reading password file"},
		{[]string{"check", "--config", "/nonexistent.yaml"}, 1, ""}, // the ✗ line goes to stdout
		{[]string{"install", "--bogus"}, 1, "flag provided but not defined"},
		{[]string{"uninstall", "extra"}, 1, "unexpected argument"},
		{[]string{"sync", "--config", "/nonexistent.yaml"}, 1, "does not exist"},
		{[]string{"diff"}, 1, "unknown command"},
		{[]string{"sync", "--bogus"}, 1, "flag provided but not defined"},
		{[]string{"help"}, 0, ""},
		{[]string{"sync", "-h"}, 0, "--fixture FILE"},
	} {
		code, _, errs := runCLI(tc.args...)
		if code != tc.code || !strings.Contains(errs, tc.msg) {
			t.Errorf("%v: exit %d, stderr %q; want %d and %q", tc.args, code, errs, tc.code, tc.msg)
		}
	}
}
