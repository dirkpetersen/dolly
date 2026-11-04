package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/go-ldap/ldap/v3"
	"gopkg.in/yaml.v3"

	dolly "github.com/dirkpetersen/dolly"
	"github.com/dirkpetersen/dolly/internal/check"
	"github.com/dirkpetersen/dolly/internal/config"
	"github.com/dirkpetersen/dolly/internal/ldapconn"
)

// errAbort ends dolly config without writing anything (EOF, Ctrl-D, or a
// limit such as three failed binds).
var errAbort = errors.New("aborted; nothing was written")

// wizardIO reads the answers of dolly config: plain lines, and passwords
// without echo.
type wizardIO interface {
	Line(prompt string) (string, error)
	Password(prompt string) (string, error)
}

// newWizardIO returns the terminal reader of dolly config; tests replace it.
var newWizardIO = func(out io.Writer) wizardIO {
	return &ttyIO{r: bufio.NewReader(stdin), out: out, color: promptColor(out)}
}

// promptColor reports whether prompts are yellow: out is a terminal and
// NO_COLOR (https://no-color.org) is unset or empty.
func promptColor(out io.Writer) bool {
	f, ok := out.(*os.File)
	return ok && os.Getenv("NO_COLOR") == "" && isTerminal(f.Fd())
}

type ttyIO struct {
	r     *bufio.Reader
	out   io.Writer
	color bool
}

// prompt writes p, in yellow if enabled; the reset comes before any input
// (and before echo is turned off), so the color never leaks.
func (t *ttyIO) prompt(p string) {
	if t.color {
		p = "\033[33m" + p + "\033[0m"
	}
	fmt.Fprint(t.out, p)
}

func (t *ttyIO) Line(prompt string) (string, error) {
	t.prompt(prompt)
	s, err := t.r.ReadString('\n')
	if err != nil {
		return "", errAbort
	}
	return strings.TrimSpace(s), nil
}

// Password reads a line with echo off. A signal while echo is off restores
// the terminal before the process exits; nothing has been written then.
func (t *ttyIO) Password(prompt string) (string, error) {
	t.prompt(prompt)
	restore, err := echoOff(os.Stdin.Fd())
	if err != nil {
		return "", fmt.Errorf("turning off echo: %w", err)
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	done := make(chan struct{})
	go func() {
		select {
		case <-sig:
			restore()
			fmt.Fprintf(t.out, "\ndolly config: %v\n", errAbort)
			os.Exit(exitError)
		case <-done:
		}
	}()
	s, err := t.r.ReadString('\n')
	close(done)
	signal.Stop(sig)
	restore()
	if err != nil {
		return "", errAbort
	}
	return strings.TrimRight(s, "\r\n"), nil
}

type wizard struct {
	ctx context.Context
	io  wizardIO
	out io.Writer
}

// ask prompts with def in brackets; Enter keeps def. check rejects an
// answer, which is asked again.
func (w *wizard) ask(label, def string, check func(string) error) (string, error) {
	prompt := label + ": "
	if def != "" {
		prompt = label + " [" + def + "]: "
	}
	for {
		v, err := w.io.Line(prompt)
		if err != nil {
			return "", err
		}
		if v == "" {
			v = def
		}
		if err := check(v); err != nil {
			fmt.Fprintf(w.out, "  ✗ %v\n", err)
			continue
		}
		return v, nil
	}
}

func required(v string) error {
	if v == "" {
		return errors.New("a value is required")
	}
	return nil
}

func validDN(v string) error {
	if v == "" {
		return errors.New("a DN is required")
	}
	_, err := ldap.ParseDN(v)
	return err
}

func (w *wizard) yes(label string) (bool, error) {
	v, err := w.ask(label+" [y/N]", "", func(string) error { return nil })
	v = strings.ToLower(v)
	return v == "y" || v == "yes", err
}

// yesDefault is yes with Enter meaning yes; anything but y/yes/n/no asks again.
func (w *wizard) yesDefault(label string) (bool, error) {
	v, err := w.ask(label+" [Y/n]", "", func(v string) error {
		switch strings.ToLower(v) {
		case "", "y", "yes", "n", "no":
			return nil
		}
		return errors.New("enter y or n")
	})
	v = strings.ToLower(v)
	return v != "n" && v != "no", err
}

func splitList(v string) []string {
	var out []string
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = appendNew(out, s)
		}
	}
	return out
}

// servers asks for server URLs, fetches each chain (no credentials), and
// asks whether to trust the leaf certificates. It returns the URLs and,
// if trusted, their pins; nil pins keep the existing ca_file/pins, which is
// offered only for the same servers as now (keepFor, nil without any).
func (w *wizard) servers(def []string, multi bool, keepFor []string) (urls, pins []string, err error) {
	label := "Server URL (ldaps://host, or ldap://host for StartTLS)"
	if multi {
		label = "Server URLs, comma-separated (ldaps://host, or ldap://host for StartTLS)"
	}
	for {
		v, err := w.ask(label, strings.Join(def, ", "), func(v string) error {
			list := splitList(v)
			switch {
			case len(list) == 0:
				return errors.New("at least one URL is required")
			case !multi && len(list) > 1:
				return errors.New("enter one URL")
			}
			for _, u := range list {
				if s := ldapconn.Scheme(u); s != "ldap" && s != "ldaps" {
					return fmt.Errorf("%s: use ldaps://host or ldap://host", u)
				}
			}
			return nil
		})
		if err != nil {
			return nil, nil, err
		}
		urls, pins = splitList(v), nil
		fetched := true
		for _, u := range urls {
			chain, err := fetchAndPrint(w.ctx, u, w.out, w.out)
			if err != nil {
				fetched = false
				break
			}
			pins = appendNew(pins, ldapconn.Fingerprint(chain[0]))
		}
		fmt.Fprintln(w.out)
		def = urls
		if !fetched {
			fmt.Fprintln(w.out, "  Could not fetch the certificate. (A plaintext target, ldap:// without StartTLS, can't be set up here: edit dolly.yaml by hand.)")
			if ok, err := w.yes("Enter the URLs again? (No ends dolly config)"); err != nil || !ok {
				if err == nil {
					err = errAbort
				}
				return nil, nil, err
			}
			continue
		}
		if ok, err := w.yesDefault("Trust these certificates (they become tls_fingerprint, and ca_file is removed)?"); err != nil || ok {
			return urls, pins, err
		}
		if keepFor != nil && sameServers(urls, keepFor) {
			if ok, err := w.yes("Keep the existing ca_file/tls_fingerprint instead?"); err != nil || ok {
				return urls, nil, err
			}
		}
		fmt.Fprintln(w.out, "  Enter the URLs again.")
	}
}

// movedFile describes a password file that moves to name in dir, or "".
func movedFile(resolved, raw, dir, name string) string {
	if resolved == "" || resolved == filepath.Join(dir, name) {
		return ""
	}
	return raw + " -> " + name + " (the old file is left in place)"
}

// sameServers reports whether a and b name the same host:port set (default
// ports filled in, case-insensitive).
func sameServers(a, b []string) bool {
	norm := func(list []string) string {
		var out []string
		for _, raw := range list {
			u, err := url.Parse(raw)
			if err != nil {
				return "\x00"
			}
			port := u.Port()
			if port == "" {
				port = map[string]string{"ldap": "389", "ldaps": "636"}[strings.ToLower(u.Scheme)]
			}
			out = appendNew(out, strings.ToLower(net.JoinHostPort(u.Hostname(), port)))
		}
		slices.Sort(out)
		return strings.Join(out, " ")
	}
	return norm(a) == norm(b)
}

// tlsOptions are the TLS settings a side has after the wizard: the new
// pins, or the existing ca_file/pins.
func tlsOptions(pins []string, caFile string, oldPins []string) (string, []string) {
	if pins != nil {
		return "", pins
	}
	return caFile, oldPins
}

// bind asks for a bind name and password and tests them at once with
// dial, up to three times. keepPw is the current password ("" if none).
func (w *wizard) bind(label, defName, keepPw string, name func(string) (string, error), dial func(name, pw string) (check.Conn, error)) (string, string, check.Conn, error) {
	for try := 1; ; try++ {
		raw, err := w.ask(label, defName, required)
		if err != nil {
			return "", "", nil, err
		}
		bindName, err := name(raw)
		if err != nil {
			fmt.Fprintf(w.out, "  ✗ %v\n", err)
			try--
			continue
		}
		prompt := "Password: "
		if keepPw != "" {
			prompt = "Password [Enter keeps the current one]: "
		}
		pw, err := w.io.Password(prompt)
		if err != nil {
			return "", "", nil, err
		}
		if pw == "" {
			pw = keepPw
		}
		if pw == "" {
			fmt.Fprintln(w.out, "  ✗ a password is required")
			try--
			continue
		}
		conn, err := dial(bindName, pw)
		if err == nil {
			fmt.Fprintf(w.out, "  ✓ bind as %s works\n", bindName)
			return raw, pw, conn, nil
		}
		fmt.Fprintf(w.out, "  ✗ bind failed: %v\n", err)
		if try == 3 {
			return "", "", nil, fmt.Errorf("bind failed 3 times; %w", errAbort)
		}
		fmt.Fprintf(w.out, "  Each failed bind can count toward the account's lockout. Try again (%d of 3 tries left).\n", 3-try)
		defName = raw
	}
}

// searchOne runs a paged subtree search for one entry and says whether
// the base returns any.
func (w *wizard) searchOne(conn check.Conn, name, base, filter string) {
	req := ldap.NewSearchRequest(base, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 0, 0, false, filter, []string{"objectGUID"},
		[]ldap.Control{ldap.NewControlPaging(1)})
	res, err := conn.Search(req)
	if err != nil && ldap.IsErrorWithCode(err, ldap.LDAPResultSizeLimitExceeded) && res != nil && len(res.Entries) > 0 {
		err = nil
	}
	switch {
	case err != nil:
		fmt.Fprintf(w.out, "  ! %s %s: search failed: %v (saved anyway)\n", name, base, err)
	case len(res.Entries) == 0:
		fmt.Fprintf(w.out, "  ! %s %s: no entries match %s (saved anyway; a sync stops while AD returns none)\n", name, base, filter)
	default:
		fmt.Fprintf(w.out, "  ✓ %s %s returns entries\n", name, base)
	}
}

// exists says whether dn exists on the target.
func (w *wizard) exists(conn check.Conn, name, dn string) {
	_, err := conn.Search(ldap.NewSearchRequest(dn, ldap.ScopeBaseObject, ldap.NeverDerefAliases, 0, 0, false, "(objectClass=*)", []string{"1.1"}, nil))
	switch {
	case err == nil:
		fmt.Fprintf(w.out, "  ✓ %s %s exists\n", name, dn)
	case ldap.IsErrorWithCode(err, ldap.LDAPResultNoSuchObject):
		fmt.Fprintf(w.out, "  - %s %s does not exist yet; the first real run creates it\n", name, dn)
	default:
		fmt.Fprintf(w.out, "  ! %s %s: %v\n", name, dn, err)
	}
}

// rawValue returns the scalar at path in the YAML document, or "".
func rawValue(doc *yaml.Node, path ...string) string {
	n := doc
	if n.Kind == yaml.DocumentNode && len(n.Content) == 1 {
		n = n.Content[0]
	}
	for _, k := range path {
		if n = mapValue(n, k); n == nil {
			return ""
		}
	}
	if n.Kind != yaml.ScalarNode {
		return ""
	}
	return n.Value
}

// defaultConfigPath is $XDG_CONFIG_HOME/dolly/dolly.yaml (~/.config without it).
func defaultConfigPath() (string, error) {
	xdg := os.Getenv("XDG_CONFIG_HOME")
	if xdg == "" || !filepath.IsAbs(xdg) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		xdg = filepath.Join(home, ".config")
	}
	return filepath.Join(xdg, "dolly", "dolly.yaml"), nil
}

// mappingFor returns the template's mapping.users and mapping.groups,
// changed for the group schema and the AD attribute the uid comes from.
func mappingFor(rfc2307 bool, uidSrc string) (users, groups *yaml.Node, err error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(dolly.ConfigTemplate, &doc); err != nil {
		return nil, nil, err
	}
	m := mapValue(doc.Content[0], "mapping")
	users, groups = mapValue(m, "users"), mapValue(m, "groups")
	attrs := mapValue(users, "attributes")
	mapValue(attrs, "uid").Value = uidSrc
	mapValue(attrs, "cn").Value = uidSrc
	mapValue(users, "required").Content[0].Value = uidSrc
	mapValue(attrs, "homeDirectory").Value = fmt.Sprintf(`{{ or .unixHomeDirectory (printf "/home/%%s" .%s) }}`, uidSrc)
	if rfc2307 {
		mapValue(groups, "object_classes").Content = flowList([]string{"posixGroup"}, false).Content
		ms := mapValue(groups, "membership")
		ms.Content = ms.Content[1:] // memberUid only
	}
	return users, groups, nil
}

func runConfigWizard(args []string, stdout, stderr io.Writer) int {
	fs := newFlags("config", "Create or edit the config interactively: the AD and target servers (their certificates are\n"+
		"fetched and pinned after you confirm them), the bind names and passwords (tested at once; the\n"+
		"passwords go to ad.secret and ldap.secret, mode 0600), the bases, and the group schema. Nothing\n"+
		"is written until you confirm the summary; then dolly check runs. Needs a terminal.", stderr, "")
	cfgPath := fs.String("config", "", "config file to create or edit (default: the config dolly finds, else $XDG_CONFIG_HOME/dolly/dolly.yaml)")
	if code, ok := parse(fs, args); !ok {
		return code
	}
	if !stdinIsTTY() {
		fmt.Fprintln(stderr, "dolly config: stdin is not a terminal; dolly config is interactive (edit the config by hand, or run it in a terminal)")
		return exitError
	}
	if err := configWizard(*cfgPath, stdout); err != nil {
		fmt.Fprintf(stderr, "dolly config: %v\n", err)
		return exitError
	}
	return exitOK
}

func configWizard(cfgFlag string, out io.Writer) error {
	path := cfgFlag
	if path == "" {
		var err error
		if path, err = config.Find(""); err != nil {
			if path, err = defaultConfigPath(); err != nil {
				return err
			}
		}
	}
	path, _ = filepath.Abs(path)
	// The real file is written, so a symlinked config stays a link; the
	// password files go beside the path Dolly finds (relative
	// *_password_file values resolve against it).
	realPath := path
	if p, err := filepath.EvalSymlinks(path); err == nil {
		realPath = p
	}
	orig, err := os.ReadFile(path)
	exists := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	base := orig
	switch {
	case exists && bytes.Equal(orig, dolly.ConfigTemplate): // as dolly install writes it
		exists, base = false, dolly.ConfigTemplate
		fmt.Fprintf(out, "Found the unedited template at %s; treating it as a new config.\n", path)
	case exists:
		fmt.Fprintf(out, "Editing %s. Enter keeps the value in [brackets].\n", path)
	default:
		base = dolly.ConfigTemplate
		fmt.Fprintf(out, "Creating %s from the template.\n", path)
	}
	cur, err := config.Parse(base, path)
	if err != nil {
		return fmt.Errorf("%s: %w (fix it by hand first)", path, err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(base, &doc); err != nil {
		return err
	}
	def := func(p ...string) string { // the current value, or "" for a new config
		if !exists {
			return ""
		}
		return rawValue(&doc, p...)
	}
	w := &wizard{ctx: context.Background(), io: newWizardIO(out), out: out}
	timeout := cur.Sync.NetworkTimeout.Duration
	header := func(n int, what string) { fmt.Fprintf(out, "\n[%d/7] %s\n", n, what) }

	// 1-3: the source.
	header(1, "Source servers (AD domain controllers)")
	var srcDef []string
	if exists {
		srcDef = cur.Source.URLs
	}
	var srcKeep []string
	if exists && (cur.Source.CAFile != "" || len(cur.Source.TLSFingerprint) > 0) {
		srcKeep = cur.Source.URLs
	}
	srcURLs, srcPins, err := w.servers(srcDef, true, srcKeep)
	if err != nil {
		return err
	}
	srcCA, srcTLS := tlsOptions(srcPins, cur.Source.CAFile, cur.Source.TLSFingerprint)

	header(2, "Source bind account")
	keep := ""
	if exists {
		keep, _ = cur.Source.Password()
	}
	usersBase, groupsBase := def("source", "users", "base"), def("source", "groups", "base")
	srcBind, srcPw, srcConn, err := w.bind("Bind name (a DN, a UPN such as svc-dolly@example.edu, or a bare account name)", def("source", "bind_dn"), keep,
		func(raw string) (string, error) {
			n := config.BindName(raw, usersBase, groupsBase)
			if !strings.ContainsAny(n, "=@\\") {
				return "", fmt.Errorf("%q is a bare name, but no base with DC= parts is known yet; enter it as a UPN (name@domain) or a DN", raw)
			}
			return n, nil
		},
		// Every DC is tried and reported, but invalid credentials stop at
		// once: each try counts toward the account's lockout.
		func(name, pw string) (check.Conn, error) {
			var good check.Conn
			var last error
			invalid := false
			for _, u := range srcURLs {
				c, err := checkDial(w.ctx, ldapconn.Options{URL: u, StartTLS: ldapconn.Scheme(u) == "ldap", CAFile: srcCA, Pins: srcTLS,
					Timeout: timeout, BindDN: name, Password: pw})
				if err != nil {
					last = fmt.Errorf("%s: %w", u, err)
					if invalid = ldap.IsErrorWithCode(err, ldap.LDAPResultInvalidCredentials); invalid {
						break
					}
					fmt.Fprintf(w.out, "  ! %s: %v\n", u, err)
					continue
				}
				fmt.Fprintf(w.out, "  ✓ %s: bind works\n", u)
				if good == nil {
					good = c
				} else {
					c.Close()
				}
			}
			if good != nil && !invalid {
				return good, nil
			}
			if good != nil {
				good.Close()
			}
			return nil, last
		})
	if err != nil {
		return err
	}
	defer srcConn.Close()

	header(3, "Source search bases")
	if usersBase, err = w.ask("Users base (for example OU=People,DC=example,DC=edu)", usersBase, validDN); err != nil {
		return err
	}
	w.searchOne(srcConn, "users base", usersBase, cur.Source.Users.Filter)
	if groupsBase, err = w.ask("Groups base (for example OU=Groups,DC=example,DC=edu)", groupsBase, validDN); err != nil {
		return err
	}
	w.searchOne(srcConn, "groups base", groupsBase, cur.Source.Groups.Filter)

	// 4-6: the target.
	header(4, "Target server (OpenLDAP)")
	var tgtDef []string
	if exists {
		tgtDef = []string{cur.Target.URL}
	}
	var tgtKeep []string
	if exists && (cur.Target.CAFile != "" || len(cur.Target.TLSFingerprint) > 0) {
		tgtKeep = tgtDef
	}
	tgtURLs, tgtPins, err := w.servers(tgtDef, false, tgtKeep)
	if err != nil {
		return err
	}
	tgtURL := tgtURLs[0]
	startTLS := ldapconn.Scheme(tgtURL) == "ldap"
	tgtCA, tgtTLS := tlsOptions(tgtPins, cur.Target.CAFile, cur.Target.TLSFingerprint)

	header(5, "Target bind account")
	keep = ""
	if exists {
		keep, _ = cur.Target.Password()
	}
	tgtBind, tgtPw, tgtConn, err := w.bind("Bind DN (for example cn=admin,dc=example,dc=org)", def("target", "bind_dn"), keep,
		func(raw string) (string, error) { return raw, validDN(raw) },
		func(name, pw string) (check.Conn, error) {
			return checkDial(w.ctx, ldapconn.Options{URL: tgtURL, StartTLS: startTLS, CAFile: tgtCA, Pins: tgtTLS,
				Timeout: timeout, BindDN: name, Password: pw})
		})
	if err != nil {
		return err
	}
	defer tgtConn.Close()

	header(6, "Target bases")
	nc := ""
	if res, err := tgtConn.Search(ldap.NewSearchRequest("", ldap.ScopeBaseObject, ldap.NeverDerefAliases, 0, 0, false, "(objectClass=*)", []string{"namingContexts"}, nil)); err == nil && len(res.Entries) > 0 {
		nc = res.Entries[0].GetAttributeValue("namingContexts")
	}
	under := func(rdn string) string {
		if nc == "" {
			return ""
		}
		return rdn + "," + nc
	}
	bases := map[string]string{}
	for _, b := range []struct{ key, rdn string }{{"users_base", "ou=people"}, {"groups_base", "ou=group"}, {"state_base", "ou=dolly"}} {
		d := def("target", b.key)
		if d == "" {
			d = under(b.rdn)
		}
		v, err := w.ask(b.key, d, validDN)
		if err != nil {
			return err
		}
		bases[b.key] = v
		w.exists(tgtConn, b.key, v)
	}

	header(7, "Group schema and uid")
	curRFC2307 := !cur.Mapping.Groups.HasMember()
	schemaDef := "rfc2307bis"
	if curRFC2307 {
		schemaDef = "rfc2307"
	}
	fmt.Fprintln(out, "  rfc2307bis: groupOfNames + posixGroup, members as member and memberUid")
	fmt.Fprintln(out, "  rfc2307:    posixGroup only, members as memberUid")
	// The server's posixGroup decides: STRUCTURAL (nis.schema) is RFC 2307,
	// AUXILIARY is rfc2307bis; groups of the other schema fail to add.
	serverSchema := ""
	switch kind, _ := check.ReadPosixGroupKind(tgtConn); kind {
	case "STRUCTURAL":
		serverSchema = "rfc2307"
		fmt.Fprintln(out, "  server schema: posixGroup is STRUCTURAL (RFC 2307), so the default is rfc2307")
	case "AUXILIARY":
		serverSchema = "rfc2307bis"
		fmt.Fprintln(out, "  server schema: posixGroup is AUXILIARY (rfc2307bis), so the default is rfc2307bis")
	default:
		fmt.Fprintln(out, "  could not read the server schema; check which one your server has")
	}
	if serverSchema != "" {
		schemaDef = serverSchema
	}
	var schema string
	for {
		if schema, err = w.ask("Group schema (rfc2307bis or rfc2307)", schemaDef, func(v string) error {
			if v != "rfc2307bis" && v != "rfc2307" {
				return errors.New("enter rfc2307bis or rfc2307")
			}
			return nil
		}); err != nil {
			return err
		}
		if serverSchema == "" || schema == serverSchema {
			break
		}
		kind := map[string]string{"rfc2307": "STRUCTURAL", "rfc2307bis": "AUXILIARY"}[serverSchema]
		fmt.Fprintf(out, "  ! the server's posixGroup is %s; %s groups will fail to add\n", kind, schema)
		ok, err := w.yes("Use " + schema + " anyway?")
		if err != nil {
			return err
		}
		if ok {
			break
		}
	}
	rfc2307 := schema == "rfc2307"
	emptyMember := ""
	if !rfc2307 {
		d := def("target", "empty_group_member")
		if d == "" {
			d = under("cn=empty")
		}
		if emptyMember, err = w.ask("Placeholder member for empty groups (empty_group_member)", d, validDN); err != nil {
			return err
		}
	}
	curUID := cur.Mapping.Users.Attributes["uid"]
	uidSrc, err := w.ask("AD attribute the uid comes from (sAMAccountName or uid)", curUID, func(v string) error {
		switch {
		case v == curUID && strings.Contains(v, "{{") && rfc2307 != curRFC2307:
			return errors.New("the uid comes from a template now; a schema change rewrites the mapping from the template, so choose sAMAccountName or uid")
		case v != "sAMAccountName" && v != "uid" && v != curUID:
			return errors.New("enter sAMAccountName or uid")
		}
		return nil
	})
	if err != nil {
		return err
	}
	mappingChanged := rfc2307 != curRFC2307 || uidSrc != curUID

	// Build the edits.
	dir := filepath.Dir(path)
	src := pinSide{section: "source", edits: []pinEdit{
		{"urls", &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Content: flowList(srcURLs, false).Content}, true},
		{"bind_dn", scalar("!!str", srcBind), true},
		{"bind_password", scalar("!!str", ""), false},
		{"bind_password_file", scalar("!!str", "ad.secret"), true},
		{"users.base", scalar("!!str", usersBase), true},
		{"groups.base", scalar("!!str", groupsBase), true},
	}}
	tgt := pinSide{section: "target", edits: []pinEdit{
		{"url", scalar("!!str", tgtURL), true},
		{"start_tls", scalar("!!bool", fmt.Sprint(startTLS)), startTLS},
		{"bind_dn", scalar("!!str", tgtBind), true},
		{"bind_password", scalar("!!str", ""), false},
		{"bind_password_file", scalar("!!str", "ldap.secret"), true},
		{"users_base", scalar("!!str", bases["users_base"]), true},
		{"groups_base", scalar("!!str", bases["groups_base"]), true},
		{"state_base", scalar("!!str", bases["state_base"]), true},
	}}
	if emptyMember != "" {
		tgt.edits = append(tgt.edits, pinEdit{"empty_group_member", scalar("!!str", emptyMember), true})
	}
	if srcPins != nil {
		src.remove = []string{"ca_file"}
		src.edits = append(src.edits, pinEdit{"tls_fingerprint", flowList(srcPins, true), true})
	}
	if tgtPins != nil {
		tgt.remove = []string{"ca_file"}
		tgt.edits = append(tgt.edits, pinEdit{"tls_fingerprint", flowList(tgtPins, true), true})
	}
	sides := []pinSide{src, tgt}
	if mappingChanged {
		users, groups, err := mappingFor(rfc2307, uidSrc)
		if err != nil {
			return err
		}
		sides = append(sides, pinSide{section: "mapping", edits: []pinEdit{{"users", users, true}, {"groups", groups, true}}})
	}
	notifyNote := ""
	if !exists { // the template's mx.example.edu would fail every check and run
		sides = append(sides, pinSide{section: "notify", edits: []pinEdit{{"smtp_host", scalar("!!str", ""), true}}})
		notifyNote = "off (smtp_host empty); edit the notify section by hand to get mail"
	}

	// Summary, without passwords.
	trust := func(pins []string) string {
		if pins == nil {
			return "existing ca_file/tls_fingerprint kept"
		}
		return "tls_fingerprint " + strings.Join(pins, ", ") + " (ca_file removed)"
	}
	mapping := "unchanged"
	if mappingChanged {
		mapping = "mapping.users and mapping.groups rewritten from the template (custom attributes and gid_from_upg are lost)"
	}
	fmt.Fprintf(out, "\nSummary\n")
	for _, l := range [][2]string{
		{"source.urls", strings.Join(srcURLs, ", ")}, {"source TLS", trust(srcPins)}, {"source.bind_dn", srcBind},
		{"source password", "written to " + filepath.Join(dir, "ad.secret")}, {"source.bind_password_file", movedFile(cur.Source.BindPasswordFile, def("source", "bind_password_file"), dir, "ad.secret")},
		{"source.users.base", usersBase}, {"source.groups.base", groupsBase},
		{"target.url", fmt.Sprintf("%s (start_tls %v)", tgtURL, startTLS)}, {"target TLS", trust(tgtPins)}, {"target.bind_dn", tgtBind},
		{"target password", "written to " + filepath.Join(dir, "ldap.secret")}, {"target.bind_password_file", movedFile(cur.Target.BindPasswordFile, def("target", "bind_password_file"), dir, "ldap.secret")},
		{"target.users_base", bases["users_base"]}, {"target.groups_base", bases["groups_base"]}, {"target.state_base", bases["state_base"]},
		{"group schema", schema}, {"empty_group_member", emptyMember}, {"uid from", uidSrc}, {"mapping", mapping}, {"notify", notifyNote},
	} {
		if l[1] != "" {
			fmt.Fprintf(out, "  %-20s %s\n", l[0]+":", l[1])
		}
	}
	if ok, err := w.yes("Write " + path + "?"); err != nil || !ok {
		if err == nil {
			fmt.Fprintln(out, "Nothing written.")
		}
		return err
	}

	edited, err := editConfig(base, sides)
	if err != nil {
		return err
	}
	if err := writeConfigSet(realPath, path, edited, map[string]string{"ad.secret": srcPw, "ldap.secret": tgtPw}); err != nil {
		return err
	}
	fmt.Fprintf(out, "Wrote %s, %s, and %s.\n\nRunning dolly check:\n", path, filepath.Join(dir, "ad.secret"), filepath.Join(dir, "ldap.secret"))
	res := check.Run(w.ctx, check.Options{ConfigPath: path, Out: out, Dial: checkDial, SMTP: notifyOptions})
	if res.Failed > 0 {
		fmt.Fprintln(out, "\nThe notify section is not part of dolly config; edit it by hand.")
		noun := "problems"
		if res.Failed == 1 {
			noun = "problem"
		}
		return fmt.Errorf("files written; dolly check found %d %s", res.Failed, noun)
	}
	return nil
}

// writeConfigSet writes the config (realPath, the file behind configPath)
// and the password files (names in configPath's directory) atomically,
// validates the config, and on failure restores every file as it was.
// Signals are held off while it writes; one that came ends the run after.
func writeConfigSet(realPath, configPath string, data []byte, secrets map[string]string) error {
	dir := filepath.Dir(configPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	err := writeConfigFiles(realPath, configPath, data, secrets)
	signal.Stop(sig)
	select {
	case <-sig:
		if err == nil {
			err = errors.New("interrupted after writing; files are complete")
		}
	default:
	}
	return err
}

func writeConfigFiles(realPath, configPath string, data []byte, secrets map[string]string) error {
	dir := filepath.Dir(configPath)

	type file struct {
		path    string
		data    []byte
		old     []byte
		existed bool
	}
	files := []*file{{path: realPath, data: data}}
	for name, pw := range secrets {
		files = append(files, &file{path: filepath.Join(dir, name), data: []byte(pw + "\n")})
	}
	for _, f := range files {
		old, err := os.ReadFile(f.path)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		f.old, f.existed = old, err == nil
	}
	restore := func() error {
		var errs error
		for _, f := range files {
			if f.existed {
				errs = errors.Join(errs, writeConfigFile(f.path, f.old))
			} else if err := os.Remove(f.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				errs = errors.Join(errs, err)
			}
		}
		return errs
	}
	for _, f := range files {
		if err := writeConfigFile(f.path, f.data); err != nil {
			return errors.Join(fmt.Errorf("writing %s: %w", f.path, err), restore())
		}
	}
	if _, err := reloadConfig(configPath); err != nil {
		if rerr := restore(); rerr != nil {
			return fmt.Errorf("the new config is invalid (%v), and restoring the original files failed: %v", err, rerr)
		}
		return fmt.Errorf("the new config is invalid, so the original files were restored: %v", err)
	}
	return nil
}
