package target

import (
	"testing"

	"github.com/go-ldap/ldap/v3"
)

func TestUIDFilter(t *testing.T) {
	f := uidFilter([]string{"jdoe", "a*b", "x)(uid=*"})
	if f != `(|(uid=jdoe)(uid=a\2ab)(uid=x\29\28uid=\2a))` {
		t.Errorf("filter = %s", f)
	}
	if _, err := ldap.CompileFilter(f); err != nil {
		t.Errorf("filter doesn't compile: %v", err)
	}
}

func TestToEntriesMergesAttributes(t *testing.T) {
	in := []*ldap.Entry{ldap.NewEntry("cn=b,ou=group,dc=local", map[string][]string{"memberUid": {"x", "y"}}),
		ldap.NewEntry("cn=a,ou=group,dc=local", map[string][]string{"cn": {"a"}})}
	out := toEntries(in)
	if len(out) != 2 || out[0].DN != "cn=a,ou=group,dc=local" || len(out[1].Get("memberuid")) != 2 {
		t.Errorf("entries = %+v", out)
	}
}
