package ldapconn

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"
)

// expiredCert is a self-signed certificate for 127.0.0.1 that expired a
// year ago, like the osixia demo certificate.
func expiredCert(t *testing.T) (tls.Certificate, *x509.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(7), Subject: pkix.Name{CommonName: "ldap.example.org"},
		NotBefore: time.Now().AddDate(-2, 0, 0), NotAfter: time.Now().AddDate(-1, 0, 0),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames: []string{"ldap.example.org"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, leaf
}

// tlsServer is an LDAPS (or, with startTLS, ldap:// + StartTLS) server on
// 127.0.0.1 that accepts any bind.
func tlsServer(t *testing.T, cert tls.Certificate, startTLS bool) string {
	return stallServer(t, func(c net.Conn) {
		if startTLS {
			id, _, err := readRequest(c)
			if err != nil {
				return
			}
			if _, err := c.Write(extendedOK(id)); err != nil {
				return
			}
		}
		tc := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{cert}})
		if tc.Handshake() != nil {
			return
		}
		answer(tc)
	})
}

func TestParseFingerprint(t *testing.T) {
	hex := strings.Repeat("ab", 32)
	want := "sha256:" + strings.TrimSuffix(strings.Repeat("AB:", 32), ":")
	for _, in := range []string{hex, strings.ToUpper(hex), want, "SHA256:" + strings.ToLower(want[7:]), " " + strings.ReplaceAll(want[7:], ":", "") + " "} {
		if got, err := ParseFingerprint(in); err != nil || got != want {
			t.Errorf("%q: %q, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "sha256:", "ab", hex + "ab", strings.Repeat("zz", 32), "sha1:" + hex} {
		if _, err := ParseFingerprint(in); err == nil {
			t.Errorf("%q accepted", in)
		}
	}
}

// The pin check is the only thing that authenticates a pinned server: a
// right pin is accepted although the certificate is expired, self-signed,
// and for another name; a wrong pin is rejected, over LDAPS and StartTLS.
func TestPinnedTLS(t *testing.T) {
	cert, leaf := expiredCert(t)
	right := Fingerprint(leaf)
	wrong := "sha256:" + strings.TrimSuffix(strings.Repeat("00:", 32), ":")
	for _, startTLS := range []bool{false, true} {
		addr := tlsServer(t, cert, startTLS)
		url := "ldaps://" + addr
		if startTLS {
			url = "ldap://" + addr
		}
		o := Options{URL: url, StartTLS: startTLS, Timeout: 2 * time.Second, BindDN: "cn=x", Password: "p"}

		o.Pins = []string{wrong, strings.ToLower(strings.ReplaceAll(right, ":", ""))[6:]} // second entry: plain hex
		c, err := Dial(context.Background(), o)
		if err != nil {
			t.Fatalf("%s: right pin among others rejected: %v", url, err)
		}
		if st, _ := c.TLSConnectionState(); !st.HandshakeComplete {
			t.Errorf("%s: not TLS", url)
		}
		c.Close()

		o.Pins = []string{wrong}
		if _, err := Dial(context.Background(), o); err == nil || !strings.Contains(err.Error(), "matches no tls_fingerprint") ||
			!strings.Contains(err.Error(), right) || !strings.Contains(err.Error(), "dolly fetch-cert "+url) {
			t.Errorf("%s: wrong pin: %v", url, err)
		}

		// Without a pin the expired certificate fails verification, and the
		// hint names its fingerprint.
		o.Pins = nil
		if _, err := Dial(context.Background(), o); err == nil || !strings.Contains(err.Error(), "leaf certificate is "+right) {
			t.Errorf("%s: unverified: %v", url, err)
		}
	}
}

func TestTLSConfigPins(t *testing.T) {
	if _, err := TLSConfig("h", "ca.pem", []string{strings.Repeat("ab", 32)}); err == nil {
		t.Error("ca_file with pins must be an error")
	}
	if _, err := TLSConfig("h", "", []string{"nope"}); err == nil {
		t.Error("a malformed pin must be an error")
	}
	c, err := TLSConfig("h", "", []string{strings.Repeat("ab", 32)})
	if err != nil || !c.InsecureSkipVerify || c.VerifyConnection == nil {
		t.Fatalf("pinned config %+v, %v", c, err)
	}
	if err := c.VerifyConnection(tls.ConnectionState{}); err == nil {
		t.Error("no certificate must be rejected")
	}
}

func TestFetchChain(t *testing.T) {
	cert, leaf := expiredCert(t)
	for _, startTLS := range []bool{false, true} {
		addr := tlsServer(t, cert, startTLS)
		url := "ldaps://" + addr
		if startTLS {
			url = "ldap://" + addr
		}
		chain, err := FetchChain(context.Background(), url, 2*time.Second)
		if err != nil || len(chain) != 1 || Fingerprint(chain[0]) != Fingerprint(leaf) {
			t.Errorf("%s: chain %v, %v", url, chain, err)
		}
	}
	if _, err := FetchChain(context.Background(), "https://127.0.0.1:1", time.Second); err == nil {
		t.Error("https:// must be refused")
	}
}
