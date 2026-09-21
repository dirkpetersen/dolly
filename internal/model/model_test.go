package model

import (
	"strings"
	"testing"
	"time"
)

func TestFormatGUID(t *testing.T) {
	for _, tc := range []struct {
		name string
		b    []byte
		want string
	}{
		// schemaIDGUID of the AD "user" class, as stored in objectGUID byte order.
		{"user class", []byte{0xba, 0x7a, 0x96, 0xbf, 0xe6, 0x0d, 0xd0, 0x11, 0xa2, 0x85, 0x00, 0xaa, 0x00, 0x30, 0x49, 0xe2},
			"bf967aba-0de6-11d0-a285-00aa003049e2"},
		// schemaIDGUID of the AD "member" attribute.
		{"member attribute", []byte{0xc0, 0x79, 0x96, 0xbf, 0xe6, 0x0d, 0xd0, 0x11, 0xa2, 0x85, 0x00, 0xaa, 0x00, 0x30, 0x49, 0xe2},
			"bf9679c0-0de6-11d0-a285-00aa003049e2"},
		// Byte order by definition: Data1-3 little-endian, Data4 as is.
		{"layout", []byte{0x33, 0x22, 0x11, 0x00, 0x55, 0x44, 0x77, 0x66, 0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff},
			"00112233-4455-6677-8899-aabbccddeeff"},
	} {
		got, err := FormatGUID(tc.b)
		if err != nil || got != tc.want {
			t.Errorf("%s: FormatGUID = %q, %v; want %q", tc.name, got, err, tc.want)
		}
	}
	if _, err := FormatGUID(make([]byte, 15)); err == nil {
		t.Error("FormatGUID accepted 15 bytes")
	}
}

func TestParseGUID(t *testing.T) {
	for in, want := range map[string]string{
		"BF967ABA-0DE6-11D0-A285-00AA003049E2":   "bf967aba-0de6-11d0-a285-00aa003049e2",
		"{bf967aba-0de6-11d0-a285-00aa003049e2}": "bf967aba-0de6-11d0-a285-00aa003049e2",
	} {
		if got, err := ParseGUID(in); err != nil || got != want {
			t.Errorf("ParseGUID(%q) = %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "bf967aba0de611d0a28500aa003049e2", "zf967aba-0de6-11d0-a285-00aa003049e2"} {
		if _, err := ParseGUID(bad); err == nil {
			t.Errorf("ParseGUID(%q) accepted", bad)
		}
	}
}

func TestDN(t *testing.T) {
	gow := `CN=Gow\, Edward L,OU=Users,OU=Accounts,DC=example,DC=edu`
	if !DNEqual(gow, `cn=gow\2c edward l, ou=users,ou=accounts,dc=EXAMPLE,dc=edu`) {
		t.Error("escaped comma / case / spacing variants must be equal")
	}
	if DNEqual(gow, `CN=Gow,OU=Users,OU=Accounts,DC=example,DC=edu`) {
		t.Error("different DNs compared equal")
	}
	if a, v, err := RDN(gow); err != nil || a != "CN" || v != "Gow, Edward L" {
		t.Errorf("RDN = %q %q %v", a, v, err)
	}
	if got := BuildDN("uid", "a,b+c", "ou=people,dc=local"); got != `uid=a\,b\+c,ou=people,dc=local` || RDNValue(got) != "a,b+c" {
		t.Errorf("BuildDN = %q", got)
	}
	if !DNExact("uid=jdoe,ou=People,dc=local", "uid=jdoe,ou=people,dc=local") {
		t.Error("parent case must not matter")
	}
	if DNExact("uid=jdoe,ou=people,dc=local", "uid=JDoe,ou=people,dc=local") {
		t.Error("RDN case must matter for DNExact")
	}
	if !IsUnder("uid=x,ou=People,dc=local", "ou=people,dc=local") || IsUnder("ou=people,dc=local", "uid=x,ou=people,dc=local") {
		t.Error("IsUnder")
	}
	if ParentKey("uid=x,ou=People,dc=local") != MustDNKey("ou=people,dc=local") {
		t.Error("ParentKey")
	}
	if _, err := DNKey("not a dn"); err == nil {
		t.Error("DNKey accepted garbage")
	}
	if !NameEqual("JDoe", "jdoe") {
		t.Error("NameEqual")
	}
}

func TestToASCII(t *testing.T) {
	for in, want := range map[string]string{
		"José Müller-Øster": "Jose Muller-Oster",
		"Straße":            "Strasse",
		"Łukasz Żółć":       "Lukasz Zolc",
		"O’Brien – “x”":     `O'Brien - "x"`,
		"日本":                "??",
		"tab\there":         "tabhere",
	} {
		if got := ToASCII(in); got != want {
			t.Errorf("ToASCII(%q) = %q, want %q", in, got, want)
		}
	}
	if !IsIA5("GECOS") || IsIA5("cn") {
		t.Error("IsIA5")
	}
}

func TestRecordRoundTrip(t *testing.T) {
	r := &Record{Kind: GroupRecord, GUID: "bf967aba-0de6-11d0-a285-00aa003049e2", SeeAlso: "cn=hpc,ou=group,dc=local",
		Occupants: []string{"uid=a,ou=people,dc=local"}}
	r.SetNote(NoteMissingSince, "2024-01-01")
	r.AddNote(NoteRenamingFrom, "cn=old,ou=group,dc=local")
	r.AddNote(NoteRenamingFrom, "cn=old,ou=group,dc=local")
	e := r.Entry("ou=dolly,dc=local")
	if e.DN != "cn=bf967aba-0de6-11d0-a285-00aa003049e2,ou=groups,ou=dolly,dc=local" {
		t.Errorf("DN = %s", e.DN)
	}
	back, err := RecordFromEntry(e, GroupRecord)
	if err != nil {
		t.Fatal(err)
	}
	if back.SeeAlso != r.SeeAlso || !back.HasOccupant("UID=A,ou=people,dc=local") || len(back.NoteValues(NoteRenamingFrom)) != 1 {
		t.Errorf("round trip: %+v", back)
	}
	since, ok := back.MissingSince()
	if !ok || !since.Equal(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("MissingSince = %v %v", since, ok)
	}
	back.DelNote(NoteMissingSince)
	if _, ok := back.Note(NoteMissingSince); ok {
		t.Error("DelNote")
	}
	e.Set("seeAlso", nil)
	if _, err := RecordFromEntry(e, GroupRecord); err == nil {
		t.Error("record without seeAlso accepted")
	}
}

func TestClassify(t *testing.T) {
	rec := (&Record{Kind: UserRecord, GUID: "bf967aba-0de6-11d0-a285-00aa003049e2", SeeAlso: "uid=a,ou=people,dc=local"}).Entry("ou=dolly,dc=local")
	entries := []*Entry{
		{DN: "ou=people,dc=local"}, {DN: "uid=a,ou=People,dc=local"},
		{DN: "cn=g,ou=group,dc=local"}, {DN: "ou=dolly,dc=local"}, {DN: "ou=users,ou=dolly,dc=local"},
		rec, {DN: "cn=lock,ou=dolly,dc=local"}, {DN: "cn=other,dc=local"},
	}
	b := Bases{Users: "ou=people,dc=local", Groups: "ou=group,dc=local", State: "ou=dolly,dc=local"}
	tgt, recs, err := Classify(entries, b)
	if err != nil {
		t.Fatal(err)
	}
	if len(tgt.Users) != 1 || len(tgt.Groups) != 1 || len(recs.Users) != 1 || len(recs.Groups) != 0 {
		t.Errorf("classify: %d users, %d groups, %d/%d records", len(tgt.Users), len(tgt.Groups), len(recs.Users), len(recs.Groups))
	}
	if !tgt.HasStateBase || !tgt.HasUserRecords || tgt.HasGroupRecords {
		t.Errorf("containers: %+v", tgt)
	}
	bad := &Entry{DN: "cn=not-a-guid,ou=users,ou=dolly,dc=local", Attrs: map[string][]string{"seeAlso": {"uid=a,dc=x"}}}
	if _, _, err := Classify([]*Entry{bad}, b); err == nil || !strings.Contains(err.Error(), "invalid GUID") {
		t.Errorf("malformed record: %v", err)
	}
}

func TestEntryHelpers(t *testing.T) {
	e := &Entry{DN: "cn=x", Attrs: map[string][]string{"memberUid": {"a", "b"}}}
	if len(e.Get("memberuid")) != 2 {
		t.Error("case-insensitive Get")
	}
	e.DeleteValue("MEMBERUID", "a", func(x, y string) bool { return x == y })
	e.AddValue("memberUid", "c")
	if got := strings.Join(e.Get("memberUid"), ","); got != "b,c" {
		t.Errorf("values = %s", got)
	}
	c := e.Clone()
	c.AddValue("memberUid", "d")
	if len(e.Get("memberUid")) != 2 {
		t.Error("Clone shares slices")
	}
	o := &ADObject{Attrs: map[string][]string{"uid": {"x"}, "gidNumber": {" "}}, UserAccountControl: 514}
	if m := o.Missing([]string{"uid", "uidNumber", "gidNumber"}); strings.Join(m, ",") != "uidNumber,gidNumber" || !o.Disabled() {
		t.Errorf("Missing = %v", m)
	}
}
