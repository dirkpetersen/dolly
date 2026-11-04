package ldapconn

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	ber "github.com/go-asn1-ber/asn1-ber"
	"github.com/go-ldap/ldap/v3"
)

// startTLSOID is the StartTLS extended operation (RFC 4511).
const startTLSOID = "1.3.6.1.4.1.1466.20037"

// secure upgrades raw: StartTLS then a TLS handshake for ldap:// with
// startTLS, the implicit TLS handshake for ldaps://, nothing for plain
// ldap://. StartTLS is sent by hand rather than through go-ldap, whose
// StartTLS flattens the handshake error and so loses the server's
// certificates (needed for the fingerprint hint).
func secure(ctx context.Context, raw net.Conn, addr, scheme string, startTLS bool, cfg *tls.Config) (net.Conn, error) {
	if scheme == "ldap" {
		if !startTLS {
			return raw, nil
		}
		if err := sendStartTLS(raw); err != nil {
			return nil, fmt.Errorf("StartTLS with %s: %w", addr, err)
		}
	}
	tc := tls.Client(raw, cfg)
	if err := tc.HandshakeContext(ctx); err != nil {
		return nil, fmt.Errorf("TLS handshake with %s: %w", addr, err)
	}
	return tc, nil
}

// sendStartTLS sends the StartTLS request and reads its answer.
func sendStartTLS(c net.Conn) error {
	p := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSequence, nil, "LDAP Request")
	p.AppendChild(ber.NewInteger(ber.ClassUniversal, ber.TypePrimitive, ber.TagInteger, int64(1), "MessageID"))
	r := ber.Encode(ber.ClassApplication, ber.TypeConstructed, ldap.ApplicationExtendedRequest, nil, "Start TLS")
	r.AppendChild(ber.NewString(ber.ClassContext, ber.TypePrimitive, 0, startTLSOID, "TLS Extended Command"))
	p.AppendChild(r)
	if _, err := c.Write(p.Bytes()); err != nil {
		return err
	}
	resp, err := ber.ReadPacket(c)
	if err != nil {
		return err
	}
	if len(resp.Children) < 2 || resp.Children[1].Tag != ldap.ApplicationExtendedResponse {
		return errors.New("unexpected answer to the StartTLS request")
	}
	return ldap.GetLDAPError(resp)
}

// Fingerprint returns the SHA-256 fingerprint of a certificate's DER bytes
// as sha256:AA:BB:... (the form tls_fingerprint accepts and Dolly prints).
func Fingerprint(c *x509.Certificate) string {
	sum := sha256.Sum256(c.Raw)
	return formatFingerprint(sum[:])
}

func formatFingerprint(sum []byte) string {
	h := strings.ToUpper(hex.EncodeToString(sum))
	var b strings.Builder
	b.WriteString("sha256:")
	for i := 0; i < len(h); i += 2 {
		if i > 0 {
			b.WriteByte(':')
		}
		b.WriteString(h[i : i+2])
	}
	return b.String()
}

// ParseFingerprint normalizes a SHA-256 fingerprint given as sha256:AA:BB:...
// or plain hex, with or without colons, in any case, to Fingerprint's form.
func ParseFingerprint(s string) (string, error) {
	h := strings.TrimSpace(s)
	if len(h) >= 7 && strings.EqualFold(h[:7], "sha256:") {
		h = h[7:]
	}
	h = strings.ReplaceAll(h, ":", "")
	b, err := hex.DecodeString(h)
	if err != nil || len(b) != sha256.Size {
		return "", fmt.Errorf("%q is not a SHA-256 fingerprint (64 hex digits, optionally sha256: and colons)", s)
	}
	return formatFingerprint(b), nil
}

var errPinMismatch = errors.New("matches no tls_fingerprint")

// pinnedConfig accepts a server only if its leaf certificate's SHA-256
// matches one of pins.
//
// spec: the pin authenticates the server, so the CA chain, expiry, and
// hostname are not checked. This is the only place Dolly sets
// InsecureSkipVerify, and only together with the pin check in
// VerifyConnection (which Go runs on every handshake, resumed ones
// included).
func pinnedConfig(host string, pins []string) (*tls.Config, error) {
	want := map[string]bool{}
	for _, p := range pins {
		fp, err := ParseFingerprint(p)
		if err != nil {
			return nil, fmt.Errorf("tls_fingerprint: %w", err)
		}
		want[fp] = true
	}
	return &tls.Config{
		ServerName:         host,
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("the server sent no certificate")
			}
			if fp := Fingerprint(cs.PeerCertificates[0]); !want[fp] {
				return fmt.Errorf("server certificate %s %w", fp, errPinMismatch)
			}
			return nil
		},
	}, nil
}

// FetchChain connects to an ldaps:// or ldap:// URL (StartTLS for ldap://),
// runs the TLS handshake, and returns the certificate chain the server
// sent, leaf first. It sends no credentials and no bind. The chain is
// returned whether or not it verifies against the system roots (it is taken
// from the verification error), so this needs no InsecureSkipVerify.
func FetchChain(ctx context.Context, rawURL string, timeout time.Duration) ([]*x509.Certificate, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	if u.Hostname() == "" {
		return nil, fmt.Errorf("%s: host is missing", rawURL)
	}
	addr, err := hostPort(u)
	if err != nil {
		return nil, err
	}
	cfg, err := TLSConfig(u.Hostname(), "", nil)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	raw, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	defer raw.Close()
	deadline, _ := ctx.Deadline()
	if err := raw.SetDeadline(deadline); err != nil {
		return nil, err
	}
	c, err := secure(ctx, raw, addr, strings.ToLower(u.Scheme), true, cfg)
	var cve *tls.CertificateVerificationError
	switch {
	case errors.As(err, &cve):
		return cve.UnverifiedCertificates, nil
	case err != nil:
		return nil, timeoutHint(err, timeout)
	}
	return c.(*tls.Conn).ConnectionState().PeerCertificates, nil
}
