package main

import (
	"context"
	"errors"
	"io"
	"mime/quotedprintable"
	"net/mail"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-ldap/ldap/v3"

	"github.com/dirkpetersen/dolly/internal/check"
	"github.com/dirkpetersen/dolly/internal/install"
	"github.com/dirkpetersen/dolly/internal/ldapconn"
	"github.com/dirkpetersen/dolly/internal/ldapfake"
	"github.com/dirkpetersen/dolly/internal/smtpfake"
)

type fakeSystemctl struct{ calls []string }

func (f *fakeSystemctl) Run(_ []string, args ...string) (string, error) {
	f.calls = append(f.calls, strings.Join(args, " "))
	return "", nil
}

// withInstallEnv points install and uninstall at a temp home with a fake
// systemctl, and runs the test from a temp working directory.
func withInstallEnv(t *testing.T) (install.Env, *fakeSystemctl, string) {
	t.Helper()
	root := t.TempDir()
	exe := filepath.Join(root, "dolly-build")
	if err := os.WriteFile(exe, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "run", "1"), 0o700); err != nil {
		t.Fatal(err)
	}
	env := install.Env{
		GOOS: "linux", Home: filepath.Join(root, "home"), Path: "/usr/bin", RuntimeDir: filepath.Join(root, "run", "1"),
		UID: 1, User: "svc", RunUser: filepath.Join(root, "run"), Systemd: func() bool { return true }, Executable: exe,
	}
	sc := &fakeSystemctl{}
	oldEnv, oldSC := installEnv, systemctl
	installEnv = func() (install.Env, error) { return env, nil }
	systemctl = sc
	wd, _ := os.Getwd()
	work := filepath.Join(root, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(work); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		installEnv, systemctl = oldEnv, oldSC
		_ = os.Chdir(wd)
	})
	return env, sc, work
}

func TestInstallCLI(t *testing.T) {
	env, sc, work := withInstallEnv(t)
	code, out, errs := runCLI("install", "--groups", "--config", "targets/a.yaml")
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
	abs := filepath.Join(work, "targets", "a.yaml")
	service, err := os.ReadFile(filepath.Join(env.Home, ".config", "systemd", "user", "dolly.service"))
	if err != nil {
		t.Fatal(err)
	}
	want := "ExecStart=%h/.local/bin/dolly sync --groups --config " + abs + "\n"
	if !strings.HasSuffix(string(service), want) || !strings.Contains(out, want) {
		t.Errorf("a relative --config must be made absolute in the unit:\n%s\n%s", service, out)
	}
	cfg, err := os.ReadFile(abs)
	if err != nil || !strings.HasPrefix(string(cfg), "# Dolly configuration template.") {
		t.Errorf("config from the embedded template: %v", err)
	}
	if strings.Join(sc.calls, ";") != "daemon-reload" {
		t.Errorf("systemctl calls %v", sc.calls)
	}

	code, out, _ = runCLI("uninstall")
	if code != 0 || strings.Join(sc.calls, ";") != "daemon-reload;disable --now dolly.timer;daemon-reload" {
		t.Errorf("uninstall: exit %d, calls %v\n%s", code, sc.calls, out)
	}
	if _, err := os.Stat(filepath.Join(env.Home, ".local", "bin", "dolly")); err == nil {
		t.Error("binary not removed")
	}
	if _, err := os.Stat(abs); err != nil {
		t.Error("uninstall must keep the config")
	}
}

// testConfig writes the template to a temp dir with the password files it
// names, and with the SMTP settings pointed at s (nil: notifications off).
func testConfig(t *testing.T, s *smtpfake.Server, edits ...string) string {
	t.Helper()
	data, err := os.ReadFile(templateSrc)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if s != nil {
		edits = append([]string{"smtp_host: mx.example.edu", "smtp_host: 127.0.0.1", "smtp_port: 25", "smtp_port: " + strconv.Itoa(s.Port)}, edits...)
	} else {
		edits = append([]string{"smtp_host: mx.example.edu", `smtp_host: ""`}, edits...)
	}
	for i := 0; i < len(edits); i += 2 {
		if !strings.Contains(text, edits[i]) {
			t.Fatalf("template lacks %q", edits[i])
		}
		text = strings.Replace(text, edits[i], edits[i+1], 1)
	}
	dir := t.TempDir()
	for _, f := range []string{"ad.secret", "ldap.secret"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("pw\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(dir, "dolly.yaml")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func withSMTPRoots(t *testing.T, s *smtpfake.Server) {
	t.Helper()
	old := notifyOptions
	notifyOptions.RootCAs = s.Pool()
	t.Cleanup(func() { notifyOptions = old })
}

// adFake and targetFake are directories matching the template's bases.
func adFake() *ldapfake.Dir {
	d := ldapfake.New()
	now := time.Now()
	d.Put("DC=example,DC=edu", map[string][]string{"objectClass": {"domain"}}, now)
	d.Put("OU=People,DC=example,DC=edu", map[string][]string{"objectClass": {"organizationalUnit"}}, now)
	d.Put("CN=Doe\\, Jane,OU=People,DC=example,DC=edu", map[string][]string{"objectClass": {"top", "person", "user"}, "objectCategory": {"person"}, "uid": {"jdoe"}}, now)
	d.Put("OU=Groups,DC=example,DC=edu", map[string][]string{"objectClass": {"organizationalUnit"}}, now)
	d.Put("CN=hpc,OU=Groups,DC=example,DC=edu", map[string][]string{"objectClass": {"top", "group"}}, now)
	return d
}

func targetFake() *ldapfake.Dir {
	d := ldapfake.New()
	now := time.Now()
	d.Put("dc=local", map[string][]string{"objectClass": {"domain"}}, now)
	d.Put("ou=people,dc=local", map[string][]string{"objectClass": {"organizationalUnit"}}, now)
	d.Put("uid=jdoe,ou=people,dc=local", map[string][]string{"objectClass": {"account"}, "uid": {"jdoe"}}, now)
	d.Put("uid=asmith,ou=people,dc=local", map[string][]string{"objectClass": {"account"}, "uid": {"asmith"}}, now)
	d.Put("ou=group,dc=local", map[string][]string{"objectClass": {"organizationalUnit"}}, now)
	return d
}

// withCheckDial serves dolly check's connections from fakes by URL; a URL
// without a fake fails like an unreachable server.
func withCheckDial(t *testing.T, dirs map[string]*ldapfake.Dir) *[]ldapconn.Options {
	t.Helper()
	var dialed []ldapconn.Options
	old := checkDial
	checkDial = func(_ context.Context, o ldapconn.Options) (check.Conn, error) {
		dialed = append(dialed, o)
		if o.Password != "pw" {
			return nil, errors.New("wrong password")
		}
		if d := dirs[o.URL]; d != nil {
			return d, nil
		}
		return nil, errors.New("dial tcp: connection refused")
	}
	t.Cleanup(func() { checkDial = old })
	return &dialed
}

func TestCheck(t *testing.T) {
	s := smtpfake.Start(t, smtpfake.Options{StartTLS: true})
	withSMTPRoots(t, s)
	cfg := testConfig(t, s)
	tgt := targetFake()
	dialed := withCheckDial(t, map[string]*ldapfake.Dir{"ldaps://dc02.example.edu:636": adFake(), "ldap://ldap.example.edu:389": tgt})

	code, out, errs := runCLI("check", "--config", cfg)
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
	for _, want := range []string{
		"✓ loaded and valid",
		"✓ source.bind_password_file ",
		"! ldaps://dc01.example.edu:636: dial tcp: connection refused (runs fail over to ldaps://dc02.example.edu:636)",
		"✓ ldaps://dc02.example.edu:636: LDAPS and bind as CN=svc-dolly",
		"✓ users base OU=People,DC=example,DC=edu: paged search with (&(objectClass=user)(objectCategory=person)) returns entries (answered by ldaps://dc02.example.edu:636)",
		"✓ groups base OU=Groups,DC=example,DC=edu: paged search",
		"✓ StartTLS and bind as cn=admin,dc=local",
		"✓ groups_base ou=group,dc=local exists; all 1 entries readable",
		"✓ users_base ou=people,dc=local: readable (uid lookup found uid=",
		"✓ users_base ou=people,dc=local: all 3 entries readable in one search",
		"✓ state_base ou=dolly,dc=local does not exist yet; the first real run creates it under dc=local",
		"✓ connected to 127.0.0.1:",
		"✓ STARTTLS (certificate verified for 127.0.0.1)",
		"✓ SMTP: no authentication (username empty)",
		"- no mail sent (use --send-test-mail",
		"All 15 checks passed, with 1 warning.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if len(*dialed) != 3 {
		t.Errorf("dialed %d servers, want both DCs and the target", len(*dialed))
	}
	if len(s.Messages()) != 0 || s.Received("AUTH") {
		t.Errorf("check must not send mail or AUTH without a username: %v", s.Commands())
	}
	for _, c := range tgt.Calls {
		if !strings.HasPrefix(c, "search ") {
			t.Errorf("check wrote to the target: %s", c)
		}
	}

	t.Run("no DC answers", func(t *testing.T) {
		withCheckDial(t, map[string]*ldapfake.Dir{"ldap://ldap.example.edu:389": targetFake()})
		code, out, _ := runCLI("check", "--config", testConfig(t, nil))
		if code != 1 || !strings.Contains(out, "✗ ldaps://dc02.example.edu:636: dial tcp") || !strings.Contains(out, "✗ no domain controller could be used") {
			t.Errorf("exit %d\n%s", code, out)
		}
	})
	t.Run("all good", func(t *testing.T) {
		withCheckDial(t, map[string]*ldapfake.Dir{"ldaps://dc01.example.edu:636": adFake(), "ldaps://dc02.example.edu:636": adFake(), "ldap://ldap.example.edu:389": targetFake()})
		code, out, _ := runCLI("check", "--config", testConfig(t, nil))
		if code != 0 || !strings.Contains(out, "All ") || !strings.Contains(out, "- notifications disabled (notify.smtp_host is empty)") {
			t.Errorf("exit %d\n%s", code, out)
		}
	})
	t.Run("size limit", func(t *testing.T) {
		tgt := targetFake()
		tgt.SizeLimit = 2 // users_base has 3 entries, groups_base 1
		withCheckDial(t, map[string]*ldapfake.Dir{"ldaps://dc01.example.edu:636": adFake(), "ldap://ldap.example.edu:389": tgt})
		cfg := testConfig(t, nil)
		code, out, _ := runCLI("check", "--config", cfg)
		if code != 1 || !strings.Contains(out, "✗ users_base ou=people,dc=local: the server truncated a full read after 2 entries") ||
			!strings.Contains(out, `olcLimits: dn.exact="cn=admin,dc=local" size=unlimited time=unlimited`) {
			t.Errorf("exit %d\n%s", code, out)
		}
		code, out, _ = runCLI("check", "--groups", "--config", cfg)
		if code != 0 || !strings.Contains(out, "! users_base ou=people,dc=local: the server truncates a full read after 2 entries (sizeLimitExceeded); fine for groups-only runs") {
			t.Errorf("--groups: exit %d\n%s", code, out)
		}
	})
	t.Run("missing groups_base", func(t *testing.T) {
		tgt := ldapfake.New()
		tgt.Put("dc=local", map[string][]string{"objectClass": {"domain"}}, time.Now())
		withCheckDial(t, map[string]*ldapfake.Dir{"ldaps://dc01.example.edu:636": adFake(), "ldap://ldap.example.edu:389": tgt})
		code, out, _ := runCLI("check", "--config", testConfig(t, nil))
		if code != 1 || !strings.Contains(out, "✗ groups_base ou=group,dc=local does not exist") || !strings.Contains(out, "✗ users_base ou=people,dc=local does not exist") {
			t.Errorf("exit %d\n%s", code, out)
		}
	})
	t.Run("invalid credentials stop at the first DC", func(t *testing.T) {
		var tried []string
		old := checkDial
		checkDial = func(_ context.Context, o ldapconn.Options) (check.Conn, error) {
			tried = append(tried, o.URL)
			return nil, ldapfake.Error(ldap.LDAPResultInvalidCredentials, "Invalid Credentials")
		}
		defer func() { checkDial = old }()
		code, out, _ := runCLI("check", "--config", testConfig(t, nil))
		if code != 1 || !strings.Contains(out, "ldaps://dc02.example.edu:636: not tried") || tried[0] != "ldaps://dc01.example.edu:636" || tried[1] != "ldap://ldap.example.edu:389" {
			t.Errorf("exit %d, tried %v\n%s", code, tried, out)
		}
	})
	t.Run("send test mail with AUTH", func(t *testing.T) {
		s := smtpfake.Start(t, smtpfake.Options{StartTLS: true, User: "dolly", Password: "smtp-pw"})
		withSMTPRoots(t, s)
		withCheckDial(t, map[string]*ldapfake.Dir{"ldaps://dc01.example.edu:636": adFake(), "ldap://ldap.example.edu:389": targetFake()})
		cfg := testConfig(t, s, `username: ""                        # optional SMTP auth`, "username: dolly", `password_file: ""`, "password_file: smtp.secret")
		if err := os.WriteFile(filepath.Join(filepath.Dir(cfg), "smtp.secret"), []byte("smtp-pw\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		code, out, _ := runCLI("check", "--send-test-mail", "--config", cfg)
		if code != 0 || !strings.Contains(out, "✓ AUTH PLAIN as dolly") || !strings.Contains(out, "✓ test mail sent to ldap-admins@example.edu") {
			t.Errorf("exit %d\n%s", code, out)
		}
		if m := s.Messages(); len(m) != 1 || m[0].Auth != "dolly" || m[0].To[0] != "ldap-admins@example.edu" {
			t.Errorf("messages %+v", m)
		}
	})
}

// mailBody decodes a received message.
func mailBody(t *testing.T, m smtpfake.Message) (subject, body string) {
	t.Helper()
	msg, err := mail.ReadMessage(strings.NewReader(m.Data))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(quotedprintable.NewReader(msg.Body))
	return msg.Header.Get("Subject"), string(b)
}

// Real runs send at most one mail each, decided from cn=status: the first
// warnings list, a new failure, silence while it persists, and recovery.
func TestRealRunNotifications(t *testing.T) {
	s := smtpfake.Start(t, smtpfake.Options{StartTLS: true})
	withSMTPRoots(t, s)
	cfg := testConfig(t, s)
	dir := ldapfake.New()
	withFake(t, dir, "", false)
	withSnapshots(t, "testdata/fixture.yaml")

	// 1. Success with a first warnings list (a conflict, an ignored user):
	// one mail, with the run's output.
	code, out, errs := runCLI("sync", "--config", cfg)
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
	msgs := s.Messages()
	if len(msgs) != 1 {
		t.Fatalf("%d mails after run 1\n%s", len(msgs), errs)
	}
	subj, body := mailBody(t, msgs[0])
	if !strings.Contains(subj, "[dolly] ") || !strings.Contains(subj, "dolly sync warnings changed") {
		t.Errorf("subject %q", subj)
	}
	for _, want := range []string{"Outcome:  succeeded", "list of warnings", "Plan: dolly sync (users and groups)", "conflict     cn=hpc-users,ou=group,dc=local", "Result: 6 applied", "connected to"} {
		if want == "connected to" {
			continue // readSnapshots is faked, so no progress lines
		}
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %q:\n%s", want, body)
		}
	}
	st := statusNotes(dir)
	if !strings.Contains(st, "last-notified=") || !strings.Contains(st, "warnings-hash=") || strings.Contains(st, "notify-error") {
		t.Errorf("status:\n%s", st)
	}

	// 2. Same warnings, success: no mail.
	if code, _, errs := runCLI("sync", "--config", cfg); code != 0 || len(s.Messages()) != 1 {
		t.Fatalf("run 2: exit %d, %d mails\n%s", code, len(s.Messages()), errs)
	}

	// 3. Guard trip: a failure, mailed once.
	empty := filepath.Join(t.TempDir(), "empty.yaml")
	if err := os.WriteFile(empty, []byte("ad: {}\ntarget: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	withSnapshots(t, empty)
	if code, _, _ := runCLI("sync", "--config", cfg); code != 2 || len(s.Messages()) != 2 {
		t.Fatalf("run 3: exit %d, %d mails", code, len(s.Messages()))
	}
	subj, body = mailBody(t, s.Messages()[1])
	if !strings.Contains(subj, "dolly sync FAILED: mass-deletion guard tripped") || !strings.Contains(body, "Outcome:  FAILED") {
		t.Errorf("failure mail %q\n%s", subj, body)
	}

	// 4. Still failing, reminder not due: no mail.
	if code, _, _ := runCLI("sync", "--config", cfg); code != 2 || len(s.Messages()) != 2 {
		t.Fatalf("run 4: exit %d, %d mails", code, len(s.Messages()))
	}

	// 5. Recovery: one mail.
	withSnapshots(t, "testdata/fixture.yaml")
	if code, _, _ := runCLI("sync", "--config", cfg); code != 0 || len(s.Messages()) != 3 {
		t.Fatalf("run 5: exit %d, %d mails", code, len(s.Messages()))
	}
	if subj, _ = mailBody(t, s.Messages()[2]); !strings.Contains(subj, "dolly sync recovered") {
		t.Errorf("recovery subject %q", subj)
	}

	// 6. A held lock: no mail, no status.
	before := statusNotes(dir)
	dir.Put(lockDN, map[string][]string{"objectClass": {"organizationalRole"}, "cn": {"lock"}, "description": {"host=other"}}, time.Now())
	if code, _, _ := runCLI("sync", "--config", cfg); code != 0 || len(s.Messages()) != 3 || statusNotes(dir) != before {
		t.Errorf("held lock: exit %d, %d mails", code, len(s.Messages()))
	}
}

// A mail failure is logged and recorded, and never changes the exit code.
func TestMailFailureKeepsExitCode(t *testing.T) {
	s := smtpfake.Start(t, smtpfake.Options{StartTLS: true, RejectData: true})
	withSMTPRoots(t, s)
	cfg := testConfig(t, s)
	dir := ldapfake.New()
	withFake(t, dir, "", false)
	withSnapshots(t, "testdata/fixture.yaml")
	code, _, errs := runCLI("sync", "--config", cfg)
	if code != 0 || !strings.Contains(errs, "sending the notification to ldap-admins@example.edu failed") {
		t.Fatalf("exit %d\n%s", code, errs)
	}
	st := statusNotes(dir)
	if !strings.Contains(st, "notify-error=") || strings.Contains(st, "last-notified=") || strings.Contains(st, "warnings-hash") {
		t.Errorf("status:\n%s", st)
	}
	// The next run retries (the hash wasn't stored); a failed run keeps exit 1.
	dir.Fail = func(op, dn string, _ any) error {
		if op == "add" && dn == "uid=jdoe,ou=people,dc=local" {
			return ldapfake.Error(ldap.LDAPResultObjectClassViolation, "injected")
		}
		return nil
	}
	if code, _, _ := runCLI("sync", "--config", cfg); code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	if n := len(s.Commands()); n == 0 {
		t.Error("no retry")
	}
}

// Breaking a stale lock is mailed, even when the run itself succeeds.
func TestStaleLockBreakIsMailed(t *testing.T) {
	s := smtpfake.Start(t, smtpfake.Options{StartTLS: true})
	withSMTPRoots(t, s)
	cfg := testConfig(t, s)
	dir := fakeWithLock(2 * time.Hour) // no started= note: judged by createTimestamp
	withFake(t, dir, "", false)
	withSnapshots(t, "testdata/fixture.yaml")
	code, _, errs := runCLI("sync", "--config", cfg)
	if code != 0 || !strings.Contains(errs, "broke a stale run lock") {
		t.Fatalf("exit %d\n%s", code, errs)
	}
	msgs := s.Messages()
	if len(msgs) != 1 {
		t.Fatalf("%d mails", len(msgs))
	}
	subj, body := mailBody(t, msgs[0])
	if !strings.Contains(subj, "dolly sync broke a stale run lock") || !strings.Contains(body, "held by host=other pid=42") {
		t.Errorf("subject %q\n%s", subj, body)
	}
}
