// Package config loads and validates dolly.yaml.
//
// Validation is structural only: it never opens password, CA, or other
// referenced files, so a config (including dolly.yaml.template) validates on
// a host where those files don't exist yet. Secrets are read on use by
// Source.Password, Target.Password, and Notify.SMTPPassword, and `dolly
// check` will verify the files. The one file-system check at load time is the
// config file's own mode when it contains an inline password.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"
	"text/template/parse"
	"time"

	"github.com/go-ldap/ldap/v3"
	"gopkg.in/yaml.v3"
)

// Config mirrors dolly.yaml (see dolly.yaml.template).
type Config struct {
	Source  Source  `yaml:"source"`
	Target  Target  `yaml:"target"`
	Mapping Mapping `yaml:"mapping"`
	Sync    Sync    `yaml:"sync"`
	Notify  Notify  `yaml:"notify"`

	// Path is the absolute path of the file the config was loaded from.
	Path string `yaml:"-"`
}

// Source is the AD side.
type Source struct {
	URLs             []string `yaml:"urls"`
	CAFile           string   `yaml:"ca_file"`
	BindDN           string   `yaml:"bind_dn"`
	BindPassword     string   `yaml:"bind_password"`
	BindPasswordFile string   `yaml:"bind_password_file"`
	Users            Search   `yaml:"users"`
	Groups           Search   `yaml:"groups"`
	PageSize         int      `yaml:"page_size"`
}

// Search is a search base and filter.
type Search struct {
	Base   string `yaml:"base"`
	Filter string `yaml:"filter"`
}

// Target is the OpenLDAP side.
type Target struct {
	URL              string `yaml:"url"`
	StartTLS         bool   `yaml:"start_tls"`
	CAFile           string `yaml:"ca_file"`
	BindDN           string `yaml:"bind_dn"`
	BindPassword     string `yaml:"bind_password"`
	BindPasswordFile string `yaml:"bind_password_file"`
	UsersBase        string `yaml:"users_base"`
	GroupsBase       string `yaml:"groups_base"`
	StateBase        string `yaml:"state_base"`
	EmptyGroupMember string `yaml:"empty_group_member"`
}

// Mapping holds the attribute mapping for users and groups.
type Mapping struct {
	Users  UserMapping  `yaml:"users"`
	Groups GroupMapping `yaml:"groups"`
}

// UserMapping maps AD users to target entries.
type UserMapping struct {
	RDN           string            `yaml:"rdn"`
	ObjectClasses []string          `yaml:"object_classes"`
	Required      []string          `yaml:"required"`
	CreateOnly    []string          `yaml:"create_only"`
	Attributes    map[string]string `yaml:"attributes"`
}

// GroupMapping maps AD groups to target entries.
type GroupMapping struct {
	RDN           string            `yaml:"rdn"`
	ObjectClasses []string          `yaml:"object_classes"`
	Required      []string          `yaml:"required"`
	Attributes    map[string]string `yaml:"attributes"`
	Membership    []Membership      `yaml:"membership"`
	FlattenNested bool              `yaml:"flatten_nested"`
}

// Membership is one membership attribute written on target groups.
type Membership struct {
	Attribute string `yaml:"attribute"`
}

// Membership attribute names.
const (
	AttrMember    = "member"
	AttrMemberUID = "memberUid"
)

// Sync holds the sync policy.
type Sync struct {
	PruneUsers       bool     `yaml:"prune_users"`
	PruneAfterDays   int      `yaml:"prune_after_days"`
	DisabledShell    string   `yaml:"disabled_shell"`
	MaxDeletePercent float64  `yaml:"max_delete_percent"`
	MaxDeleteMin     int      `yaml:"max_delete_min"`
	LockTTL          Duration `yaml:"lock_ttl"`
	RunTimeout       Duration `yaml:"run_timeout"`
	NetworkTimeout   Duration `yaml:"network_timeout"`
}

// removedKeys explains config keys that no longer exist, so the unknown-key
// error says what to do instead of just "not found".
var removedKeys = map[string]string{
	"require_member_on_target": "sync.require_member_on_target was removed: a member is now always added to a target group only if its entry exists under users_base; delete the line",
}

// Notify configures email notifications. An empty SMTPHost disables them.
type Notify struct {
	SMTPHost      string   `yaml:"smtp_host"`
	SMTPPort      int      `yaml:"smtp_port"`
	StartTLS      bool     `yaml:"start_tls"`
	Username      string   `yaml:"username"`
	Password      string   `yaml:"password"`
	PasswordFile  string   `yaml:"password_file"`
	From          string   `yaml:"from"`
	To            []string `yaml:"to"`
	SubjectPrefix string   `yaml:"subject_prefix"`
	On            string   `yaml:"on"`
	RemindEvery   Duration `yaml:"remind_every"`
}

// Duration is a time.Duration written as a Go duration string ("45m").
type Duration struct{ time.Duration }

// UnmarshalYAML parses a Go duration string.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return fmt.Errorf("line %d: duration must be a string such as \"45m\"", n.Line)
	}
	v, err := time.ParseDuration(strings.TrimSpace(s))
	if err != nil {
		return fmt.Errorf("line %d: invalid duration %q (use Go syntax such as 30s, 45m, 24h)", n.Line, s)
	}
	d.Duration = v
	return nil
}

// Load reads, parses, and validates the config file at path. If the file
// holds an inline password it must not be readable by group or others.
func Load(path string) (*Config, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}
	c, err := Parse(data, abs)
	if err != nil {
		return nil, err
	}
	if c.hasInlinePassword() {
		st, err := os.Stat(abs)
		if err != nil {
			return nil, err
		}
		if perm := st.Mode().Perm(); perm&0o077 != 0 {
			return nil, fmt.Errorf("%s contains an inline password but has mode %04o; it must be readable only by its owner (chmod 600 %s), or move the password to a *_password_file",
				abs, perm, abs)
		}
	}
	return c, nil
}

// Parse decodes and validates config data. path is the config file's
// absolute path, used to resolve relative paths; it is not read.
func Parse(data []byte, path string) (*Config, error) {
	c := &Config{Path: path}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(c); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%s: config is empty", path)
		}
		return nil, fmt.Errorf("%s: %w", path, explainRemoved(err))
	}
	c.applyDefaults()
	c.resolvePaths()
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("%s: invalid config:\n%w", path, err)
	}
	return c, nil
}

// explainRemoved rewrites yaml.v3's unknown-field errors for removed keys.
func explainRemoved(err error) error {
	var te *yaml.TypeError
	if !errors.As(err, &te) {
		return err
	}
	changed := false
	for i, msg := range te.Errors {
		for key, why := range removedKeys {
			if strings.Contains(msg, "field "+key+" not found") {
				te.Errors[i] = msg + ": " + why
				changed = true
			}
		}
	}
	if !changed {
		return err
	}
	return te
}

func (c *Config) applyDefaults() {
	// spec: README says paged AD searches use a page size of 500.
	if c.Source.PageSize == 0 {
		c.Source.PageSize = 500
	}
}

func (c *Config) resolvePaths() {
	dir := filepath.Dir(c.Path)
	for _, p := range []*string{&c.Source.CAFile, &c.Source.BindPasswordFile, &c.Target.CAFile, &c.Target.BindPasswordFile, &c.Notify.PasswordFile} {
		if *p != "" && !filepath.IsAbs(*p) {
			*p = filepath.Join(dir, *p)
		}
	}
}

func (c *Config) hasInlinePassword() bool {
	return c.Source.BindPassword != "" || c.Target.BindPassword != "" || c.Notify.Password != ""
}

// Password returns the AD bind password, reading the file if configured.
func (s Source) Password() (string, error) { return secret(s.BindPassword, s.BindPasswordFile) }

// Password returns the target bind password, reading the file if configured.
func (t Target) Password() (string, error) { return secret(t.BindPassword, t.BindPasswordFile) }

// SMTPPassword returns the SMTP password, reading the file if configured.
func (n Notify) SMTPPassword() (string, error) { return secret(n.Password, n.PasswordFile) }

func secret(inline, file string) (string, error) {
	if inline != "" || file == "" {
		return inline, nil
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("reading password file: %w", err)
	}
	return strings.TrimRight(string(b), "\r\n"), nil
}

// Membership attribute helpers.

// HasMember reports whether groups carry `member` DNs (groupOfNames).
func (m GroupMapping) HasMember() bool { return m.hasAttr(AttrMember) }

// HasMemberUID reports whether groups carry `memberUid` values.
func (m GroupMapping) HasMemberUID() bool { return m.hasAttr(AttrMemberUID) }

func (m GroupMapping) hasAttr(a string) bool {
	for _, x := range m.Membership {
		if strings.EqualFold(x.Attribute, a) {
			return true
		}
	}
	return false
}

// IsCreateOnly reports whether attr is written only when a user is created.
func (m UserMapping) IsCreateOnly(attr string) bool {
	for _, a := range m.CreateOnly {
		if strings.EqualFold(a, attr) {
			return true
		}
	}
	return false
}

// Value is a compiled mapping value: either a plain AD attribute name or a
// Go template executed with the AD attributes (first values) as data.
type Value struct {
	Attr string
	Tmpl *template.Template
}

var attrName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9-]*$`)

// CompileValue compiles a mapping value. A value containing "{{" is a Go
// template; anything else must be a plain attribute name.
func CompileValue(name, src string) (Value, error) {
	if strings.Contains(src, "{{") {
		t, err := template.New(name).Option("missingkey=zero").Parse(src)
		if err != nil {
			return Value{}, err
		}
		return Value{Tmpl: t}, nil
	}
	s := strings.TrimSpace(src)
	if !attrName.MatchString(s) {
		return Value{}, fmt.Errorf("%q is neither an AD attribute name nor a Go template", src)
	}
	return Value{Attr: s}, nil
}

// Sources returns the AD attributes the value reads: the plain attribute, or
// every field a template references (.name). The AD reader requests exactly
// these, so it never has to fetch every attribute of every object.
func (v Value) Sources() []string {
	if v.Tmpl == nil {
		if v.Attr == "" {
			return nil
		}
		return []string{v.Attr}
	}
	var out []string
	seen := map[string]bool{}
	var walk func(n parse.Node)
	walk = func(n parse.Node) {
		switch n := n.(type) {
		case *parse.ListNode:
			if n == nil {
				return
			}
			for _, c := range n.Nodes {
				walk(c)
			}
		case *parse.ActionNode:
			walk(n.Pipe)
		case *parse.PipeNode:
			if n == nil {
				return
			}
			for _, c := range n.Cmds {
				walk(c)
			}
		case *parse.CommandNode:
			for _, a := range n.Args {
				walk(a)
			}
		case *parse.FieldNode:
			if len(n.Ident) > 0 && !seen[strings.ToLower(n.Ident[0])] {
				seen[strings.ToLower(n.Ident[0])] = true
				out = append(out, n.Ident[0])
			}
		case *parse.ChainNode:
			walk(n.Node)
		case *parse.IfNode:
			walk(n.Pipe)
			walk(n.List)
			walk(n.ElseList)
		case *parse.WithNode:
			walk(n.Pipe)
			walk(n.List)
			walk(n.ElseList)
		case *parse.RangeNode:
			walk(n.Pipe)
			walk(n.List)
			walk(n.ElseList)
		case *parse.TemplateNode:
			walk(n.Pipe)
		}
	}
	if v.Tmpl.Tree != nil {
		walk(v.Tmpl.Tree.Root)
	}
	return out
}

// Validate checks the config for structural errors and returns all of them.
func (c *Config) Validate() error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf("  "+format, a...)) }

	// source
	if len(c.Source.URLs) == 0 {
		add("source.urls: at least one domain controller URL is required")
	}
	for i, u := range c.Source.URLs {
		if err := checkURL(u); err != nil {
			add("source.urls[%d]: %v", i, err)
		}
	}
	if c.Source.BindDN == "" {
		add("source.bind_dn: required (a DN or a UPN such as svc-dolly@example.edu)")
	}
	checkPassword("source.bind_password", c.Source.BindPassword, c.Source.BindPasswordFile, true, add)
	for _, s := range []struct {
		name string
		s    Search
	}{{"source.users", c.Source.Users}, {"source.groups", c.Source.Groups}} {
		checkDN(s.name+".base", s.s.Base, true, add)
		if s.s.Filter == "" {
			add("%s.filter: required", s.name)
		} else if _, err := ldap.CompileFilter(s.s.Filter); err != nil {
			add("%s.filter: %v", s.name, err)
		}
	}
	if c.Source.PageSize < 1 || c.Source.PageSize > 1000 {
		add("source.page_size: must be between 1 and 1000 (AD's MaxPageSize), got %d", c.Source.PageSize)
	}

	// target
	if c.Target.URL == "" {
		add("target.url: required")
	} else if err := checkURL(c.Target.URL); err != nil {
		add("target.url: %v", err)
	} else if c.Target.StartTLS && strings.HasPrefix(strings.ToLower(c.Target.URL), "ldaps://") {
		add("target.start_tls: can't be combined with an ldaps:// URL")
	}
	checkDN("target.bind_dn", c.Target.BindDN, true, add)
	checkPassword("target.bind_password", c.Target.BindPassword, c.Target.BindPasswordFile, true, add)
	checkDN("target.users_base", c.Target.UsersBase, true, add)
	checkDN("target.groups_base", c.Target.GroupsBase, true, add)
	checkDN("target.state_base", c.Target.StateBase, true, add)
	if ub, err1 := ldap.ParseDN(c.Target.UsersBase); err1 == nil && c.Target.UsersBase != "" {
		if gb, err2 := ldap.ParseDN(c.Target.GroupsBase); err2 == nil && ub.EqualFold(gb) {
			add("target.users_base and target.groups_base must differ; Dolly tells users from groups by their container")
		}
	}
	c.checkStateBase(add)
	checkDN("target.empty_group_member", c.Target.EmptyGroupMember, false, add)

	c.validateMapping(add)

	// sync
	s := c.Sync
	if s.PruneAfterDays < 0 {
		add("sync.prune_after_days: must not be negative")
	}
	if s.MaxDeletePercent < 0 || s.MaxDeletePercent > 100 {
		add("sync.max_delete_percent: must be between 0 and 100")
	}
	if s.MaxDeleteMin < 0 {
		add("sync.max_delete_min: must not be negative")
	}
	for _, d := range []struct {
		name string
		d    time.Duration
	}{{"sync.lock_ttl", s.LockTTL.Duration}, {"sync.run_timeout", s.RunTimeout.Duration}, {"sync.network_timeout", s.NetworkTimeout.Duration}} {
		if d.d <= 0 {
			add("%s: required and must be positive", d.name)
		}
	}
	if s.RunTimeout.Duration > 0 && s.LockTTL.Duration > 0 && s.RunTimeout.Duration >= s.LockTTL.Duration {
		add("sync.run_timeout (%s) must be shorter than sync.lock_ttl (%s), or a live run could have its lock broken", s.RunTimeout.Duration, s.LockTTL.Duration)
	}

	// notify
	n := c.Notify
	checkPassword("notify.password", n.Password, n.PasswordFile, false, add)
	if n.SMTPHost != "" {
		if n.SMTPPort < 1 || n.SMTPPort > 65535 {
			add("notify.smtp_port: must be between 1 and 65535")
		}
		if n.From == "" {
			add("notify.from: required when notify.smtp_host is set")
		}
		if len(n.To) == 0 {
			add("notify.to: at least one recipient is required when notify.smtp_host is set")
		}
		switch n.On {
		case "failure", "changes", "always":
		default:
			add("notify.on: must be failure, changes, or always, got %q", n.On)
		}
		if n.RemindEvery.Duration <= 0 {
			add("notify.remind_every: required and must be positive")
		}
		if n.Username == "" && (n.Password != "" || n.PasswordFile != "") {
			add("notify.username: required when a password is set")
		}
		if n.Username != "" && n.Password == "" && n.PasswordFile == "" {
			add("notify.password: set password or password_file when username is set")
		}
	}
	return errors.Join(errs...)
}

func (c *Config) validateMapping(add func(string, ...any)) {
	u, g := c.Mapping.Users, c.Mapping.Groups
	checkValues := func(prefix string, attrs map[string]string) {
		if len(attrs) == 0 {
			add("%s.attributes: at least one attribute is required", prefix)
		}
		for name, src := range attrs {
			if !attrName.MatchString(name) {
				add("%s.attributes: %q is not a valid attribute name", prefix, name)
			}
			if _, err := CompileValue(name, src); err != nil {
				add("%s.attributes.%s: %v", prefix, name, err)
			}
		}
	}
	hasAll := func(prefix, field string, list []string, want ...string) {
		for _, w := range want {
			found := false
			for _, x := range list {
				if strings.EqualFold(x, w) {
					found = true
				}
			}
			if !found {
				add("%s.%s: must include %s", prefix, field, w)
			}
		}
	}
	lookup := func(attrs map[string]string, name string) (string, bool) {
		for k, v := range attrs {
			if strings.EqualFold(k, name) {
				return v, true
			}
		}
		return "", false
	}

	// users
	checkValues("mapping.users", u.Attributes)
	if len(u.ObjectClasses) == 0 {
		add("mapping.users.object_classes: required")
	}
	// spec: required attributes are absolute (CLAUDE.md), so the list may add
	// attributes but never drop the AD attributes that uid, uidNumber, and
	// gidNumber are mapped from. A value mapped through a template has no
	// single source attribute, so it can't be checked here; an empty result
	// still makes the user unusable (uid) or incomplete (the numbers).
	uidSrc, ok := lookup(u.Attributes, "uid")
	if !ok {
		add("mapping.users.attributes: uid is required (it names members and ownership records)")
	}
	for _, name := range []string{"uid", "uidNumber", "gidNumber"} {
		src, ok := lookup(u.Attributes, name)
		if !ok {
			if name != "uid" {
				add("mapping.users.attributes: %s is required (posixAccount needs it)", name)
			}
			continue
		}
		if v, err := CompileValue(name, src); err == nil && v.Attr != "" {
			hasAll("mapping.users", "required", u.Required, v.Attr)
		}
	}
	if u.RDN == "" {
		add("mapping.users.rdn: required")
	} else if rdnSrc, ok := lookup(u.Attributes, u.RDN); !ok {
		add("mapping.users.rdn: %q must be one of mapping.users.attributes", u.RDN)
	} else if !strings.EqualFold(u.RDN, "uid") && rdnSrc != uidSrc {
		// spec: roleOccupant DNs are <rdn>=<uid>,<users_base> and memberUid
		// ownership is derived from them, so the RDN value must be the uid.
		add("mapping.users.rdn: %q must map to the same value as uid (%q)", u.RDN, uidSrc)
	}
	for _, a := range u.CreateOnly {
		if _, ok := lookup(u.Attributes, a); !ok {
			add("mapping.users.create_only: %q is not in mapping.users.attributes", a)
		}
		if strings.EqualFold(a, u.RDN) || strings.EqualFold(a, "uid") {
			add("mapping.users.create_only: %q names the entry and can't be create-only", a)
		}
	}

	// groups
	checkValues("mapping.groups", g.Attributes)
	if len(g.ObjectClasses) == 0 {
		add("mapping.groups.object_classes: required")
	}
	hasAll("mapping.groups", "required", g.Required, "name", "gidNumber")
	if g.RDN == "" {
		add("mapping.groups.rdn: required")
	} else if _, ok := lookup(g.Attributes, g.RDN); !ok {
		add("mapping.groups.rdn: %q must be one of mapping.groups.attributes", g.RDN)
	}
	if len(g.Membership) == 0 {
		add("mapping.groups.membership: list member, memberUid, or both")
	}
	seen := map[string]bool{}
	for i, m := range g.Membership {
		switch m.Attribute {
		case AttrMember, AttrMemberUID:
		default:
			add("mapping.groups.membership[%d].attribute: must be %q or %q, got %q", i, AttrMember, AttrMemberUID, m.Attribute)
		}
		if seen[m.Attribute] {
			add("mapping.groups.membership: %q is listed twice", m.Attribute)
		}
		seen[m.Attribute] = true
	}
	for name := range g.Attributes {
		if strings.EqualFold(name, AttrMember) || strings.EqualFold(name, AttrMemberUID) {
			add("mapping.groups.attributes: %q is managed through membership and can't be mapped", name)
		}
	}
	if g.HasMember() && c.Target.EmptyGroupMember == "" {
		add("target.empty_group_member: required when mapping.groups.membership includes member (groupOfNames needs at least one member)")
	}
}

func checkURL(s string) error {
	u, err := url.Parse(s)
	if err != nil {
		return err
	}
	if u.Scheme != "ldap" && u.Scheme != "ldaps" {
		return fmt.Errorf("%q: scheme must be ldap:// or ldaps://", s)
	}
	if u.Host == "" {
		return fmt.Errorf("%q: host is missing", s)
	}
	return nil
}

// checkStateBase checks where state_base sits relative to the two bases.
// state_base may live inside users_base or groups_base (Dolly ignores that
// subtree when reading users and groups), but it must not equal either base,
// and neither base may live inside state_base.
func (c *Config) checkStateBase(add func(string, ...any)) {
	t := c.Target
	if t.StateBase == "" {
		return
	}
	sb, err := ldap.ParseDN(t.StateBase)
	if err != nil {
		return // checkDN reported it
	}
	for _, b := range []struct{ name, dn string }{{"users_base", t.UsersBase}, {"groups_base", t.GroupsBase}} {
		if b.dn == "" {
			continue
		}
		d, err := ldap.ParseDN(b.dn)
		if err != nil {
			continue
		}
		switch {
		case d.EqualFold(sb):
			add("target.state_base and target.%s must differ; state_base may live inside %s, but not be it", b.name, b.name)
		case sb.AncestorOfFold(d):
			add("target.%s must not be inside target.state_base (state_base may be inside %s, not the other way round)", b.name, b.name)
		}
	}
}

func checkDN(name, dn string, required bool, add func(string, ...any)) {
	if dn == "" {
		if required {
			add("%s: required", name)
		}
		return
	}
	if _, err := ldap.ParseDN(dn); err != nil {
		add("%s: invalid DN %q: %v", name, dn, err)
	}
}

// checkPassword enforces "inline or file, never both; empty means unset".
func checkPassword(name, inline, file string, required bool, add func(string, ...any)) {
	switch {
	case inline != "" && file != "":
		add("%s and %s_file are both set; set only one", name, name)
	case required && inline == "" && file == "":
		add("%s or %s_file: one is required", name, name)
	}
}

// Find returns the config path to use: explicit if given, else ./dolly.yaml,
// else $XDG_CONFIG_HOME/dolly/dolly.yaml (XDG_CONFIG_HOME defaults to
// ~/.config).
func Find(explicit string) (string, error) {
	home, _ := os.UserHomeDir()
	return find(explicit, os.Getenv("XDG_CONFIG_HOME"), home, func(p string) bool {
		st, err := os.Stat(p)
		return err == nil && !st.IsDir()
	})
}

func find(explicit, xdg, home string, exists func(string) bool) (string, error) {
	if explicit != "" {
		if !exists(explicit) {
			return "", fmt.Errorf("config file %s: %w", explicit, fs.ErrNotExist)
		}
		return explicit, nil
	}
	tried := []string{"dolly.yaml"}
	if exists("dolly.yaml") {
		return "dolly.yaml", nil
	}
	// XDG: a relative XDG_CONFIG_HOME is invalid and must be ignored.
	if xdg == "" || !filepath.IsAbs(xdg) {
		if home == "" {
			return "", errors.New("no config found: ./dolly.yaml does not exist and neither XDG_CONFIG_HOME nor HOME is set; use --config")
		}
		xdg = filepath.Join(home, ".config")
	}
	p := filepath.Join(xdg, "dolly", "dolly.yaml")
	tried = append(tried, p)
	if exists(p) {
		return p, nil
	}
	return "", fmt.Errorf("no config found (tried %s); run `dolly install` or pass --config", strings.Join(tried, ", "))
}
