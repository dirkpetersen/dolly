package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dirkpetersen/dolly/internal/ldapconn"
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
