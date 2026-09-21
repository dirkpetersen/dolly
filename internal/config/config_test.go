package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dirkpetersen/dolly"
)

// The checked-in template must always load and validate. Validation is
// structural, so the password and CA files it names need not exist.
func TestTemplateIsValid(t *testing.T) {
	c, err := Parse(dolly.ConfigTemplate, "/home/u/.config/dolly/dolly.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if c.Source.BindPasswordFile != "/home/u/.config/dolly/ad.secret" || c.Target.CAFile != "/home/u/.config/dolly/ldap-ca.pem" {
		t.Errorf("relative paths not resolved: %q %q", c.Source.BindPasswordFile, c.Target.CAFile)
	}
	if c.Sync.LockTTL.Duration != time.Hour || c.Sync.RunTimeout.Duration != 45*time.Minute || c.Notify.RemindEvery.Duration != 24*time.Hour {
		t.Errorf("durations: %+v", c.Sync)
	}
	if !c.Mapping.Groups.HasMember() || !c.Mapping.Groups.HasMemberUID() || !c.Mapping.Groups.FlattenNested {
		t.Error("group mapping")
	}
	if !c.Mapping.Users.IsCreateOnly("loginshell") || c.Notify.On != "failure" || c.Source.PageSize != 500 {
		t.Error("template values")
	}
	for name, src := range c.Mapping.Users.Attributes {
		if _, err := CompileValue(name, src); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// Loading from disk works with the template unchanged (no inline password,
// so any file mode is fine).
func TestLoadTemplateFromDisk(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "dolly.yaml")
	if err := os.WriteFile(p, dolly.ConfigTemplate, 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Path != p {
		t.Errorf("Path = %s", c.Path)
	}
}

// edit returns the template with old replaced by new (must match once).
func edit(t *testing.T, pairs ...string) []byte {
	t.Helper()
	s := string(dolly.ConfigTemplate)
	for i := 0; i < len(pairs); i += 2 {
		if strings.Count(s, pairs[i]) != 1 {
			t.Fatalf("template must contain %q exactly once", pairs[i])
		}
		s = strings.Replace(s, pairs[i], pairs[i+1], 1)
	}
	return []byte(s)
}

func TestValidationErrors(t *testing.T) {
	for _, tc := range []struct {
		name  string
		edits []string
		want  string
	}{
		{"both passwords", []string{`bind_password: ""                   # inline password, or use bind_password_file (set only one)
  bind_password_file: ad.secret`, `bind_password: "x"
  bind_password_file: ad.secret`}, "source.bind_password and source.bind_password_file are both set"},
		{"no password", []string{"bind_password_file: ldap.secret", "bind_password_file: \"\""}, "target.bind_password or target.bind_password_file: one is required"},
		{"run_timeout", []string{"run_timeout: 45m", "run_timeout: 60m"}, "sync.run_timeout (1h0m0s) must be shorter than sync.lock_ttl"},
		{"bad duration", []string{"lock_ttl: 60m", "lock_ttl: 1d"}, `invalid duration "1d"`},
		{"membership attribute", []string{"- attribute: memberUid          # bare uid; RFC 2307 servers: list only this one", "- attribute: uniqueMember"}, `must be "member" or "memberUid"`},
		{"member needs placeholder", []string{"empty_group_member: cn=empty,dc=local", `empty_group_member: ""`}, "target.empty_group_member: required when"},
		{"bad template", []string{`'{{ or .loginShell "/bin/bash" }}'`, `'{{ or .loginShell }'`}, "mapping.users.attributes.loginShell"},
		{"not an attribute", []string{"gecos: gecos", "gecos: two words"}, "neither an AD attribute name nor a Go template"},
		{"required is absolute", []string{"required: [uid, uidNumber, gidNumber]", "required: [uid, uidNumber]"}, "mapping.users.required: must include gidNumber"},
		{"unknown key", []string{"page_size: 500", "page_size: 500\n  pagesize: 1"}, "field pagesize not found"},
		{"bad filter", []string{"filter: (objectClass=group)", "filter: (objectClass=group"}, "source.groups.filter"},
		{"same bases", []string{"groups_base: ou=group,dc=local", "groups_base: OU=People,dc=local"}, "target.users_base and target.groups_base must differ"},
		{"state_base is groups_base", []string{"state_base: ou=dolly,dc=local", "state_base: OU=Group,dc=local"}, "target.state_base and target.groups_base must differ"},
		{"state_base is users_base", []string{"state_base: ou=dolly,dc=local", "state_base: ou=people,DC=local"}, "target.state_base and target.users_base must differ"},
		{"groups_base inside state_base", []string{"groups_base: ou=group,dc=local", "groups_base: ou=group,ou=dolly,dc=local"}, "target.groups_base must not be inside target.state_base"},
		{"users_base inside state_base", []string{"users_base: ou=people,dc=local", "users_base: ou=people,ou=Dolly,dc=local"}, "target.users_base must not be inside target.state_base"},
		{"bad DN", []string{"users_base: ou=people,dc=local", "users_base: people"}, "target.users_base: invalid DN"},
		{"bad url", []string{"url: ldap://ldap.example.edu:389", "url: http://ldap.example.edu"}, "scheme must be ldap:// or ldaps://"},
		{"notify.on", []string{"on: failure ", "on: sometimes "}, "notify.on: must be failure, changes, or always"},
		{"smtp username without password", []string{`username: ""                        # optional SMTP auth`, `username: dolly`}, "notify.password: set password or password_file when username is set"},
		{"smtp password without username", []string{`password: ""                        # inline, or use password_file (set only one)`, `password: "x"`}, "notify.username: required when a password is set"},
		{"smtps with start_tls", []string{"smtp_port: 25", "smtp_port: 465"}, "notify.start_tls: port 465 uses implicit TLS"},
		{"bad from", []string{`from: "Dolly <dolly-noreply@example.edu>"`, `from: "not an address"`}, "notify.from:"},
		{"bad to", []string{`to: [ldap-admins@example.edu]`, `to: [ldap-admins]`}, "notify.to[0]:"},
		{"create_only unmapped", []string{"create_only: [loginShell, homeDirectory]", "create_only: [loginShell, mail]"}, `"mail" is not in mapping.users.attributes`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(edit(t, tc.edits...), "/x/dolly.yaml")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v\nwant it to contain %q", err, tc.want)
			}
		})
	}
}

func TestMemberUIDOnlyNeedsNoPlaceholder(t *testing.T) {
	data := edit(t,
		"      - attribute: member             # full DN of the target user\n", "",
		"empty_group_member: cn=empty,dc=local", `empty_group_member: ""`)
	c, err := Parse(data, "/x/dolly.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if c.Mapping.Groups.HasMember() || !c.Mapping.Groups.HasMemberUID() {
		t.Error("membership")
	}
}

func TestInlinePasswordNeedsPrivateFile(t *testing.T) {
	data := edit(t, `bind_password: ""                   # inline password, or use bind_password_file (set only one)
  bind_password_file: ldap.secret`, `bind_password: "s3cret"
  bind_password_file: ""`)
	dir := t.TempDir()
	p := filepath.Join(dir, "dolly.yaml")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Errorf("mode 0644 with inline password: %v", err)
	}
	for _, mode := range []os.FileMode{0o600, 0o400} {
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		c, err := Load(p)
		if err != nil {
			t.Fatalf("mode %o: %v", mode, err)
		}
		if pw, _ := c.Target.Password(); pw != "s3cret" {
			t.Errorf("password = %q", pw)
		}
	}
}

func TestPasswordFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ad.secret"), []byte("pw\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Parse(dolly.ConfigTemplate, filepath.Join(dir, "dolly.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if pw, err := c.Source.Password(); err != nil || pw != "pw" {
		t.Errorf("Password = %q, %v", pw, err)
	}
	if _, err := c.Target.Password(); err == nil {
		t.Error("missing password file must be an error on use")
	}
}

func TestFind(t *testing.T) {
	has := func(files ...string) func(string) bool {
		return func(p string) bool {
			for _, f := range files {
				if f == p {
					return true
				}
			}
			return false
		}
	}
	for _, tc := range []struct {
		name, explicit, xdg string
		files               []string
		want, err           string
	}{
		{"explicit wins", "/etc/other.yaml", "/x", []string{"/etc/other.yaml", "dolly.yaml"}, "/etc/other.yaml", ""},
		{"explicit missing", "/nope.yaml", "", []string{"dolly.yaml"}, "", "does not exist"},
		{"cwd before xdg", "", "/x", []string{"dolly.yaml", "/x/dolly/dolly.yaml"}, "dolly.yaml", ""},
		{"xdg", "", "/x", []string{"/x/dolly/dolly.yaml", "/home/u/.config/dolly/dolly.yaml"}, "/x/dolly/dolly.yaml", ""},
		{"home fallback", "", "", []string{"/home/u/.config/dolly/dolly.yaml"}, "/home/u/.config/dolly/dolly.yaml", ""},
		{"relative xdg ignored", "", "rel", []string{"rel/dolly/dolly.yaml", "/home/u/.config/dolly/dolly.yaml"}, "/home/u/.config/dolly/dolly.yaml", ""},
		{"nothing", "", "", nil, "", "no config found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := find(tc.explicit, tc.xdg, "/home/u", has(tc.files...))
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Errorf("err = %v, want %q", err, tc.err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Errorf("find = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

// sync.require_member_on_target was removed (the rule is always on). The
// unknown-key error must say so instead of just "not found".
func TestRemovedRequireMemberOnTarget(t *testing.T) {
	for _, v := range []string{"true", "false"} {
		_, err := Parse(edit(t, "  network_timeout: 30s ", "  require_member_on_target: "+v+"\n  network_timeout: 30s "), "/x/dolly.yaml")
		if err == nil || !strings.Contains(err.Error(), "field require_member_on_target not found") ||
			!strings.Contains(err.Error(), "was removed: a member is now always added to a target group only if its entry exists under users_base") {
			t.Errorf("%s: err = %v", v, err)
		}
	}
	// Other unknown keys keep yaml.v3's plain message.
	_, err := Parse(edit(t, "  network_timeout: 30s ", "  bogus: 1\n  network_timeout: 30s "), "/x/dolly.yaml")
	if err == nil || !strings.Contains(err.Error(), "field bogus not found") || strings.Contains(err.Error(), "removed") {
		t.Errorf("bogus: err = %v", err)
	}
}

// The users required list must hold the AD attributes uid, uidNumber, and
// gidNumber are mapped from, whatever they are called.
func TestRequiredFollowsUIDSource(t *testing.T) {
	sam := []string{"      uid: uid\n      cn: uid\n", "      uid: sAMAccountName\n      cn: sAMAccountName\n"}
	_, err := Parse(edit(t, sam...), "/x/dolly.yaml")
	if err == nil || !strings.Contains(err.Error(), "mapping.users.required: must include sAMAccountName") {
		t.Errorf("uid from sAMAccountName, required without it: %v", err)
	}
	c, err := Parse(edit(t, append(sam, "required: [uid, uidNumber, gidNumber]", "required: [sAMAccountName, uidNumber, gidNumber]")...), "/x/dolly.yaml")
	if err != nil {
		t.Fatalf("required with sAMAccountName: %v", err)
	}
	if got := c.Mapping.Users.Required; strings.Join(got, ",") != "sAMAccountName,uidNumber,gidNumber" {
		t.Errorf("required = %v", got)
	}
	_, err = Parse(edit(t, "      uidNumber: uidNumber\n", "      uidNumber: employeeNumber\n"), "/x/dolly.yaml")
	if err == nil || !strings.Contains(err.Error(), "must include employeeNumber") {
		t.Errorf("uidNumber from employeeNumber: %v", err)
	}
	_, err = Parse(edit(t, "      uidNumber: uidNumber\n      gidNumber: gidNumber\n", "      uidNumber: uidNumber\n"), "/x/dolly.yaml")
	if err == nil || !strings.Contains(err.Error(), "gidNumber is required") {
		t.Errorf("unmapped gidNumber: %v", err)
	}
}

func TestValueSources(t *testing.T) {
	for src, want := range map[string]string{
		"uid": "uid",
		`{{ or .unixHomeDirectory (printf "/home/%s" .uid) }}`: "unixHomeDirectory,uid",
		`{{ if .a }}{{ .b }}{{ else }}{{ .c }}{{ end }}`:       "a,b,c",
		`{{ with .x }}{{ . }}{{ end }}{{ .X }}`:                "x",
		`static`:                                               "static",
	} {
		v, err := CompileValue("n", src)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(v.Sources(), ","); got != want {
			t.Errorf("%s: sources = %q, want %q", src, got, want)
		}
	}
}

func TestStateBaseInsideBaseIsValid(t *testing.T) {
	for _, sb := range []string{"ou=dolly,ou=group,dc=local", "ou=dolly,ou=people,dc=local"} {
		if _, err := Parse(edit(t, "state_base: ou=dolly,dc=local", "state_base: "+sb), "/x/dolly.yaml"); err != nil {
			t.Errorf("state_base %s: %v", sb, err)
		}
	}
}

// SMTP without authentication is plain relay and valid with or without
// StartTLS; AUTH needs TLS (StartTLS or implicit TLS on port 465).
func TestSMTPAuthRules(t *testing.T) {
	const notifyAuth = "start_tls: true\n  username: \"\"                        # optional SMTP auth\n  password: \"\"                        # inline, or use password_file (set only one)\n  password_file: \"\""
	for _, tc := range []struct {
		name, repl, want string // want "" means valid
	}{
		{"no auth, StartTLS", "start_tls: true", ""},
		{"no auth, plain relay", "start_tls: false", ""},
		{"auth over StartTLS", "start_tls: true\n  username: dolly\n  password_file: smtp.secret", ""},
		{"auth without TLS", "start_tls: false\n  username: dolly\n  password_file: smtp.secret", "notify.username: SMTP AUTH is only sent over TLS"},
		{"username without password", "start_tls: true\n  username: dolly", "notify.password: set password or password_file when username is set"},
		{"password without username", "start_tls: true\n  password_file: smtp.secret", "notify.username: required when a password is set"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(edit(t, notifyAuth, tc.repl), "/x/dolly.yaml")
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("want valid, got %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Errorf("error = %v\nwant it to contain %q", err, tc.want)
			}
		})
	}
	// Implicit TLS on port 465 allows AUTH without start_tls.
	_, err := Parse(edit(t, notifyAuth, "start_tls: false\n  username: dolly\n  password_file: smtp.secret", "smtp_port: 25", "smtp_port: 465"), "/x/dolly.yaml")
	if err != nil {
		t.Errorf("port 465 with AUTH: %v", err)
	}
}
