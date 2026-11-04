package check

import (
	"testing"
	"time"

	"github.com/dirkpetersen/dolly/internal/ldapfake"
)

// Definitions as OpenLDAP serves them from nis.schema and rfc2307bis.schema.
const (
	nisPosixGroup = "( 1.3.6.1.1.1.2.2 NAME 'posixGroup' DESC 'Abstraction of a group of accounts' SUP top STRUCTURAL MUST ( cn $ gidNumber ) MAY ( userPassword $ memberUid $ description ) )"
	bisPosixGroup = "( 1.3.6.1.1.1.2.2 NAME 'posixGroup' DESC 'Abstraction of a group of accounts' SUP top AUXILIARY MUST gidNumber MAY ( userPassword $ memberUid $ description ) )"
	posixAccount  = "( 1.3.6.1.1.1.2.0 NAME 'posixAccount' DESC 'Abstraction of an account with POSIX attributes' SUP top AUXILIARY MUST ( cn $ uid $ uidNumber $ gidNumber $ homeDirectory ) MAY ( userPassword $ loginShell $ gecos $ description ) )"
)

func TestPosixGroupKind(t *testing.T) {
	for _, tc := range []struct {
		name string
		defs []string
		want string
	}{
		{"nis.schema", []string{posixAccount, nisPosixGroup}, "STRUCTURAL"},
		{"rfc2307bis", []string{bisPosixGroup, posixAccount}, "AUXILIARY"},
		{"no kind is structural", []string{"( 1.3.6.1.1.1.2.2 NAME 'posixGroup' SUP top MUST gidNumber )"}, "STRUCTURAL"},
		{"name list, case", []string{"( 1.2 NAME ( 'pg' 'POSIXGROUP' ) AUXILIARY )"}, "AUXILIARY"},
		{"kind word in DESC", []string{"( 1.2 NAME 'posixGroup' DESC 'not AUXILIARY' STRUCTURAL )"}, "STRUCTURAL"},
		{"only in MUST of another", []string{"( 1.3 NAME 'x' AUXILIARY MUST posixGroup )"}, ""},
		{"missing", []string{posixAccount}, ""},
	} {
		if got := PosixGroupKind(tc.defs); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestReadPosixGroupKind(t *testing.T) {
	d := ldapfake.New()
	if _, err := ReadPosixGroupKind(d); err == nil {
		t.Error("no root DSE: no error")
	}
	d.Put("", map[string][]string{"subschemaSubentry": {"cn=Subschema"}}, time.Now())
	d.Put("cn=Subschema", map[string][]string{"objectClass": {"subschema"}, "objectClasses": {posixAccount, nisPosixGroup}}, time.Now())
	if got, err := ReadPosixGroupKind(d); err != nil || got != "STRUCTURAL" {
		t.Errorf("%q, %v", got, err)
	}
}
