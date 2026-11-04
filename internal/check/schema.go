package check

import (
	"errors"
	"regexp"
	"slices"
	"strings"

	"github.com/go-ldap/ldap/v3"
)

// PosixGroupKind returns the kind of the posixGroup object class,
// "STRUCTURAL" (RFC 2307, nis.schema) or "AUXILIARY" (rfc2307bis), from a
// subschema entry's objectClasses values; "" if posixGroup isn't defined.
// A definition without a kind keyword is STRUCTURAL (RFC 4512).
func PosixGroupKind(defs []string) string {
	for _, d := range defs {
		m := nameClause.FindStringSubmatch(d)
		if m == nil || !slices.ContainsFunc(quoted.FindAllString(m[1], -1), func(n string) bool { return strings.EqualFold(n, "'posixGroup'") }) {
			continue
		}
		kind := "STRUCTURAL"
		for _, tok := range strings.Fields(quoted.ReplaceAllString(d, "''")) { // DESC may contain any word
			switch up := strings.ToUpper(strings.Trim(tok, "()")); up {
			case "STRUCTURAL", "AUXILIARY", "ABSTRACT":
				kind = up
			}
		}
		return kind
	}
	return ""
}

var (
	nameClause = regexp.MustCompile(`(?i)\bNAME\s+(\([^)]*\)|'[^']*')`)
	quoted     = regexp.MustCompile(`'[^']*'`)
)

// ReadPosixGroupKind reads the server's subschema (the root DSE's
// subschemaSubentry, then its objectClasses) and returns PosixGroupKind.
func ReadPosixGroupKind(conn Conn) (string, error) {
	res, err := conn.Search(ldap.NewSearchRequest("", ldap.ScopeBaseObject, ldap.NeverDerefAliases, 0, 0, false, "(objectClass=*)", []string{"subschemaSubentry"}, nil))
	if err != nil {
		return "", err
	}
	dn := ""
	if len(res.Entries) > 0 {
		dn = res.Entries[0].GetAttributeValue("subschemaSubentry")
	}
	if dn == "" {
		return "", errors.New("the root DSE names no subschemaSubentry")
	}
	res, err = conn.Search(ldap.NewSearchRequest(dn, ldap.ScopeBaseObject, ldap.NeverDerefAliases, 0, 0, false, "(objectClass=*)", []string{"objectClasses"}, nil))
	if err != nil {
		return "", err
	}
	if len(res.Entries) == 0 {
		return "", errors.New("subschema entry " + dn + " not readable")
	}
	return PosixGroupKind(res.Entries[0].GetEqualFoldAttributeValues("objectClasses")), nil
}
