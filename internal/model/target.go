package model

import "fmt"

// TargetSnapshot is a complete read of the target: every entry under
// users_base and groups_base, and whether Dolly's containers exist. Config
// validation requires the two bases to differ; if one is nested in the
// other, an entry can appear in both lists as the same pointer.
type TargetSnapshot struct {
	Users  []*Entry
	Groups []*Entry
	// HasStateBase and friends report which of Dolly's containers exist.
	HasStateBase, HasUserRecords, HasGroupRecords bool
}

// Records are the ownership records read from state_base.
type Records struct {
	Users  []*Record
	Groups []*Record
}

// Bases names the target containers used to classify entries.
type Bases struct {
	Users, Groups, State string
}

// Classify sorts raw target entries into users, groups, and ownership
// records. The containers themselves are not returned as entries. Entries
// outside all bases, cn=lock, and cn=status are ignored. A malformed
// ownership record is an error: Dolly never plans from data it can't read.
func Classify(entries []*Entry, b Bases) (*TargetSnapshot, *Records, error) {
	t := &TargetSnapshot{}
	r := &Records{}
	usersKey, groupsKey, stateKey := MustDNKey(b.Users), MustDNKey(b.Groups), MustDNKey(b.State)
	userRecs := MustDNKey(RecordsBase(b.State, UserRecord))
	groupRecs := MustDNKey(RecordsBase(b.State, GroupRecord))
	for _, e := range entries {
		k, err := DNKey(e.DN)
		if err != nil {
			return nil, nil, fmt.Errorf("target entry: %w", err)
		}
		switch k {
		case stateKey:
			t.HasStateBase = true
			continue
		case userRecs:
			t.HasUserRecords = true
			continue
		case groupRecs:
			t.HasGroupRecords = true
			continue
		case usersKey, groupsKey:
			continue
		}
		parent := ParentKey(e.DN)
		switch {
		case parent == userRecs:
			rec, err := RecordFromEntry(e, UserRecord)
			if err != nil {
				return nil, nil, err
			}
			r.Users = append(r.Users, rec)
			continue
		case parent == groupRecs:
			rec, err := RecordFromEntry(e, GroupRecord)
			if err != nil {
				return nil, nil, err
			}
			r.Groups = append(r.Groups, rec)
			continue
		case IsUnder(e.DN, b.State):
			continue // cn=lock, cn=status, anything else Dolly doesn't plan with
		}
		if IsUnder(e.DN, b.Users) {
			t.Users = append(t.Users, e)
		}
		if IsUnder(e.DN, b.Groups) {
			t.Groups = append(t.Groups, e)
		}
	}
	return t, r, nil
}
