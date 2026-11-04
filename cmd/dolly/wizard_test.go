package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/go-ldap/ldap/v3"

	"github.com/dirkpetersen/dolly"
	"github.com/dirkpetersen/dolly/internal/check"
	"github.com/dirkpetersen/dolly/internal/config"
	"github.com/dirkpetersen/dolly/internal/ldapconn"
	"github.com/dirkpetersen/dolly/internal/ldapfake"
)

const (
	adPw  = "s3cret-AD-pw"
	tgtPw = "s3cret-LDAP-pw"
)

// scriptIO answers the wizard's prompts in order; running out of answers
// is EOF.
type scriptIO struct {
	out     io.Writer
	answers []string
}

func (s *scriptIO) next(prompt string) (string, error) {
	fmt.Fprint(s.out, prompt)
	if len(s.answers) == 0 {
		return "", errAbort
	}
	a := s.answers[0]
	s.answers = s.answers[1:]
	fmt.Fprintln(s.out) // the user's Enter; the answer itself isn't echoed (passwords)
	return a, nil
}

func (s *scriptIO) Line(p string) (string, error)     { return s.next(p) }
func (s *scriptIO) Password(p string) (string, error) { return s.next(p) }

// wizardWorld is an AD and a target, each behind its own in-process TLS
// listener (for the certificate fetch) and an ldapfake (for the binds,
// served through checkDial).
type wizardWorld struct {
	adURL, tgtURL string
	adFP, tgtFP   string
	dials         *[]ldapconn.Options
}

func newWizardWorld(t *testing.T) *wizardWorld {
	t.Helper()
	adAddr, adLeaf := ldapsServer(t)
	tgtAddr, tgtLeaf := ldapsServer(t)
	w := &wizardWorld{adURL: "ldaps://" + adAddr, tgtURL: "ldaps://" + tgtAddr,
		adFP: ldapconn.Fingerprint(adLeaf), tgtFP: ldapconn.Fingerprint(tgtLeaf), dials: &[]ldapconn.Options{}}
	now := time.Now()
	ad := ldapfake.New()
	ad.Put("OU=People,DC=example,DC=edu", map[string][]string{"objectClass": {"organizationalUnit"}}, now)
	ad.Put("CN=jdoe,OU=People,DC=example,DC=edu", map[string][]string{"objectClass": {"user"}, "objectCategory": {"person"}}, now)
	ad.Put("OU=Groups,DC=example,DC=edu", map[string][]string{"objectClass": {"organizationalUnit"}}, now)
	tgt := ldapfake.New()
	tgt.Put("", map[string][]string{"namingContexts": {"dc=local"}}, now)
	tgt.Put("ou=people,dc=local", map[string][]string{"objectClass": {"organizationalUnit"}}, now)
	old := checkDial
	checkDial = func(_ context.Context, o ldapconn.Options) (check.Conn, error) {
		*w.dials = append(*w.dials, o)
		switch {
		case o.URL == w.adURL && o.Password == adPw:
			return ad, nil
		case o.URL == w.tgtURL && o.Password == tgtPw:
			return tgt, nil
		case o.URL == w.adURL || o.URL == w.tgtURL:
			return nil, ldap.NewError(ldap.LDAPResultInvalidCredentials, errors.New("Invalid Credentials"))
		}
		return nil, errors.New("dial tcp: connection refused")
	}
	t.Cleanup(func() { checkDial = old })
	return w
}

// runWizard runs dolly config on a terminal with scripted answers.
func runWizard(t *testing.T, path string, answers ...string) (int, string, string) {
	t.Helper()
	withFake(t, ldapfake.New(), "", true)
	old := newWizardIO
	newWizardIO = func(out io.Writer) wizardIO { return &scriptIO{out: out, answers: answers} }
	t.Cleanup(func() { newWizardIO = old })
	return runCLI("config", "--config", path)
}

// The answers for a new config: trust both servers, bind, accept the
// target base defaults, RFC 2307 with sAMAccountName.
func (w *wizardWorld) newAnswers() []string {
	return []string{
		w.adURL, "", // 1: Enter trusts
		"svc-dolly@example.edu", adPw, // 2
		"OU=People,DC=example,DC=edu", "OU=Groups,DC=example,DC=edu", // 3
		w.tgtURL, "", // 4
		"cn=admin,dc=local", tgtPw, // 5
		"", "", "", // 6: the defaults under dc=local
		"rfc2307", "sAMAccountName", // 7
		"y", // write
	}
}

// written reports a run that wrote the files; the check after it fails in
// the test world (no groups, no target bases, no SMTP), so the exit is 1.
func written(code int, errs string) bool {
	return code == 1 && strings.Contains(errs, "files written; dolly check found")
}

func noSecrets(t *testing.T, texts ...string) {
	t.Helper()
	for _, s := range texts {
		if strings.Contains(s, adPw) || strings.Contains(s, tgtPw) {
			t.Errorf("a password is in the output or config:\n%s", s)
		}
	}
}

func TestConfigWizardNew(t *testing.T) {
	w := newWizardWorld(t)
	path := filepath.Join(t.TempDir(), "conf", "dolly.yaml")
	code, out, errs := runWizard(t, path, w.newAnswers()...)
	if !written(code, errs) {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
	for _, want := range []string{"Creating " + path, "[1/7] Source servers", "[7/7] Group schema", "Trust these certificates (they become tls_fingerprint, and ca_file is removed)? [Y/n]",
		"✓ bind as svc-dolly@example.edu works", "✓ users base OU=People,DC=example,DC=edu returns entries",
		"! groups base OU=Groups,DC=example,DC=edu: no entries match", "users_base [ou=people,dc=local]",
		"✓ users_base ou=people,dc=local exists", "groups_base ou=group,dc=local does not exist yet",
		"Write " + path + "? [y/N]", "Running dolly check:", "off (smtp_host empty); edit the notify section by hand to get mail"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	data, _ := os.ReadFile(path)
	noSecrets(t, out, errs, string(data))
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(cfg.Source.URLs, ",") != w.adURL || strings.Join(cfg.Source.TLSFingerprint, ",") != w.adFP || cfg.Source.CAFile != "" ||
		cfg.Target.URL != w.tgtURL || cfg.Target.StartTLS || strings.Join(cfg.Target.TLSFingerprint, ",") != w.tgtFP || cfg.Target.CAFile != "" ||
		cfg.Target.UsersBase != "ou=people,dc=local" || cfg.Target.StateBase != "ou=dolly,dc=local" ||
		cfg.Mapping.Users.Attributes["uid"] != "sAMAccountName" || cfg.Mapping.Users.Required[0] != "sAMAccountName" ||
		!strings.Contains(cfg.Mapping.Users.Attributes["homeDirectory"], ".sAMAccountName") ||
		strings.Join(cfg.Mapping.Groups.ObjectClasses, ",") != "posixGroup" || cfg.Mapping.Groups.HasMember() || cfg.Notify.SMTPHost != "" {
		t.Errorf("config:\n%s", data)
	}
	if pw, err := cfg.Source.Password(); err != nil || pw != adPw {
		t.Errorf("source password %v", err)
	}
	for _, f := range []string{path, filepath.Join(filepath.Dir(path), "ad.secret"), filepath.Join(filepath.Dir(path), "ldap.secret")} {
		if st, err := os.Stat(f); err != nil || st.Mode().Perm() != 0o600 {
			t.Errorf("%s: %v %v", f, err, st)
		}
	}
	if st, _ := os.Stat(filepath.Dir(path)); st.Mode().Perm() != 0o700 {
		t.Errorf("dir mode %v", st.Mode())
	}
	// The pins were used for the binds.
	for _, o := range *w.dials {
		if len(o.Pins) == 0 && (o.URL == w.adURL || o.URL == w.tgtURL) {
			t.Errorf("dial without pins: %+v", o.URL)
		}
	}

	// Editing: Enter keeps every value; the existing pins are kept.
	before, _ := os.ReadFile(path)
	code, out, errs = runWizard(t, path,
		"", "n", "y", // URLs kept, not re-trusted, keep the existing pins
		"", "", // bind name and password kept
		"", "",
		"", "n", "y",
		"", "",
		"", "", "",
		"", "",
		"y")
	if !written(code, errs) {
		t.Fatalf("edit: exit %d\n%s\n%s", code, out, errs)
	}
	after, _ := os.ReadFile(path)
	noSecrets(t, out, errs, string(after))
	for _, want := range []string{"Editing " + path, "[" + w.adURL + "]", "[svc-dolly@example.edu]", "Password [Enter keeps the current one]", "[rfc2307]", "mapping:             unchanged"} {
		if !strings.Contains(out, want) {
			t.Errorf("edit output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "smtp_host empty") {
		t.Error("an edit touched notify")
	}
	if string(before) != string(after) {
		t.Errorf("Enter everywhere changed the config:\n%s\n---\n%s", before, after)
	}
}

// A rejected certificate asks for the URLs again; a failed bind asks for
// the name and password again.
func TestConfigWizardRetries(t *testing.T) {
	w := newWizardWorld(t)
	path := filepath.Join(t.TempDir(), "dolly.yaml")
	a := w.newAnswers()
	answers := append([]string{w.adURL, "x", "n"}, a[:2]...)    // asked again, rejected, then trusted
	answers = append(answers, "svc-dolly@example.edu", "wrong") // bind failed
	answers = append(answers, a[2:]...)                         // then the right password
	code, out, errs := runWizard(t, path, answers...)
	if !written(code, errs) {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
	if !strings.Contains(out, "Enter the URLs again.") || !strings.Contains(out, "enter y or n") || !strings.Contains(out, "Server URLs, comma-separated (ldaps://host, or ldap://host for StartTLS) ["+w.adURL+"]") ||
		!strings.Contains(out, "✗ bind failed") || !strings.Contains(out, "2 of 3 tries left") {
		t.Errorf("output:\n%s", out)
	}
	noSecrets(t, out, errs)
}

// Without a terminal dolly config refuses; EOF and "no" write nothing; an
// invalid result is rolled back with the secrets restored.
func TestConfigWizardWritesNothing(t *testing.T) {
	w := newWizardWorld(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "dolly.yaml")

	withFake(t, ldapfake.New(), "", false)
	if code, _, errs := runCLI("config", "--config", path); code != 1 || !strings.Contains(errs, "stdin is not a terminal") {
		t.Errorf("no TTY: exit %d %s", code, errs)
	}
	a := w.newAnswers()
	if code, _, errs := runWizard(t, path, a[:5]...); code != 1 || !strings.Contains(errs, "nothing was written") {
		t.Errorf("EOF: exit %d %s", code, errs)
	}
	if code, out, _ := runWizard(t, path, append(a[:len(a)-1:len(a)-1], "n")...); code != 0 || !strings.Contains(out, "Nothing written.") {
		t.Errorf("no: exit %d", code)
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("a config was written")
	}

	// Rollback: an existing config and ad.secret stay as they were, and a
	// new ldap.secret is removed.
	if err := os.WriteFile(path, []byte("old config\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ad.secret"), []byte("old-ad\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := reloadConfig
	reloadConfig = func(string) (*config.Config, error) { return nil, errors.New("injected") }
	t.Cleanup(func() { reloadConfig = old })
	// "old config" doesn't parse as a config: dolly config says so.
	if code, _, errs := runWizard(t, path, a...); code != 1 || !strings.Contains(errs, "fix it by hand first") {
		t.Errorf("broken config: exit %d %s", code, errs)
	}
	data, _ := os.ReadFile(templateCopy(t))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	edit := []string{w.adURL, "y", "svc-dolly@example.edu", adPw, "OU=People,DC=example,DC=edu", "OU=Groups,DC=example,DC=edu",
		w.tgtURL, "y", "cn=admin,dc=local", tgtPw, "ou=people,dc=local", "ou=group,dc=local", "ou=dolly,dc=local", "", "", "", "y"}
	code, _, errs := runWizard(t, path, edit...)
	if code != 1 || !strings.Contains(errs, "the original files were restored") {
		t.Errorf("rollback: exit %d %s", code, errs)
	}
	if got, _ := os.ReadFile(path); string(got) != string(data) {
		t.Error("config not restored")
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "ad.secret")); string(got) != "old-ad\n" {
		t.Errorf("ad.secret = %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "ldap.secret")); err == nil {
		t.Error("ldap.secret left behind")
	}
}

// A symlinked config stays a link to the edited file, with the password
// files beside the link. Then: with other URLs the existing TLS settings
// aren't offered, every DC's bind is reported, invalid credentials stop
// at the first DC, and a moved password file is named in the summary.
func TestConfigWizardSymlinkAndDCs(t *testing.T) {
	w := newWizardWorld(t)
	realDir, linkDir := t.TempDir(), t.TempDir()
	realPath, link := filepath.Join(realDir, "real.yaml"), filepath.Join(linkDir, "dolly.yaml")
	data, _ := os.ReadFile(templateCopy(t))
	data = []byte(strings.Replace(string(data), "bind_password_file: ad.secret ", "bind_password_file: /old/ad.pw   ", 1))
	if err := os.WriteFile(realPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realPath, link); err != nil {
		t.Skip(err)
	}
	edit := []string{w.adURL, "y", "svc-dolly@example.edu", adPw, "OU=People,DC=example,DC=edu", "OU=Groups,DC=example,DC=edu",
		w.tgtURL, "y", "cn=admin,dc=local", tgtPw, "ou=people,dc=local", "ou=group,dc=local", "ou=dolly,dc=local", "", "", "", "y"}
	code, out, errs := runWizard(t, link, edit...)
	if !written(code, errs) {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
	if !strings.Contains(out, "/old/ad.pw -> ad.secret (the old file is left in place)") {
		t.Errorf("no moved-file line:\n%s", out)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the link is gone: %v", err)
	}
	if got, _ := os.ReadFile(realPath); !strings.Contains(string(got), w.adFP) {
		t.Errorf("the real file wasn't edited:\n%s", got)
	}
	for _, f := range []string{"ad.secret", "ldap.secret"} {
		if _, err := os.Stat(filepath.Join(linkDir, f)); err != nil {
			t.Errorf("%s not beside the link: %v", f, err)
		}
		if _, err := os.Stat(filepath.Join(realDir, f)); err == nil {
			t.Errorf("%s beside the real file", f)
		}
	}

	addr, _ := ldapsServer(t)
	dead := "ldaps://" + addr // fetchable, but checkDial refuses it
	*w.dials = nil
	code, out, errs = runWizard(t, link,
		w.adURL+", "+dead, "n", "", "y", // other URLs: no keep offer
		"", "wrong", "", adPw,
		"", "",
		"", "y", "", "",
		"", "", "",
		"", "", "", // schema, placeholder, uid
		"y")
	if !written(code, errs) {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
	if strings.Contains(out, "Keep the existing") {
		t.Errorf("keep offered for other URLs:\n%s", out)
	}
	if !strings.Contains(out, "✓ "+w.adURL+": bind works") || !strings.Contains(out, "! "+dead+": dial tcp: connection refused") {
		t.Errorf("no per-DC lines:\n%s", out)
	}
	for _, o := range *w.dials {
		if o.URL == dead && o.Password == "wrong" {
			t.Error("the wrong password was tried on the second DC too")
		}
	}
	noSecrets(t, out, errs)
}

// A failed fetch can end dolly config; a template uid must be replaced
// when the schema changes.
func TestConfigWizardAbortAndTemplateUID(t *testing.T) {
	w := newWizardWorld(t)
	path := filepath.Join(t.TempDir(), "dolly.yaml")
	if code, out, errs := runWizard(t, path, "ldaps://127.0.0.1:1", "n"); code != 1 || !strings.Contains(out, "edit dolly.yaml by hand") || !strings.Contains(errs, "nothing was written") {
		t.Errorf("abort: exit %d\n%s\n%s", code, out, errs)
	}
	data, _ := os.ReadFile(templateCopy(t))
	data = []byte(strings.Replace(string(data), "      uid: uid\n", "      uid: '{{ .sAMAccountName }}'\n", 1))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	edit := []string{w.adURL, "y", "svc-dolly@example.edu", adPw, "OU=People,DC=example,DC=edu", "OU=Groups,DC=example,DC=edu",
		w.tgtURL, "y", "cn=admin,dc=local", tgtPw, "ou=people,dc=local", "ou=group,dc=local", "ou=dolly,dc=local", "rfc2307", "", "uid", "y"}
	code, out, errs := runWizard(t, path, edit...)
	if !written(code, errs) {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
	if !strings.Contains(out, "the uid comes from a template now") || !strings.Contains(out, "custom attributes and gid_from_upg are lost") {
		t.Errorf("output:\n%s", out)
	}
}

// A signal during the write is held until the files are complete, then
// ends the run.
func TestWriteConfigSetInterrupted(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no os.Interrupt to self on Windows")
	}
	path := templateCopy(t)
	data, _ := os.ReadFile(path)
	old := reloadConfig
	reloadConfig = func(p string) (*config.Config, error) {
		self, _ := os.FindProcess(os.Getpid())
		if err := self.Signal(os.Interrupt); err != nil {
			t.Fatal(err)
		}
		time.Sleep(50 * time.Millisecond) // let the runtime deliver it
		return old(p)
	}
	t.Cleanup(func() { reloadConfig = old })
	err := writeConfigSet(path, path, data, map[string]string{"ad.secret": "a", "ldap.secret": "b"})
	if err == nil || err.Error() != "interrupted after writing; files are complete" {
		t.Fatalf("err = %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(filepath.Dir(path), "ldap.secret")); string(got) != "b\n" {
		t.Errorf("ldap.secret = %q", got)
	}
}

// The template as dolly install writes it is treated as a new config: no
// example defaults, and notify off.
func TestConfigWizardUneditedTemplate(t *testing.T) {
	w := newWizardWorld(t)
	path := filepath.Join(t.TempDir(), "dolly.yaml")
	if err := os.WriteFile(path, dolly.ConfigTemplate, 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errs := runWizard(t, path, w.newAnswers()...)
	if !written(code, errs) {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
	if !strings.Contains(out, "Found the unedited template at "+path+"; treating it as a new config.") ||
		strings.Contains(out, "example.edu]") || !strings.Contains(out, "smtp_host empty") {
		t.Errorf("output:\n%s", out)
	}
	if cfg, err := config.Load(path); err != nil || cfg.Notify.SMTPHost != "" {
		t.Errorf("config: %v", err)
	}
}
