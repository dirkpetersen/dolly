package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const template = "../../dolly.yaml.template"

func runCLI(args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := run(args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestVersion(t *testing.T) {
	version, commit, date = "v1.2.3", "abc123", "2024-06-01"
	defer func() { version, commit, date = "", "", "" }()
	code, out, _ := runCLI("version")
	if code != 0 || strings.TrimSpace(out) != "dolly v1.2.3 (commit abc123, built 2024-06-01)" {
		t.Errorf("version: %d %q", code, out)
	}
}

func TestDryRunWithFixture(t *testing.T) {
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
	code, out, errs := runCLI("sync", "--dry-run", "--users", "--groups", "--config", template, "--fixture", "testdata/fixture.yaml")
	if code != 0 || !strings.Contains(out, "Plan: dolly sync (users and groups)") {
		t.Errorf("--users --groups: exit %d, stderr %q\n%s", code, errs, out)
	}
	_, neither, _ := runCLI("sync", "--dry-run", "--config", template, "--fixture", "testdata/fixture.yaml")
	if out != neither {
		t.Errorf("--users --groups differs from neither flag")
	}
}

func TestGuardExitCode(t *testing.T) {
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
	for _, tc := range []struct {
		args []string
		code int
		msg  string
	}{
		{[]string{"sync", "--config", template}, 1, "not implemented yet"},
		{[]string{"sync", "--dry-run", "--config", template}, 1, "reading password file"}, // reads AD for real; the template names no existing secret
		{[]string{"adopt", "--config", template}, 1, "not implemented yet"},
		{[]string{"unlock", "--yes"}, 1, "not implemented yet"},
		{[]string{"check"}, 1, "not implemented yet"},
		{[]string{"install"}, 1, "not implemented yet"},
		{[]string{"uninstall"}, 1, "not implemented yet"},
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
