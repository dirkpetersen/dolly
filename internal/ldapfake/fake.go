// Package ldapfake is an in-memory LDAP directory for tests of the target
// writer: Add, Modify, ModifyDN, Del, and base-scope Search, with the
// result codes a real server returns (entryAlreadyExists, noSuchObject,
// attributeOrValueExists, noSuchAttribute, notAllowedOnNonLeaf) and a
// createTimestamp on every entry. Hooks inject failures and races.
package ldapfake

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-ldap/ldap/v3"

	"github.com/dirkpetersen/dolly/internal/model"
)

// Dir is the fake directory. It is safe for concurrent use.
type Dir struct {
	mu      sync.Mutex
	entries map[string]*entry // DNKey -> entry

	// Now is the server clock for createTimestamp (default time.Now).
	Now func() time.Time
	// Fail, if set, is called before every operation with its name ("add",
	// "modify", "modifydn", "del", "search"), DN, and request. A non-nil
	// error is returned instead of performing the operation.
	Fail func(op, dn string, req any) error
	// Before, if set, is called before every operation (after Fail), with
	// the lock released, so a test can change the directory in between.
	Before func(op, dn string)
	// Calls records "<op> <dn>" for every call, in order.
	Calls []string
}

type entry struct {
	dn      string
	attrs   map[string][]string // attribute name as first written -> values
	created time.Time
}

// New returns an empty directory.
func New() *Dir { return &Dir{entries: map[string]*entry{}, Now: time.Now} }

// Error returns an LDAP error with code, like a server's result.
func Error(code uint16, msg string) error { return ldap.NewError(code, errors.New(msg)) }

// Put stores an entry directly (test setup), with the given createTimestamp.
func (d *Dir) Put(dn string, attrs map[string][]string, created time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.entries[model.MustDNKey(dn)] = &entry{dn: dn, attrs: copyAttrs(attrs), created: created}
}

// Get returns a copy of the entry's attributes, or nil.
func (d *Dir) Get(dn string) map[string][]string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if e := d.entries[model.MustDNKey(dn)]; e != nil {
		return copyAttrs(e.attrs)
	}
	return nil
}

// Values returns the values of attr (case-insensitive name) of the entry.
func (d *Dir) Values(dn, attr string) []string {
	for k, v := range d.Get(dn) {
		if strings.EqualFold(k, attr) {
			return v
		}
	}
	return nil
}

// Has reports whether an entry exists at dn.
func (d *Dir) Has(dn string) bool { return d.Get(dn) != nil }

// DNs lists every entry's DN, sorted by DNKey.
func (d *Dir) DNs() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var keys []string
	for k := range d.entries {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = d.entries[k].dn
	}
	return out
}

func (d *Dir) begin(op, dn string, req any) error {
	d.mu.Lock()
	d.Calls = append(d.Calls, op+" "+dn)
	fail, before := d.Fail, d.Before
	d.mu.Unlock()
	if fail != nil {
		if err := fail(op, dn, req); err != nil {
			return err
		}
	}
	if before != nil {
		before(op, dn)
	}
	return nil
}

// Add implements target.Conn.
func (d *Dir) Add(req *ldap.AddRequest) error {
	if err := d.begin("add", req.DN, req); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	k, err := model.DNKey(req.DN)
	if err != nil {
		return Error(ldap.LDAPResultInvalidDNSyntax, err.Error())
	}
	if d.entries[k] != nil {
		return Error(ldap.LDAPResultEntryAlreadyExists, "Already exists")
	}
	attrs := map[string][]string{}
	for _, a := range req.Attributes {
		if len(a.Vals) == 0 {
			return Error(ldap.LDAPResultProtocolError, "attribute "+a.Type+" has no values")
		}
		attrs[a.Type] = append([]string(nil), a.Vals...)
	}
	d.entries[k] = &entry{dn: req.DN, attrs: attrs, created: d.Now()}
	return nil
}

// Modify implements target.Conn. The changes apply atomically: any error
// leaves the entry unchanged.
func (d *Dir) Modify(req *ldap.ModifyRequest) error {
	if err := d.begin("modify", req.DN, req); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	e := d.entries[model.MustDNKey(req.DN)]
	if e == nil {
		return Error(ldap.LDAPResultNoSuchObject, "No such object")
	}
	attrs := copyAttrs(e.attrs)
	for _, ch := range req.Changes {
		name := ch.Modification.Type
		vals := ch.Modification.Vals
		key := attrKey(attrs, name)
		switch ch.Operation {
		case ldap.AddAttribute:
			if key == "" {
				key = name
			}
			for _, v := range vals {
				if contains(name, attrs[key], v) {
					return Error(ldap.LDAPResultAttributeOrValueExists, fmt.Sprintf("%s: value #0 already exists", name))
				}
				attrs[key] = append(attrs[key], v)
			}
		case ldap.DeleteAttribute:
			if key == "" {
				return Error(ldap.LDAPResultNoSuchAttribute, name+": no such attribute")
			}
			if len(vals) == 0 {
				delete(attrs, key)
				continue
			}
			for _, v := range vals {
				if !contains(name, attrs[key], v) {
					return Error(ldap.LDAPResultNoSuchAttribute, name+": no such value")
				}
				attrs[key] = remove(name, attrs[key], v)
			}
			if len(attrs[key]) == 0 {
				delete(attrs, key)
			}
		case ldap.ReplaceAttribute:
			if key != "" {
				delete(attrs, key)
			}
			if len(vals) > 0 {
				attrs[name] = append([]string(nil), vals...)
			}
		default:
			return Error(ldap.LDAPResultProtocolError, "unknown modify operation")
		}
	}
	e.attrs = attrs
	return nil
}

// ModifyDN implements target.Conn (same parent only).
func (d *Dir) ModifyDN(req *ldap.ModifyDNRequest) error {
	if err := d.begin("modifydn", req.DN, req); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if req.NewSuperior != "" {
		return Error(ldap.LDAPResultUnwillingToPerform, "newSuperior not supported by the fake")
	}
	oldKey := model.MustDNKey(req.DN)
	e := d.entries[oldKey]
	if e == nil {
		return Error(ldap.LDAPResultNoSuchObject, "No such object")
	}
	parsed, err := ldap.ParseDN(req.DN)
	if err != nil {
		return Error(ldap.LDAPResultInvalidDNSyntax, err.Error())
	}
	parent := (&ldap.DN{RDNs: parsed.RDNs[1:]}).String()
	newDN := req.NewRDN + "," + parent
	newKey := model.MustDNKey(newDN)
	if newKey != oldKey && d.entries[newKey] != nil {
		return Error(ldap.LDAPResultEntryAlreadyExists, "Already exists")
	}
	oldAttr, oldVal, _ := model.RDN(req.DN)
	newAttr, newVal, err := model.RDN(newDN)
	if err != nil {
		return Error(ldap.LDAPResultInvalidDNSyntax, err.Error())
	}
	if req.DeleteOldRDN {
		if k := attrKey(e.attrs, oldAttr); k != "" {
			e.attrs[k] = remove(oldAttr, e.attrs[k], oldVal)
			if len(e.attrs[k]) == 0 {
				delete(e.attrs, k)
			}
		}
	}
	k := attrKey(e.attrs, newAttr)
	if k == "" {
		k = newAttr
	}
	if !contains(newAttr, e.attrs[k], newVal) {
		e.attrs[k] = append(e.attrs[k], newVal)
	}
	delete(d.entries, oldKey)
	e.dn = newDN
	d.entries[newKey] = e
	return nil
}

// Del implements target.Conn.
func (d *Dir) Del(req *ldap.DelRequest) error {
	if err := d.begin("del", req.DN, req); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	k := model.MustDNKey(req.DN)
	if d.entries[k] == nil {
		return Error(ldap.LDAPResultNoSuchObject, "No such object")
	}
	for other, e := range d.entries {
		if other != k && model.IsUnder(e.dn, req.DN) {
			return Error(ldap.LDAPResultNotAllowedOnNonLeaf, "subordinate objects must be deleted first")
		}
	}
	delete(d.entries, k)
	return nil
}

// Search implements target.Conn for base-scope searches. It returns the
// requested attributes ("*" or none: all user attributes; "1.1": none) and
// createTimestamp when requested by name.
func (d *Dir) Search(req *ldap.SearchRequest) (*ldap.SearchResult, error) {
	if err := d.begin("search", req.BaseDN, req); err != nil {
		return nil, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if req.Scope != ldap.ScopeBaseObject {
		return nil, Error(ldap.LDAPResultUnwillingToPerform, "the fake supports base-scope searches only")
	}
	e := d.entries[model.MustDNKey(req.BaseDN)]
	if e == nil {
		return nil, Error(ldap.LDAPResultNoSuchObject, "No such object")
	}
	out := map[string][]string{}
	all := len(req.Attributes) == 0
	for _, a := range req.Attributes {
		if a == "*" {
			all = true
		}
	}
	for _, a := range req.Attributes {
		switch {
		case a == "1.1" || a == "*":
		case strings.EqualFold(a, "createTimestamp"):
			out["createTimestamp"] = []string{e.created.UTC().Format("20060102150405Z")}
		default:
			if k := attrKey(e.attrs, a); k != "" {
				out[k] = append([]string(nil), e.attrs[k]...)
			}
		}
	}
	if all {
		for k, v := range e.attrs {
			out[k] = append([]string(nil), v...)
		}
	}
	return &ldap.SearchResult{Entries: []*ldap.Entry{ldap.NewEntry(e.dn, out)}}, nil
}

// Close is a no-op.
func (d *Dir) Close() error { return nil }

func attrKey(attrs map[string][]string, name string) string {
	for k := range attrs {
		if strings.EqualFold(k, name) {
			return k
		}
	}
	return ""
}

var dnAttrs = map[string]bool{"member": true, "roleoccupant": true, "seealso": true}

func equal(attr, a, b string) bool {
	switch {
	case dnAttrs[strings.ToLower(attr)]:
		return model.DNEqual(a, b)
	case strings.EqualFold(attr, "memberUid"):
		return a == b // caseExactIA5Match
	default:
		return strings.EqualFold(a, b)
	}
}

func contains(attr string, vals []string, v string) bool {
	for _, x := range vals {
		if equal(attr, x, v) {
			return true
		}
	}
	return false
}

func remove(attr string, vals []string, v string) []string {
	var out []string
	for _, x := range vals {
		if !equal(attr, x, v) {
			out = append(out, x)
		}
	}
	return out
}

func copyAttrs(in map[string][]string) map[string][]string {
	out := make(map[string][]string, len(in))
	for k, v := range in {
		out[k] = append([]string(nil), v...)
	}
	return out
}
