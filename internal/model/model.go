// Package model holds the data types shared by the source, the planner, and
// the target: AD snapshots, target entries, and ownership records, plus
// helpers for objectGUIDs, DNs, and ASCII transliteration.
package model

import (
	"sort"
	"strings"
)

// Entry is an LDAP entry: a DN and its attributes. Attribute names are
// matched case-insensitively, values are kept as they are.
type Entry struct {
	DN    string              `yaml:"dn" json:"dn"`
	Attrs map[string][]string `yaml:"attrs" json:"attrs"`
}

// key returns the name under which attr is stored, or "" if absent.
func (e *Entry) key(attr string) string {
	if _, ok := e.Attrs[attr]; ok {
		return attr
	}
	for k := range e.Attrs {
		if strings.EqualFold(k, attr) {
			return k
		}
	}
	return ""
}

// Get returns the values of attr (case-insensitive name).
func (e *Entry) Get(attr string) []string {
	if k := e.key(attr); k != "" {
		return e.Attrs[k]
	}
	return nil
}

// First returns the first value of attr, or "".
func (e *Entry) First(attr string) string {
	if v := e.Get(attr); len(v) > 0 {
		return v[0]
	}
	return ""
}

// Set replaces the values of attr. No values removes the attribute.
func (e *Entry) Set(attr string, values []string) {
	if e.Attrs == nil {
		e.Attrs = map[string][]string{}
	}
	if k := e.key(attr); k != "" {
		delete(e.Attrs, k)
	}
	if len(values) > 0 {
		e.Attrs[attr] = append([]string(nil), values...)
	}
}

// AddValue appends a single value to attr.
func (e *Entry) AddValue(attr, value string) {
	e.Set(attr, append(e.Get(attr), value))
}

// DeleteValue removes every value of attr equal to value under eq.
func (e *Entry) DeleteValue(attr, value string, eq func(a, b string) bool) {
	var keep []string
	for _, v := range e.Get(attr) {
		if !eq(v, value) {
			keep = append(keep, v)
		}
	}
	e.Set(attr, keep)
}

// Clone returns a deep copy of e.
func (e *Entry) Clone() *Entry {
	c := &Entry{DN: e.DN, Attrs: make(map[string][]string, len(e.Attrs))}
	for k, v := range e.Attrs {
		c.Attrs[k] = append([]string(nil), v...)
	}
	return c
}

// HasObjectClass reports whether the entry lists class (case-insensitive).
func (e *Entry) HasObjectClass(class string) bool {
	for _, v := range e.Get("objectClass") {
		if strings.EqualFold(v, class) {
			return true
		}
	}
	return false
}

// SortedKeys returns the keys of m in sorted order.
func SortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
