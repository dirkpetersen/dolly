package planner

import (
	"strings"
	"testing"

	"github.com/dirkpetersen/dolly/internal/config"
	"github.com/dirkpetersen/dolly/internal/model"
)

// Rule (gid_from_upg): a user's gidNumber comes from the AD group named by
// group_name (u-<uid>), matched case-insensitively, when the user has no
// valid gidNumber (when: missing) or whenever the group exists (when:
// always). The derived value passes the ID range check; a user left without
// one gets no entry and is listed once.
func TestRuleGIDFromUPG(t *testing.T) {
	upg := func(name string, n int, gid string) *model.ADObject {
		g := mkGroup(n, name)
		g.Attrs["gidNumber"] = []string{gid}
		return g
	}
	noGID := func() *model.ADObject {
		u := mkUser(1, "jdoe")
		delete(u.Attrs, "gidNumber")
		return u
	}
	withGID := func(gid string) *model.ADObject {
		u := mkUser(1, "jdoe")
		u.Attrs["gidNumber"] = []string{gid}
		return u
	}
	dup := upg("u-jdoe", 21, "7002")
	dup.DN = "CN=u-jdoe,OU=Other,DC=example,DC=edu" // sorts after OU=Groups by DN
	for _, tc := range []struct {
		name    string
		when    string
		ad      []*model.ADObject
		wantGID string // "" means no user entry
		warn    string // a warning that must be present
		kind    WarningKind
		counted int
	}{
		{"from the user only", config.GIDWhenMissing, []*model.ADObject{withGID("100")}, "100", "", "", 0},
		{"from the group, when missing", config.GIDWhenMissing, []*model.ADObject{noGID(), upg("u-jdoe", 20, "7001")}, "7001", "", "", 1},
		{"user value kept, when missing", config.GIDWhenMissing, []*model.ADObject{withGID("100"), upg("u-jdoe", 20, "7001")}, "100", "", "", 0},
		{"invalid user value, when missing", config.GIDWhenMissing, []*model.ADObject{withGID("abc"), upg("u-jdoe", 20, "7001")}, "7001", "", "", 1},
		{"group overrides the user, when always", config.GIDWhenAlways, []*model.ADObject{withGID("100"), upg("u-jdoe", 20, "7001")}, "7001", "", "", 1},
		{"no group, when always", config.GIDWhenAlways, []*model.ADObject{withGID("100")}, "100", "", "", 0},
		{"no group found", config.GIDWhenMissing, []*model.ADObject{noGID()}, "", `has no gidNumber, and no AD group is named "u-jdoe"`, WarnIgnored, 0},
		{"case-insensitive name", config.GIDWhenMissing, []*model.ADObject{noGID(), upg("U-JDoe", 20, "7001")}, "7001", "", "", 1},
		{"out-of-range group gidNumber", config.GIDWhenMissing, []*model.ADObject{noGID(), upg("u-jdoe", 20, "4294967295")}, "", `gets its gidNumber from AD group CN=u-jdoe,OU=Groups,DC=example,DC=edu, but gidNumber "4294967295" is not a valid ID`, WarnIgnored, 0},
		{"duplicate group names", config.GIDWhenMissing, []*model.ADObject{noGID(), upg("u-jdoe", 20, "7001"), dup}, "7001", `another AD group is also named "u-jdoe"; gid_from_upg uses the first by DN, CN=u-jdoe,OU=Groups,DC=example,DC=edu`, WarnDuplicate, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := cfg(t, func(c *config.Config) {
				c.Mapping.Users.Required = []string{"uid", "uidNumber"}
				c.Mapping.Users.GIDFromUPG = &config.GIDFromUPG{GroupName: "u-{{ .uid }}", When: tc.when}
			})
			p := build(t, c, world{ad: tc.ad}, both)
			got := ""
			for _, op := range p.Ops {
				if op.Kind == AddEntry && model.DNEqual(op.DN, udn("jdoe")) {
					for _, a := range op.Attrs {
						if a.Name == "gidNumber" {
							got = strings.Join(a.Values, ",")
						}
					}
				}
			}
			if got != tc.wantGID {
				t.Errorf("gidNumber = %q, want %q\n%v", got, tc.wantGID, opStrs(p))
			}
			if tc.warn != "" {
				wantWarning(t, p, tc.kind, tc.warn)
			}
			if p.Counts.GIDFromUPG != tc.counted {
				t.Errorf("counted %d, want %d", p.Counts.GIDFromUPG, tc.counted)
			}
		})
	}
}

// With gid_from_upg, users.required may leave out gidNumber's source;
// without it, it may not. group_name must be a template.
func TestGIDFromUPGConfig(t *testing.T) {
	c := cfg(t)
	c.Mapping.Users.Required = []string{"uid", "uidNumber"}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "must include gidNumber") {
		t.Errorf("without gid_from_upg: %v", err)
	}
	c.Mapping.Users.GIDFromUPG = &config.GIDFromUPG{GroupName: "u-{{ .uid }}", When: config.GIDWhenMissing}
	if err := c.Validate(); err != nil {
		t.Errorf("with gid_from_upg: %v", err)
	}
	c.Mapping.Users.Required = []string{"uid"}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "must include uidNumber") {
		t.Errorf("uidNumber still required: %v", err)
	}
	c.Mapping.Users.Required = []string{"uid", "uidNumber"}
	for _, g := range []config.GIDFromUPG{{GroupName: "upg", When: "missing"}, {GroupName: "", When: "missing"}, {GroupName: "u-{{ .uid }}", When: "sometimes"}, {GroupName: "u-{{ .uid", When: "missing"}} {
		g := g
		c.Mapping.Users.GIDFromUPG = &g
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "gid_from_upg") {
			t.Errorf("%+v accepted: %v", g, err)
		}
	}
}

// A gidNumber that changes because the UPG group's changed is applied and
// reported like any gidNumber change.
func TestRuleGIDFromUPGChangeIsReported(t *testing.T) {
	c := cfg(t, func(c *config.Config) {
		c.Mapping.Users.GIDFromUPG = &config.GIDFromUPG{GroupName: "u-{{ .uid }}", When: config.GIDWhenAlways}
	})
	g := mkGroup(20, "u-jdoe")
	g.Attrs["gidNumber"] = []string{"7001"}
	p := build(t, c, world{ad: []*model.ADObject{mkUser(1, "jdoe"), g}, target: []*model.Entry{tUser(1, "jdoe"), uRec(1, "jdoe")}}, Options{Users: true})
	if !hasOp(p, "modify-entry "+udn("jdoe")+" replace gidNumber=7001") {
		t.Errorf("no modify: %v", opStrs(p))
	}
	wantWarning(t, p, WarnIDChanged, "gidNumber changes from 100 to 7001")
}
