package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dirkpetersen/dolly/internal/config"
	"github.com/dirkpetersen/dolly/internal/ldapconn"
	"github.com/dirkpetersen/dolly/internal/ldapfake"
)

// ldapsServer is an LDAPS listener on 127.0.0.1 with an expired self-signed
// certificate that completes the handshake and then closes.
func ldapsServer(t *testing.T) (string, *x509.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "ldap.example.org"},
		NotBefore: time.Now().AddDate(-2, 0, 0), NotAfter: time.Now().AddDate(-1, 0, 0)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.(*tls.Conn).Handshake()
			c.Close()
		}
	}()
	return ln.Addr().(*net.TCPAddr).String(), leaf
}

func TestFetchCert(t *testing.T) {
	addr, leaf := ldapsServer(t)
	fp := ldapconn.Fingerprint(leaf)
	out := filepath.Join(t.TempDir(), "ca.pem")
	code, stdout, stderr := runCLI("fetch-cert", "ldaps://"+addr, "--out", out)
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, stdout, stderr)
	}
	for _, want := range []string{"[0] subject: CN=ldap.example.org", "EXPIRED", "sha256:  " + fp, `tls_fingerprint: ["` + fp + `"]`, "Wrote 1 certificates to " + out} {
		if !strings.Contains(stdout, want) {
			t.Errorf("output lacks %q:\n%s", want, stdout)
		}
	}
	data, err := os.ReadFile(out)
	if b, _ := pem.Decode(data); err != nil || b == nil || string(b.Bytes) != string(leaf.Raw) {
		t.Errorf("PEM file: %v", err)
	}
	if st, _ := os.Stat(out); st.Mode().Perm()&^0o644 != 0 { // 0644 less the umask
		t.Errorf("mode %v", st.Mode())
	}
	// Never overwritten; the URL may also follow the flags.
	if code, _, stderr := runCLI("fetch-cert", "--out", out, "ldaps://"+addr); code != 1 || !strings.Contains(stderr, "never overwritten") {
		t.Errorf("existing file: exit %d %s", code, stderr)
	}
	if code, _, _ := runCLI("fetch-cert"); code != 1 {
		t.Error("no URL must fail")
	}
}

// pinConfig is a copy of the template (DCs dc01 and dc02, target
// ldap://ldap.example.edu:389 with start_tls, ca_file on both sides) with
// the given file mode.
func pinConfig(t *testing.T, mode os.FileMode) string {
	t.Helper()
	path := templateCopy(t)
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

// fetch-cert --source/--target writes the given URLs and their leaf pins
// into the config: comments and other keys are kept, ca_file is removed,
// target.start_tls follows the scheme, the file mode is kept, it needs a
// TTY or --yes, and a result that doesn't validate is rolled back.
func TestFetchCertWritesPins(t *testing.T) {
	addr, leaf := ldapsServer(t)
	fp := ldapconn.Fingerprint(leaf)
	url := "ldaps://" + addr
	t.Run("writes URLs and pins", func(t *testing.T) {
		path := pinConfig(t, 0o600)
		code, out, errs := runCLI("fetch-cert", "--source", url, "--target", url, "--config", path, "--yes")
		if code != 0 {
			t.Fatalf("exit %d\n%s\n%s", code, out, errs)
		}
		for _, want := range []string{
			"source.urls: [ldaps://dc01.example.edu:636, ldaps://dc02.example.edu:636] -> [" + url + "]",
			"source.tls_fingerprint: [" + fp + "]",
			"source.ca_file: removed",
			"target.url: ldap://ldap.example.edu:389 -> " + url,
			"target.start_tls: true -> false",
			"target.ca_file: removed",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("summary lacks %q:\n%s", want, out)
			}
		}
		data, _ := os.ReadFile(path)
		s := string(data)
		for _, want := range []string{
			"tls_fingerprint: [\"" + fp + "\"]",
			"# tried in random order each run", // comments are kept
			"bind_password_file: ad.secret",    // other values too
			"  url: " + url + "\n",
			"start_tls: false",
		} {
			if !strings.Contains(s, want) {
				t.Errorf("config lacks %q:\n%s", want, s)
			}
		}
		if strings.Count(s, "tls_fingerprint: [\""+fp) != 2 || strings.Contains(s, "\n  ca_file:") || strings.Contains(s, "dc02.example.edu:636\n") {
			t.Errorf("want pins on both sides, no ca_file, and only the given DC:\n%s", s)
		}
		if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
			t.Errorf("mode %v", st.Mode())
		}
		cfg, err := config.Load(path)
		if err != nil || len(cfg.Source.URLs) != 1 || cfg.Source.URLs[0] != url || len(cfg.Source.TLSFingerprint) != 1 ||
			cfg.Target.URL != url || cfg.Target.StartTLS || cfg.Target.CAFile != "" || cfg.Source.CAFile != "" {
			t.Errorf("reload: %v %+v", err, cfg)
		}
	})
	t.Run("ldap:// sets start_tls", func(t *testing.T) {
		data := []byte("target:\n  url: ldaps://old.example.edu\n")
		edited, err := setPins(data, []pinSide{{section: "target", edits: []pinEdit{
			{"url", scalar("!!str", "ldap://new.example.edu"), true}, {"start_tls", scalar("!!bool", "true"), true}}}})
		if err != nil || string(edited) != "target:\n  url: ldap://new.example.edu\n  start_tls: true\n" {
			t.Errorf("%v\n%s", err, edited)
		}
	})
	t.Run("a stricter mode is kept", func(t *testing.T) {
		path := pinConfig(t, 0o400)
		if code, out, errs := runCLI("fetch-cert", "--target", url, "--config", path, "--yes"); code != 0 {
			t.Fatalf("exit %d\n%s\n%s", code, out, errs)
		}
		if st, _ := os.Stat(path); st.Mode().Perm() != 0o400 {
			t.Errorf("mode %v", st.Mode())
		}
	})
	t.Run("no TTY and no --yes: refused", func(t *testing.T) {
		path := pinConfig(t, 0o600)
		before, _ := os.ReadFile(path)
		withFake(t, ldapfake.New(), "", false)
		code, _, errs := runCLI("fetch-cert", "--target", url, "--config", path)
		after, _ := os.ReadFile(path)
		if code != 1 || !strings.Contains(errs, "refusing to write the config without confirmation") || string(before) != string(after) {
			t.Errorf("exit %d %s", code, errs)
		}
	})
	t.Run("answer y on a TTY", func(t *testing.T) {
		path := pinConfig(t, 0o600)
		withFake(t, ldapfake.New(), "y\n", true)
		code, out, _ := runCLI("fetch-cert", "--target", url, "--config", path)
		if code != 0 || !strings.Contains(out, "Write these changes to "+path+"? [y/N]") || !strings.Contains(out, "Wrote "+path) {
			t.Errorf("exit %d\n%s", code, out)
		}
	})
	t.Run("a failed fetch writes nothing", func(t *testing.T) {
		path := pinConfig(t, 0o600)
		before, _ := os.ReadFile(path)
		ln, _ := net.Listen("tcp", "127.0.0.1:0")
		dead := ln.Addr().String()
		ln.Close()
		code, _, errs := runCLI("fetch-cert", "--source", url, "--source", "ldaps://"+dead, "--config", path, "--yes")
		after, _ := os.ReadFile(path)
		if code != 1 || !strings.Contains(errs, "nothing was written") || string(before) != string(after) {
			t.Errorf("exit %d %s", code, errs)
		}
	})
	t.Run("an invalid result is rolled back", func(t *testing.T) {
		path := pinConfig(t, 0o600)
		before, _ := os.ReadFile(path)
		old := reloadConfig
		reloadConfig = func(string) (*config.Config, error) { return nil, errors.New("injected") }
		t.Cleanup(func() { reloadConfig = old })
		code, _, errs := runCLI("fetch-cert", "--target", url, "--config", path, "--yes")
		after, _ := os.ReadFile(path)
		if code != 1 || !strings.Contains(errs, "the original was restored") || string(before) != string(after) {
			t.Errorf("exit %d %s", code, errs)
		}
		if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
			t.Errorf("mode %v", st.Mode())
		}
	})
	t.Run("usage errors", func(t *testing.T) {
		for _, args := range [][]string{
			{"fetch-cert", url, "--target", url},
			{"fetch-cert", "--target", url, "--out", "x.pem"},
			{"fetch-cert", url, "--yes"},
		} {
			if code, _, errs := runCLI(args...); code != 1 || !strings.Contains(errs, "Usage:") {
				t.Errorf("%v: exit %d %s", args, code, errs)
			}
		}
	})
}
