package target

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/go-ldap/ldap/v3"

	"github.com/dirkpetersen/dolly/internal/config"
	"github.com/dirkpetersen/dolly/internal/ldapconn"
	"github.com/dirkpetersen/dolly/internal/model"
)

// Conn is the part of an LDAP connection the writer uses: the applier, the
// run lock, and cn=status. *ldap.Conn implements it; tests use a fake.
// network_timeout applies to every call (ldapconn sets it on the
// connection), so a single operation can never hang a run.
type Conn interface {
	Add(*ldap.AddRequest) error
	Modify(*ldap.ModifyRequest) error
	ModifyDN(*ldap.ModifyDNRequest) error
	Del(*ldap.DelRequest) error
	Search(*ldap.SearchRequest) (*ldap.SearchResult, error)
}

// DialWriter opens the connection a real run writes through: the run lock,
// the plan, and cn=status. Unlike the reader's connection it isn't closed
// when the run context ends, so the lock can still be released after a
// SIGTERM or run_timeout. The connection setup (dial, TLS, bind) and every
// later operation are bounded by network_timeout (see ldapconn.Dial), and
// the applier stops between operations once the run context ends.
func DialWriter(cfg *config.Config) (*ldap.Conn, error) {
	pw, err := cfg.Target.Password()
	if err != nil {
		return nil, fmt.Errorf("target: %w", err)
	}
	conn, err := ldapconn.Dial(context.Background(), ldapconn.Options{
		URL: cfg.Target.URL, StartTLS: cfg.Target.StartTLS, CAFile: cfg.Target.CAFile,
		Timeout: cfg.Sync.NetworkTimeout.Duration, BindDN: cfg.Target.BindDN, Password: pw,
	})
	if err != nil {
		return nil, fmt.Errorf("target %s: %w", cfg.Target.URL, err)
	}
	return conn, nil
}

// EnsureStateBase creates state_base (and only it) if it is missing, so the
// run lock can be created under it. A concurrent creation by another host
// (entryAlreadyExists) is fine. The record containers below it are created
// by the plan.
func EnsureStateBase(ctx context.Context, c Conn, stateBase string) (created bool, err error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	_, err = c.Search(ldap.NewSearchRequest(stateBase, ldap.ScopeBaseObject, ldap.NeverDerefAliases, 1, 0, false,
		"(objectClass=*)", []string{"1.1"}, nil))
	switch {
	case err == nil:
		return false, nil
	case !ldapconn.IsNoSuchObject(err):
		return false, fmt.Errorf("reading state_base %s: %w", stateBase, err)
	}
	attr, val, err := model.RDN(stateBase)
	if err != nil {
		return false, err
	}
	var class string
	switch strings.ToLower(attr) {
	case "ou":
		class = "organizationalUnit"
	case "cn":
		class = "organizationalRole"
	default:
		return false, fmt.Errorf("state_base %s is missing, and Dolly creates it only with an ou= or cn= RDN; create it by hand", stateBase)
	}
	req := ldap.NewAddRequest(stateBase, nil)
	req.Attribute("objectClass", []string{class})
	req.Attribute(attr, []string{val})
	switch err := c.Add(req); {
	case ldap.IsErrorWithCode(err, ldap.LDAPResultEntryAlreadyExists):
		return false, nil // another host created it first
	case err != nil:
		return false, fmt.Errorf("creating state_base %s: %w", stateBase, err)
	}
	return true, nil
}

// Searcher is the read-only part of Conn (dolly check reads the run lock
// through it).
type Searcher interface {
	Search(*ldap.SearchRequest) (*ldap.SearchResult, error)
}

// readEntry reads one entry by DN (base scope). It returns nil, nil if the
// entry doesn't exist.
func readEntry(c Searcher, dn string, attrs ...string) (*ldap.Entry, error) {
	res, err := c.Search(ldap.NewSearchRequest(dn, ldap.ScopeBaseObject, ldap.NeverDerefAliases, 1, 0, false,
		"(objectClass=*)", attrs, nil))
	switch {
	case ldapconn.IsNoSuchObject(err):
		return nil, nil
	case err != nil:
		return nil, err
	case len(res.Entries) == 0:
		return nil, nil
	}
	return res.Entries[0], nil
}

// parseGeneralizedTime parses an LDAP GeneralizedTime such as
// 20260921190000Z (createTimestamp).
func parseGeneralizedTime(s string) (time.Time, error) {
	for _, layout := range []string{"20060102150405Z0700", "20060102150405.999999999Z0700", "200601021504Z0700"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid GeneralizedTime %q", s)
}
