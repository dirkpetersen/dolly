// Package ldapconn opens authenticated LDAP connections for the AD source
// and the target: TLS setup (ca_file or system roots, never
// InsecureSkipVerify), timeouts, and the bind. It also runs searches that
// fail on any incomplete result.
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
	Timeout  time.Duration
	BindDN   string // a DN, or for AD also a UPN
	Password string
}

// Dial connects, sets up TLS, and binds. network_timeout (Timeout) applies
// to the dial, the TLS handshake, and every later operation. When ctx ends,
// the connection is closed, which aborts any operation in flight.
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
	tlsCfg, err := TLSConfig(u.Hostname(), o.CAFile)
	if err != nil {
		return nil, err
	}
	conn, err := ldap.DialURL(o.URL, ldap.DialWithDialer(&net.Dialer{Timeout: o.Timeout}), ldap.DialWithTLSConfig(tlsCfg))
	if err != nil {
		return nil, explainTLS(err, o.CAFile)
	}
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
	if u.Scheme == "ldap" && o.StartTLS {
		if err := conn.StartTLS(tlsCfg); err != nil {
			return fail(fmt.Errorf("StartTLS: %w", explainTLS(err, o.CAFile)))
		}
	}
	if err := conn.Bind(o.BindDN, o.Password); err != nil {
		return fail(fmt.Errorf("bind as %s: %w", o.BindDN, err))
	}
	return conn, nil
}

// TLSConfig returns a TLS config that verifies host against the system
// roots plus the certificates in caFile (if set).
func TLSConfig(host, caFile string) (*tls.Config, error) {
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

// explainTLS adds a hint to certificate verification errors.
func explainTLS(err error, caFile string) error {
	var ua x509.UnknownAuthorityError
	var hn x509.HostnameError
	switch {
	case errors.As(err, &ua), strings.Contains(err.Error(), "certificate signed by unknown authority"):
		if caFile == "" {
			return fmt.Errorf("%w (the server's CA isn't trusted: fetch its chain and set ca_file, see README \"TLS certificates\")", err)
		}
		return fmt.Errorf("%w (the server's CA isn't in ca_file %s, see README \"TLS certificates\")", err, caFile)
	case errors.As(err, &hn), strings.Contains(err.Error(), "certificate is valid for"):
		return fmt.Errorf("%w (connect by a name the certificate lists)", err)
	}
	return err
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
