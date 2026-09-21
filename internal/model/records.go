package model

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Record note keys stored as "key=value" description values.
const (
	// NoteMissingSince is when a user was first seen gone from AD (RFC 3339, UTC).
	NoteMissingSince = "missing-since"
	// NoteSavedShell is the loginShell to restore when a disabled account is
	// re-enabled. An empty value means the entry had no loginShell.
	NoteSavedShell = "saved-shell"
	// NoteRenamingFrom is the DN an entry is being renamed from. It is written
	// before the modrdn and removed after all references are updated, so an
	// interrupted rename can be finished by the next run.
	NoteRenamingFrom = "renaming-from"
)

// Containers under state_base.
const (
	UserRecordsRDN  = "ou=users"
	GroupRecordsRDN = "ou=groups"
	LockRDN         = "cn=lock"
	StatusRDN       = "cn=status"
)

// RecordKind says whether a record belongs to a user or a group.
type RecordKind string

// Record kinds.
const (
	UserRecord  RecordKind = "user"
	GroupRecord RecordKind = "group"
)

// Record is an ownership record: an organizationalRole entry named
// cn=<objectGUID> under state_base. SeeAlso is the managed entry,
// Occupants (roleOccupant, groups only) are the member DNs Dolly added, and
// Notes are key=value description values.
type Record struct {
	Kind      RecordKind
	GUID      string
	SeeAlso   string
	Occupants []string
	Notes     []string
}

// RecordsBase returns the container for records of kind k.
func RecordsBase(stateBase string, k RecordKind) string {
	if k == GroupRecord {
		return GroupRecordsRDN + "," + stateBase
	}
	return UserRecordsRDN + "," + stateBase
}

// DN returns the record's DN under stateBase.
func (r *Record) DN(stateBase string) string {
	return "cn=" + r.GUID + "," + RecordsBase(stateBase, r.Kind)
}

// Note returns the first value of note key and whether it exists.
func (r *Record) Note(key string) (string, bool) {
	for _, n := range r.Notes {
		if k, v, _ := strings.Cut(n, "="); k == key {
			return v, true
		}
	}
	return "", false
}

// NoteValues returns every value of note key.
func (r *Record) NoteValues(key string) []string {
	var out []string
	for _, n := range r.Notes {
		if k, v, _ := strings.Cut(n, "="); k == key {
			out = append(out, v)
		}
	}
	return out
}

// SetNote replaces all values of key with value.
func (r *Record) SetNote(key, value string) {
	r.DelNote(key)
	r.Notes = append(r.Notes, key+"="+value)
	sort.Strings(r.Notes)
}

// AddNote adds a value to a multi-valued note unless it is already there.
func (r *Record) AddNote(key, value string) {
	for _, v := range r.NoteValues(key) {
		if v == value {
			return
		}
	}
	r.Notes = append(r.Notes, key+"="+value)
	sort.Strings(r.Notes)
}

// DelNote removes all values of key.
func (r *Record) DelNote(key string) {
	var keep []string
	for _, n := range r.Notes {
		if k, _, _ := strings.Cut(n, "="); k != key {
			keep = append(keep, n)
		}
	}
	r.Notes = keep
}

// MissingSince returns the parsed missing-since note. A bare date
// (2006-01-02) is accepted as midnight UTC.
func (r *Record) MissingSince() (time.Time, bool) {
	v, ok := r.Note(NoteMissingSince)
	if !ok {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, true
	}
	if t, err := time.Parse("2006-01-02", v); err == nil {
		return t, true
	}
	return time.Time{}, false
}

// HasOccupant reports whether dn is a roleOccupant (case-insensitive DN match).
func (r *Record) HasOccupant(dn string) bool {
	for _, o := range r.Occupants {
		if DNEqual(o, dn) {
			return true
		}
	}
	return false
}

// Clone returns a deep copy of r.
func (r *Record) Clone() *Record {
	c := *r
	c.Occupants = append([]string(nil), r.Occupants...)
	c.Notes = append([]string(nil), r.Notes...)
	return &c
}

// Entry renders the record as an LDAP entry (core schema only).
func (r *Record) Entry(stateBase string) *Entry {
	e := &Entry{DN: r.DN(stateBase), Attrs: map[string][]string{
		"objectClass": {"organizationalRole"},
		"cn":          {r.GUID},
	}}
	if r.SeeAlso != "" {
		e.Attrs["seeAlso"] = []string{r.SeeAlso}
	}
	if len(r.Occupants) > 0 {
		e.Attrs["roleOccupant"] = append([]string(nil), r.Occupants...)
	}
	if len(r.Notes) > 0 {
		e.Attrs["description"] = append([]string(nil), r.Notes...)
	}
	return e
}

// RecordFromEntry parses an ownership record entry of kind k.
func RecordFromEntry(e *Entry, k RecordKind) (*Record, error) {
	_, cn, err := RDN(e.DN)
	if err != nil {
		return nil, err
	}
	guid, err := ParseGUID(cn)
	if err != nil {
		return nil, fmt.Errorf("ownership record %s: %w", e.DN, err)
	}
	see := e.Get("seeAlso")
	if len(see) != 1 {
		return nil, fmt.Errorf("ownership record %s: want exactly one seeAlso, got %d", e.DN, len(see))
	}
	if _, err := DNKey(see[0]); err != nil {
		return nil, fmt.Errorf("ownership record %s: %w", e.DN, err)
	}
	r := &Record{Kind: k, GUID: guid, SeeAlso: see[0], Notes: append([]string(nil), e.Get("description")...)}
	sort.Strings(r.Notes)
	for _, o := range e.Get("roleOccupant") {
		if _, err := DNKey(o); err != nil {
			return nil, fmt.Errorf("ownership record %s: roleOccupant: %w", e.DN, err)
		}
		r.Occupants = append(r.Occupants, o)
	}
	return r, nil
}
