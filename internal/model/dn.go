package model

import (
	"fmt"
	"strings"
	"sync"

	"github.com/go-ldap/ldap/v3"
)

// DNKey returns a normalized, case-folded form of dn: attribute types and
// values are lowercased, whitespace around separators is dropped, and
// escaping is canonical. Two DNs are equal (case-insensitively) exactly when
// their keys are equal. Escaped commas such as `CN=Gow\, Edward L` are
// handled by go-ldap's RFC 4514 parser.
//
// Results are memoized: planning compares the same DNs many times.
func DNKey(dn string) (string, error) {
	keyMu.Lock()
	k, ok := keyCache[dn]
	keyMu.Unlock()
	if ok {
		return k, nil
	}
	k, err := dnKey(dn)
	if err == nil {
		keyMu.Lock()
		if len(keyCache) >= maxKeyCache {
			keyCache = map[string]string{}
		}
		keyCache[dn] = k
		keyMu.Unlock()
	}
	return k, err
}

var (
	keyMu    sync.Mutex
	keyCache = map[string]string{}
)

const maxKeyCache = 1 << 20

func dnKey(dn string) (string, error) {
	d, err := ldap.ParseDN(dn)
	if err != nil {
		return "", fmt.Errorf("invalid DN %q: %w", dn, err)
	}
	for _, r := range d.RDNs {
		for _, a := range r.Attributes {
			a.Value = strings.ToLower(a.Value)
		}
	}
	return d.String(), nil
}

// MustDNKey is DNKey for DNs already known to be valid. An invalid DN yields
// a key that cannot collide with a valid one.
func MustDNKey(dn string) string {
	k, err := DNKey(dn)
	if err != nil {
		return "\x00invalid:" + dn
	}
	return k
}

// DNEqual reports whether two DNs are equal, ignoring case.
func DNEqual(a, b string) bool {
	ka, err1 := DNKey(a)
	kb, err2 := DNKey(b)
	return err1 == nil && err2 == nil && ka == kb
}

// NameEqual compares names (uid, cn) the way LDAP does, ignoring case.
func NameEqual(a, b string) bool { return strings.EqualFold(a, b) }

// RDN returns the attribute type and the unescaped value of the first RDN
// component of dn.
func RDN(dn string) (attr, value string, err error) {
	d, err := ldap.ParseDN(dn)
	if err != nil {
		return "", "", fmt.Errorf("invalid DN %q: %w", dn, err)
	}
	if len(d.RDNs) == 0 || len(d.RDNs[0].Attributes) == 0 {
		return "", "", fmt.Errorf("empty DN")
	}
	a := d.RDNs[0].Attributes[0]
	return a.Type, a.Value, nil
}

// RDNValue returns the unescaped value of the first RDN, or "".
func RDNValue(dn string) string {
	_, v, _ := RDN(dn)
	return v
}

// ParentKey returns the DNKey of dn's parent.
func ParentKey(dn string) string {
	d, err := ldap.ParseDN(dn)
	if err != nil || len(d.RDNs) == 0 {
		return MustDNKey(dn)
	}
	p := &ldap.DN{RDNs: d.RDNs[1:]}
	return MustDNKey(p.String())
}

// DNExact reports whether a and b name the same entry with the same RDN
// spelling: the RDN value must match exactly (so a case-only rename is a
// change), the rest of the DN case-insensitively.
func DNExact(a, b string) bool {
	return DNEqual(a, b) && RDNValue(a) == RDNValue(b)
}

// BuildDN returns attr=value,base with value escaped per RFC 4514.
func BuildDN(attr, value, base string) string {
	return RDNString(attr, value) + "," + base
}

// RDNString returns attr=value with value escaped per RFC 4514.
func RDNString(attr, value string) string {
	return attr + "=" + ldap.EscapeDN(value)
}

// IsUnder reports whether dn is base or a descendant of base.
func IsUnder(dn, base string) bool {
	d, err1 := ldap.ParseDN(dn)
	b, err2 := ldap.ParseDN(base)
	if err1 != nil || err2 != nil {
		return false
	}
	return d.EqualFold(b) || b.AncestorOfFold(d)
}
