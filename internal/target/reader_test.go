package target

import (
	"strings"
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

// The existence lookup of a groups-only run matches on uid alone and asks
// only for uid: a target entry without uidNumber or posixAccount counts.
func TestLookupRequestNeedsOnlyUID(t *testing.T) {
	req := lookupRequest("ou=people,dc=local", []string{"jdoe", "bob"})
	if req.Filter != "(|(uid=jdoe)(uid=bob))" {
		t.Errorf("filter = %s, want only (uid=...) terms", req.Filter)
	}
	for _, bad := range []string{"objectclass", "posixaccount", "uidnumber", "gidnumber", "&"} {
		if strings.Contains(strings.ToLower(req.Filter), bad) {
			t.Errorf("filter %s must not contain %q", req.Filter, bad)
		}
	}
	if len(req.Attributes) != 1 || req.Attributes[0] != "uid" {
		t.Errorf("attributes = %v, want only uid", req.Attributes)
	}
	if req.BaseDN != "ou=people,dc=local" || req.Scope != ldap.ScopeWholeSubtree {
		t.Errorf("base %q scope %d", req.BaseDN, req.Scope)
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
