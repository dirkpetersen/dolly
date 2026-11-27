package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The sync flags that change the config for one run, and their errors.
func TestOverrideFlags(t *testing.T) {
	template := templateCopy(t)
	for _, tc := range []struct {
		args []string
		code int
		msg  string
	}{
		{[]string{"sync", "--upg-prefix", "", "--config", template}, 1, "must not be empty"},
		{[]string{"sync", "--upg-prefix", "u-{{ .uid }}", "--config", template}, 1, "must not contain {{"},
		{[]string{"adopt", "--only-group-members", "--config", template}, 1, "flag provided but not defined"},
		{[]string{"adopt", "--upg-prefix", "u-", "--config", template}, 1, "flag provided but not defined"},
		{[]string{"sync", "-h"}, 0, "--only-group-members"},
		{[]string{"sync", "-h"}, 0, "--upg-prefix"},
	} {
		code, _, errs := runCLI(tc.args...)
		if code != tc.code || !strings.Contains(errs, tc.msg) {
			t.Errorf("%v: exit %d, stderr %q; want %d and %q", tc.args, code, errs, tc.code, tc.msg)
		}
	}
}

// --only-group-members plans user entries only for the group members
// (jdoe and asmith; nogid is in no group) and prints the summary line.
func TestOnlyGroupMembersFlag(t *testing.T) {
	template := templateCopy(t)
	code, out, errs := runCLI("sync", "--dry-run", "-v", "--only-group-members", "--config", template, "--fixture", "testdata/fixture.yaml")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	for _, want := range []string{"Users: only group members (2 found by DN lookup)\n", "users base not read", "add uid=jdoe,", "add uid=asmith,"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "CN=No Gid") {
		t.Errorf("nogid is in no group and must not be listed:\n%s", out)
	}
	// Default: the users base is read, and nogid is listed as ignored.
	_, out, _ = runCLI("sync", "--dry-run", "-v", "--config", template, "--fixture", "testdata/fixture.yaml")
	if strings.Contains(out, "Users: only group members") || !strings.Contains(out, "AD: 3 users") || !strings.Contains(out, "CN=No Gid") {
		t.Errorf("default run changed:\n%s", out)
	}
}

// --upg-prefix u- plans the same gidNumbers as the gid_from_upg config
// block of docs/operations/gid-from-upg.md with when: always.
func TestUPGPrefixEqualsConfigBlock(t *testing.T) {
	dir := t.TempDir()
	fix := filepath.Join(dir, "upg.yaml")
	if err := os.WriteFile(fix, []byte(`now: 2024-06-01T00:00:00Z
ad:
  users:
    - {guid: 00000000-0000-0000-0000-000000000001, dn: "CN=jdoe,OU=People,DC=example,DC=edu", attrs: {sAMAccountName: [jdoe], uidNumber: ["1001"], gidNumber: ["100"]}, userAccountControl: 512}
    - {guid: 00000000-0000-0000-0000-000000000002, dn: "CN=bob,OU=People,DC=example,DC=edu", attrs: {sAMAccountName: [bob], uidNumber: ["1002"]}, userAccountControl: 512}
    - {guid: 00000000-0000-0000-0000-000000000003, dn: "CN=carl,OU=People,DC=example,DC=edu", attrs: {sAMAccountName: [carl], uidNumber: ["1003"], gidNumber: ["100"]}, userAccountControl: 512}
  groups:
    - {guid: 00000000-0000-0000-0000-0000000000a1, dn: "CN=u-jdoe,OU=Groups,DC=example,DC=edu", attrs: {name: [u-jdoe], gidNumber: ["7001"]}, members: ["CN=jdoe,OU=People,DC=example,DC=edu"]}
    - {guid: 00000000-0000-0000-0000-0000000000a2, dn: "CN=U-Bob,OU=Groups,DC=example,DC=edu", attrs: {name: [U-Bob], gidNumber: ["7002"]}, members: ["CN=bob,OU=People,DC=example,DC=edu"]}
target: {entries: []}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(templateSrc)
	if err != nil {
		t.Fatal(err)
	}
	block := strings.NewReplacer(
		"required: [sAMAccountName, uidNumber, gidNumber]", "required: [sAMAccountName, uidNumber]",
		"    # gid_from_upg:  ", "    gid_from_upg:    ",
		"    #   group_name: 'u-{{ .uid }}'  ", "      group_name: 'u-{{ .uid }}'    ",
		"    #   when: missing  ", "      when: always     ",
	).Replace(string(data))
	if !strings.Contains(block, "\n    gid_from_upg:") || !strings.Contains(block, "when: always") {
		t.Fatal("template layout changed: update this test")
	}
	withBlock := filepath.Join(dir, "block.yaml")
	if err := os.WriteFile(withBlock, []byte(block), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"sync", "--dry-run", "-v", "--users", "--fixture", fix, "--config"}
	_, viaBlock, errs := runCLI(append(args, withBlock)...)
	code, viaFlag, errs2 := runCLI(append(args, templateCopy(t), "--upg-prefix", "u-")...)
	if code != 0 {
		t.Fatalf("exit %d: %s %s", code, errs, errs2)
	}
	note, rest, _ := strings.Cut(viaFlag, "\n")
	if note != "--upg-prefix u-: gidNumber from the AD group named u-<uid> (gid_from_upg, when: always)" || rest != viaBlock {
		t.Errorf("flag and config block differ\nflag:\n%s\nblock:\n%s", viaFlag, viaBlock)
	}
	for _, tc := range []struct{ name, want string }{
		{"group wins over the user's value", "add uid=jdoe,ou=people,dc=local [objectClass: account, posixAccount; cn: jdoe; gidNumber: 7001;"},
		{"no AD gidNumber, case-insensitive group", "add uid=bob,ou=people,dc=local [objectClass: account, posixAccount; cn: bob; gidNumber: 7002;"},
		{"no group, the user's value stays", "add uid=carl,ou=people,dc=local [objectClass: account, posixAccount; cn: carl; gidNumber: 100;"},
	} {
		if !strings.Contains(viaFlag, tc.want) {
			t.Errorf("%s: output lacks %q", tc.name, tc.want)
		}
	}
	// A config that has gid_from_upg already: the flag replaces it, and says so.
	_, out, _ := runCLI(append(args, withBlock, "--upg-prefix", "x-")...)
	if !strings.Contains(out, "replaces mapping.users.gid_from_upg of the config for this run") || !strings.Contains(out, "gidNumber: 100;") {
		t.Errorf("replace:\n%s", out)
	}
}
