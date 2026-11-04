// Package ldapconn opens authenticated LDAP connections for the AD source
// and the target: TLS setup (ca_file or system roots, or a pinned leaf
// certificate; InsecureSkipVerify only ever together with the pin check),
// timeouts, and the bind. It also runs searches that fail on any incomplete
// result, and fetches a server's certificate chain (dolly fetch-cert).
package ldapconn

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/go-ldap/ldap/v3"
)

// Options describe one connection.
type Options struct {
	URL      string // ldap:// or ldaps://
	StartTLS bool   // upgrade an ldap:// connection with StartTLS
	CAFile   string // PEM bundle added to the system roots; "" uses the system roots only
	// Pins are SHA-256 fingerprints of the server's leaf certificate
	// (tls_fingerprint). When set, the pin replaces CA, expiry, and
	// hostname checks.
	Pins     []string
	Timeout  time.Duration
	BindDN   string // a DN, or for AD also a UPN
	Password string
}

// Dial connects, sets up TLS, and binds. The whole setup (TCP dial,
// StartTLS or the implicit TLS handshake, and the bind) is bounded by one
// network_timeout (Timeout) deadline on the socket, so a server that accepts
// the connection and then stalls can't hang Dolly. Once the setup succeeds
// the deadline is cleared, and every later operation is bounded by
// network_timeout through go-ldap's per-request timeout. When ctx ends, the
// connection is closed, which aborts any operation in flight.
func Dial(ctx context.Context, o Options) (*ldap.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	u, err := url.Parse(o.URL)
	if err != nil {
		return nil, err
	}
	if o.Password == "" {
		// An empty password would be an unauthenticated bind, which many
		// servers accept and then answer every search with nothing.
		return nil, errors.New("the bind password is empty")
	}
	if o.Timeout <= 0 {
		return nil, errors.New("the network timeout must be positive")
	}
	tlsCfg, err := TLSConfig(u.Hostname(), o.CAFile, o.Pins)
	if err != nil {
		return nil, err
	}
	addr, err := hostPort(u)
	if err != nil {
		return nil, err
	}

	setupCtx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	raw, err := (&net.Dialer{Timeout: o.Timeout}).DialContext(setupCtx, "tcp", addr)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ldap.NewError(ldap.ErrorNetwork, err)
	}
	// One deadline for the rest of the setup: every read and write on the
	// socket (TLS handshake, StartTLS, bind) fails once it passes, which
	// makes go-ldap close the connection and fail the pending request.
	deadline, _ := setupCtx.Deadline()
	if err := raw.SetDeadline(deadline); err != nil {
		raw.Close()
		return nil, ldap.NewError(ldap.ErrorNetwork, err)
	}
	netConn, err := secure(setupCtx, raw, addr, strings.ToLower(u.Scheme), o.StartTLS, tlsCfg)
	if err != nil {
		raw.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, explainTLS(ldap.NewError(ldap.ErrorNetwork, timeoutHint(err, o.Timeout)), o.URL, o.CAFile)
	}
	_, isTLS := netConn.(*tls.Conn)
	conn := ldap.NewConn(netConn, isTLS)
	conn.Start()
	conn.SetTimeout(o.Timeout)
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	fail := func(err error) (*ldap.Conn, error) {
		stop()
		conn.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	if err := conn.Bind(o.BindDN, o.Password); err != nil {
		return fail(fmt.Errorf("bind as %s: %w", o.BindDN, timeoutHint(err, o.Timeout)))
	}
	// Setup done: clear the deadline (a tls.Conn wraps raw, so clearing it
	// on raw clears it for the TLS layer too).
	if err := raw.SetDeadline(time.Time{}); err != nil {
		return fail(ldap.NewError(ldap.ErrorNetwork, err))
	}
	return conn, nil
}

// hostPort returns host:port of an ldap:// or ldaps:// URL, with the
// scheme's default port if none is given.
func hostPort(u *url.URL) (string, error) {
	var port string
	switch strings.ToLower(u.Scheme) {
	case "ldap":
		port = ldap.DefaultLdapPort
	case "ldaps":
		port = ldap.DefaultLdapsPort
	default:
		return "", fmt.Errorf("%s: scheme must be ldap:// or ldaps://", u)
	}
	if p := u.Port(); p != "" {
		port = p
	}
	return net.JoinHostPort(u.Hostname(), port), nil
}

// timeoutHint names network_timeout in an error caused by the setup
// deadline, whose own text (a closed channel, an i/o timeout) doesn't say
// why the connection went away.
func timeoutHint(err error, timeout time.Duration) error {
	var ne net.Error
	msg := err.Error()
	if (errors.As(err, &ne) && ne.Timeout()) || errors.Is(err, context.DeadlineExceeded) ||
		strings.Contains(msg, "i/o timeout") || strings.Contains(msg, "response channel closed") ||
		strings.Contains(msg, "connection closed") || strings.Contains(msg, "timed out") {
		return fmt.Errorf("%w (no answer within network_timeout %s)", err, timeout)
	}
	return err
}

// TLSConfig returns a TLS config that verifies host against the system
// roots plus the certificates in caFile (if set). With pins, it instead
// accepts exactly the servers whose leaf certificate has one of the pinned
// SHA-256 fingerprints, and checks nothing else.
func TLSConfig(host, caFile string, pins []string) (*tls.Config, error) {
	if len(pins) > 0 {
		if caFile != "" {
			return nil, errors.New("ca_file and tls_fingerprint can't both be set")
		}
		return pinnedConfig(host, pins)
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("ca_file: %w", err)
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("ca_file %s: no PEM certificates found", caFile)
		}
	}
	return &tls.Config{ServerName: host, RootCAs: pool, MinVersion: tls.VersionTLS12}, nil
}

// explainTLS adds a hint to certificate verification errors: what to do,
// and the server's leaf fingerprint for tls_fingerprint.
func explainTLS(err error, rawURL, caFile string) error {
	var ua x509.UnknownAuthorityError
	var hn x509.HostnameError
	var hint string
	switch {
	case errors.As(err, &ua), strings.Contains(err.Error(), "certificate signed by unknown authority"):
		if caFile == "" {
			hint = "the server's CA isn't trusted: set ca_file or tls_fingerprint"
		} else {
			hint = "the server's CA isn't in ca_file " + caFile + ": fix ca_file, or set tls_fingerprint instead"
		}
	case errors.As(err, &hn), strings.Contains(err.Error(), "certificate is valid for"):
		hint = "connect by a name the certificate lists, or set tls_fingerprint"
	case errors.Is(err, errPinMismatch):
		hint = "the server's certificate changed, or this is not the server you pinned"
	}
	var cve *tls.CertificateVerificationError
	if errors.As(err, &cve) && len(cve.UnverifiedCertificates) > 0 {
		if hint == "" {
			hint = "set ca_file, or set tls_fingerprint"
		}
		hint += "; the server's leaf certificate is " + Fingerprint(cve.UnverifiedCertificates[0])
	}
	if hint == "" {
		return err
	}
	return fmt.Errorf("%w (%s; run `dolly fetch-cert %s` to inspect the chain, see README \"TLS certificates\")", err, hint, rawURL)
}

// IsNoSuchObject reports whether err is LDAP noSuchObject (32).
func IsNoSuchObject(err error) bool { return ldap.IsErrorWithCode(err, ldap.LDAPResultNoSuchObject) }

// SearchPaged runs a paged search and returns every entry. Any error, a
// sizeLimitExceeded included, fails the whole search: a truncated result is
// never returned as if it were complete. Search continuation references
// (referrals to other naming contexts) are not followed.
func SearchPaged(ctx context.Context, conn *ldap.Conn, req *ldap.SearchRequest, pageSize int) ([]*ldap.Entry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	res, err := conn.SearchWithPaging(req, uint32(pageSize))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if ldap.IsErrorWithCode(err, ldap.LDAPResultSizeLimitExceeded) {
			return nil, fmt.Errorf("%w (result truncated after %d entries; Dolly never plans from partial data)", err, len(res.Entries))
		}
		return nil, err
	}
	return res.Entries, nil
}

// Search runs an unpaged search (base-scope lookups and small bounded
// searches) with the same all-or-nothing rule as SearchPaged.
func Search(ctx context.Context, conn *ldap.Conn, req *ldap.SearchRequest) ([]*ldap.Entry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	res, err := conn.Search(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if ldap.IsErrorWithCode(err, ldap.LDAPResultSizeLimitExceeded) {
			return nil, fmt.Errorf("%w (result truncated; Dolly never plans from partial data)", err)
		}
		return nil, err
	}
	return res.Entries, nil
}

// Scheme returns the lowercased URL scheme of u, or "".
func Scheme(u string) string {
	p, err := url.Parse(u)
	if err != nil {
		return ""
	}
	return strings.ToLower(p.Scheme)
}
