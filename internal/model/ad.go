package model

import "strings"

// Kind is the type of an AD object.
type Kind string

// AD object kinds. Anything that is neither a user nor a group (computers,
// contacts, foreign security principals) is KindOther and is skipped.
const (
	KindUser  Kind = "user"
	KindGroup Kind = "group"
	KindOther Kind = "other"
)

// UACDisabled is the ACCOUNTDISABLE bit of userAccountControl.
const UACDisabled = 0x2

// ADObject is one user, group, or other object read from AD.
type ADObject struct {
	GUID               string              `yaml:"guid" json:"guid"` // canonical, see FormatGUID
	DN                 string              `yaml:"dn" json:"dn"`
	Kind               Kind                `yaml:"kind" json:"kind"`
	Attrs              map[string][]string `yaml:"attrs" json:"attrs"`     // raw AD attributes by name
	Members            []string            `yaml:"members" json:"members"` // member DNs (groups), fully ranged
	UserAccountControl int                 `yaml:"userAccountControl" json:"userAccountControl"`
	// InScope is true for objects found by the configured user or group
	// search, false for objects fetched by DN because a group referenced them.
	InScope bool `yaml:"-" json:"-"`
}

// Get returns the values of an AD attribute (case-insensitive name).
func (o *ADObject) Get(attr string) []string {
	if v, ok := o.Attrs[attr]; ok {
		return v
	}
	for k, v := range o.Attrs {
		if strings.EqualFold(k, attr) {
			return v
		}
	}
	return nil
}

// First returns the first value of attr, or "".
func (o *ADObject) First(attr string) string {
	if v := o.Get(attr); len(v) > 0 {
		return v[0]
	}
	return ""
}

// Disabled reports whether the account is disabled in AD.
func (o *ADObject) Disabled() bool { return o.UserAccountControl&UACDisabled != 0 }

// Missing returns the attributes of required that o lacks (absent or empty).
func (o *ADObject) Missing(required []string) []string {
	var miss []string
	for _, r := range required {
		ok := false
		for _, v := range o.Get(r) {
			if strings.TrimSpace(v) != "" {
				ok = true
				break
			}
		}
		if !ok {
			miss = append(miss, r)
		}
	}
	return miss
}

// ADSnapshot is a complete read of AD: every in-scope user and group, plus
// every out-of-scope object reachable as a group member.
type ADSnapshot struct {
	Objects []*ADObject
	// Unresolved lists member DNs that AD could not find.
	Unresolved []string
}

// Count returns the number of in-scope objects of kind k.
func (s *ADSnapshot) Count(k Kind) int {
	n := 0
	for _, o := range s.Objects {
		if o.InScope && o.Kind == k {
			n++
		}
	}
	return n
}
