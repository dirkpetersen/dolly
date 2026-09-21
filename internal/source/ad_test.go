package source

import (
	"testing"

	"github.com/dirkpetersen/dolly/internal/model"
)

func TestParseRange(t *testing.T) {
	for _, tc := range []struct {
		desc       string
		ranged     bool
		start, end int
		last       bool
		err        bool
	}{
		{"member", false, 0, -1, false, false},
		{"member;range=0-1499", true, 0, 1499, false, false},
		{"member;range=1500-2999", true, 1500, 2999, false, false},
		{"member;range=3000-*", true, 3000, -1, true, false},
		{"Member;Range=0-*", true, 0, -1, true, false},
		{"member;binary;range=10-19", true, 10, 19, false, false},
		{"member;range=abc-*", false, 0, 0, false, true},
		{"member;range=5", false, 0, 0, false, true},
		{"member;range=10-3", false, 0, 0, false, true},
		{"member;range=-1-5", false, 0, 0, false, true},
	} {
		r, err := parseRange(tc.desc)
		if tc.err {
			if err == nil {
				t.Errorf("%s: want an error", tc.desc)
			}
			continue
		}
		if err != nil || r.ranged != tc.ranged || r.start != tc.start || r.end != tc.end || r.last != tc.last || r.attr == "" {
			t.Errorf("%s: got %+v, %v", tc.desc, r, err)
		}
	}
}

func TestKindOf(t *testing.T) {
	for want, classes := range map[model.Kind][]string{
		model.KindUser:  {"top", "person", "organizationalPerson", "user"},
		model.KindGroup: {"top", "group"},
		model.KindOther: {"top", "person", "organizationalPerson", "user", "computer"},
	} {
		if got := kindOf(classes); got != want {
			t.Errorf("%v: %s, want %s", classes, got, want)
		}
	}
	if kindOf([]string{"top", "contact"}) != model.KindOther || kindOf([]string{"foreignSecurityPrincipal"}) != model.KindOther {
		t.Error("contacts and foreign security principals are other")
	}
}
