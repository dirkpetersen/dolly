// Package check implements `dolly check`: it tests the config, every AD
// domain controller, the target, and SMTP, printing one line per check
// (✓ passed, ✗ failed, ! warning). It never writes to either directory and
// sends mail only when asked to (--send-test-mail).
package check

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/go-ldap/ldap/v3"

	"github.com/dirkpetersen/dolly/internal/config"
	"github.com/dirkpetersen/dolly/internal/ldapconn"
	"github.com/dirkpetersen/dolly/internal/model"
	"github.com/dirkpetersen/dolly/internal/notify"
	"github.com/dirkpetersen/dolly/internal/target"
)

// Conn is the part of an LDAP connection check uses.
type Conn interface {
	Search(*ldap.SearchRequest) (*ldap.SearchResult, error)
	Close() error
}

// DialFunc opens an authenticated connection (TLS set up, bound).
type DialFunc func(ctx context.Context, o ldapconn.Options) (Conn, error)

// Dial is the default DialFunc: ldapconn.Dial.
func Dial(ctx context.Context, o ldapconn.Options) (Conn, error) {
	c, err := ldapconn.Dial(ctx, o)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// Options configure a check.
type Options struct {
	// ConfigPath is the config file to check (from config.Find).
	ConfigPath string
	// GroupsOnly is a groups-only deployment (dolly sync --groups): the AD
	// users base and a size-limited users_base are warnings, not failures,
	// since such runs never read either in full.
	GroupsOnly bool
	// SendTestMail sends one test message to notify.to.
	SendTestMail bool
	Out          io.Writer
	Dial         DialFunc       // default Dial
	SMTP         notify.Options // RootCAs for tests; Timeout comes from the config
}

// Result counts the checks.
type Result struct{ Passed, Failed, Warnings int }

type printer struct {
	w   io.Writer
	res Result
}

func (p *printer) section(format string, a ...any) { fmt.Fprintf(p.w, "\n"+format+"\n", a...) }
func (p *printer) ok(format string, a ...any) {
	p.res.Passed++
	fmt.Fprintf(p.w, "  ✓ "+format+"\n", a...)
}
func (p *printer) fail(format string, a ...any) {
	p.res.Failed++
	fmt.Fprintf(p.w, "  ✗ "+format+"\n", a...)
}
func (p *printer) warn(format string, a ...any) {
	p.res.Warnings++
	fmt.Fprintf(p.w, "  ! "+format+"\n", a...)
}
func (p *printer) info(format string, a ...any) { fmt.Fprintf(p.w, "  - "+format+"\n", a...) }
func (p *printer) note(format string, a ...any) { fmt.Fprintf(p.w, "      "+format+"\n", a...) }

// Run runs every check and prints the results. The caller exits 1 if
// Result.Failed > 0.
func Run(ctx context.Context, o Options) Result {
	if o.Dial == nil {
		o.Dial = Dial
	}
	p := &printer{w: o.Out}
	fmt.Fprintf(p.w, "Config %s\n", o.ConfigPath)
	cfg, err := config.Load(o.ConfigPath)
	if err != nil {
		p.fail("%v", err)
		return p.summary()
	}
	p.ok("loaded and valid")
	secrets(p, cfg)

	timeout := cfg.Sync.NetworkTimeout.Duration
	ctx, cancel := context.WithTimeout(ctx, cfg.Sync.RunTimeout.Duration)
	defer cancel()

	checkAD(ctx, p, cfg, o)
	checkTarget(ctx, p, cfg, o)
	checkSMTP(ctx, p, cfg, o, timeout)
	return p.summary()
}

func (p *printer) summary() Result {
	fmt.Fprintln(p.w)
	r := p.res
	switch {
	case r.Failed > 0:
		fmt.Fprintf(p.w, "%s failed, %d passed, %s.\n", plural(r.Failed, "check"), r.Passed, plural(r.Warnings, "warning"))
	case r.Warnings > 0:
		fmt.Fprintf(p.w, "All %s passed, with %s.\n", plural(r.Passed, "check"), plural(r.Warnings, "warning"))
	default:
		fmt.Fprintf(p.w, "All %s passed.\n", plural(r.Passed, "check"))
	}
	return r
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// secrets checks that every configured password can be read, is not
// empty, and (for password files) is readable only by its owner.
func secrets(p *printer, cfg *config.Config) {
	type sec struct {
		name, inline, file string
		get                func() (string, error)
	}
	list := []sec{
		{"source.bind_password", cfg.Source.BindPassword, cfg.Source.BindPasswordFile, cfg.Source.Password},
		{"target.bind_password", cfg.Target.BindPassword, cfg.Target.BindPasswordFile, cfg.Target.Password},
	}
	if cfg.Notify.SMTPHost != "" && cfg.Notify.Username != "" {
		list = append(list, sec{"notify.password", cfg.Notify.Password, cfg.Notify.PasswordFile, cfg.Notify.SMTPPassword})
	}
	for _, s := range list {
		pw, err := s.get()
		switch {
		case err != nil:
			p.fail("%s_file %s: %v", s.name, s.file, err)
			continue
		case pw == "":
			p.fail("%s is empty", s.name)
			continue
		}
		if s.inline != "" {
			p.ok("%s: inline (the config is mode 0600 or stricter)", s.name)
			continue
		}
		if st, err := os.Stat(s.file); err == nil && st.Mode().Perm()&0o077 != 0 {
			p.warn("%s_file %s has mode %04o; make it readable only by you (chmod 600 %s)", s.name, s.file, st.Mode().Perm(), s.file)
			continue
		}
		p.ok("%s_file %s: readable", s.name, s.file)
	}
}

func tlsMode(url string, startTLS bool) string {
	switch {
	case ldapconn.Scheme(url) == "ldaps":
		return "LDAPS"
	case startTLS:
		return "StartTLS"
	}
	return "no TLS"
}

// checkAD tests each DC's TLS and bind, then a size-1 paged search of the
// users and groups bases on the first DC that answered.
func checkAD(ctx context.Context, p *printer, cfg *config.Config, o Options) {
	p.section("AD (source.urls; sync tries them in random order)")
	pw, err := cfg.Source.Password()
	if err != nil || pw == "" {
		p.fail("skipped: no AD bind password")
		return
	}
	// Every DC is tried, so a broken one is reported even when an earlier
	// one works. An unreachable DC is a warning while another answers
	// (runs fail over to it) and a failure when none does.
	type dcResult struct {
		url, line string
		ok, skip  bool
	}
	var results []dcResult
	var conn Conn
	var answered string
	for i, u := range cfg.Source.URLs {
		start := time.Now()
		c, err := o.Dial(ctx, ldapconn.Options{
			URL: u, StartTLS: ldapconn.Scheme(u) == "ldap", CAFile: cfg.Source.CAFile,
			Timeout: cfg.Sync.NetworkTimeout.Duration, BindDN: cfg.Source.BindDN, Password: pw,
		})
		if err != nil {
			results = append(results, dcResult{url: u, line: fmt.Sprintf("%s: %v", u, err)})
			if ldap.IsErrorWithCode(err, ldap.LDAPResultInvalidCredentials) {
				for _, rest := range cfg.Source.URLs[i+1:] {
					results = append(results, dcResult{url: rest, skip: true,
						line: fmt.Sprintf("%s: not tried (invalid credentials fail on every DC and count toward the account's lockout)", rest)})
				}
				break
			}
			continue
		}
		results = append(results, dcResult{url: u, ok: true,
			line: fmt.Sprintf("%s: %s and bind as %s (%s)", u, tlsMode(u, true), cfg.Source.BindDN, time.Since(start).Round(time.Millisecond))})
		if conn == nil {
			conn, answered = c, u
		} else {
			c.Close()
		}
	}
	for _, r := range results {
		switch {
		case r.ok:
			p.ok("%s", r.line)
		case r.skip:
			p.info("%s", r.line)
		case conn != nil:
			p.warn("%s (runs fail over to %s)", r.line, answered)
		default:
			p.fail("%s", r.line)
		}
	}
	if conn == nil {
		p.fail("no domain controller could be used; skipping the AD search checks")
		return
	}
	defer conn.Close()
	for _, s := range []struct {
		name   string
		search config.Search
		users  bool
	}{{"users base", cfg.Source.Users, true}, {"groups base", cfg.Source.Groups, false}} {
		req := ldap.NewSearchRequest(s.search.Base, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 0, 0, false, s.search.Filter, []string{"objectGUID"},
			[]ldap.Control{ldap.NewControlPaging(1)})
		res, err := conn.Search(req)
		if err != nil && ldap.IsErrorWithCode(err, ldap.LDAPResultSizeLimitExceeded) && res != nil && len(res.Entries) > 0 {
			err = nil // one entry is all this check asks for
		}
		// A groups-only run never searches the AD users base: it fetches
		// members by DN.
		soft := s.users && o.GroupsOnly
		switch {
		case err != nil && soft:
			p.warn("%s %s: paged search failed: %v (groups-only runs don't search it)", s.name, s.search.Base, err)
		case err != nil:
			p.fail("%s %s: paged search failed: %v", s.name, s.search.Base, err)
		case len(res.Entries) == 0 && soft:
			p.warn("%s %s: no entries match %s (groups-only runs don't search it)", s.name, s.search.Base, s.search.Filter)
		case len(res.Entries) == 0:
			p.fail("%s %s: no entries match %s; the guard stops every sync when AD returns none", s.name, s.search.Base, s.search.Filter)
		default:
			p.ok("%s %s: paged search with %s returns entries (answered by %s)", s.name, s.search.Base, s.search.Filter, answered)
		}
	}
}

// checkTarget tests TLS and bind, the containers, and the size limit.
func checkTarget(ctx context.Context, p *printer, cfg *config.Config, o Options) {
	t := cfg.Target
	p.section("Target %s", t.URL)
	pw, err := t.Password()
	if err != nil || pw == "" {
		p.fail("skipped: no target bind password")
		return
	}
	start := time.Now()
	conn, err := o.Dial(ctx, ldapconn.Options{
		URL: t.URL, StartTLS: t.StartTLS, CAFile: t.CAFile,
		Timeout: cfg.Sync.NetworkTimeout.Duration, BindDN: t.BindDN, Password: pw,
	})
	if err != nil {
		p.fail("%v", err)
		return
	}
	defer conn.Close()
	mode := tlsMode(t.URL, t.StartTLS)
	p.ok("%s and bind as %s (%s)", mode, t.BindDN, time.Since(start).Round(time.Millisecond))
	if mode == "no TLS" {
		p.warn("the bind password is sent in the clear; use ldaps:// or start_tls: true")
	}

	exists := func(dn string) (bool, error) {
		_, err := conn.Search(ldap.NewSearchRequest(dn, ldap.ScopeBaseObject, ldap.NeverDerefAliases, 1, 0, false, "(objectClass=*)", []string{"1.1"}, nil))
		switch {
		case err == nil:
			return true, nil
		case ldapconn.IsNoSuchObject(err):
			return false, nil
		}
		return false, err
	}
	// sizeCheck runs an unpaged subtree search with no client size limit,
	// as a full read would hit the server's limit.
	sizeCheck := func(base string) (int, bool, error) {
		res, err := conn.Search(ldap.NewSearchRequest(base, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 0, 0, false, "(objectClass=*)", []string{"1.1"}, nil))
		n := 0
		if res != nil {
			n = len(res.Entries)
		}
		if ldap.IsErrorWithCode(err, ldap.LDAPResultSizeLimitExceeded) {
			return n, true, nil
		}
		return n, false, err
	}
	advice := fmt.Sprintf("bind as the rootdn, or raise the limit for Dolly's DN on the database entry: olcLimits: dn.exact=%q size=unlimited time=unlimited (README \"Permissions\")", t.BindDN)

	// groups_base: read in full by every run.
	switch ok, err := exists(t.GroupsBase); {
	case err != nil:
		p.fail("groups_base %s: %v", t.GroupsBase, err)
	case !ok:
		p.fail("groups_base %s does not exist; create it (Dolly never creates groups_base)", t.GroupsBase)
	default:
		switch n, limited, err := sizeCheck(t.GroupsBase); {
		case err != nil:
			p.fail("groups_base %s: reading it: %v", t.GroupsBase, err)
		case limited:
			p.fail("groups_base %s: the server truncated a full read after %d entries (sizeLimitExceeded); every run would abort", t.GroupsBase, n)
			p.note("%s", advice)
		default:
			p.ok("groups_base %s exists; all %d entries readable in one search", t.GroupsBase, n)
		}
	}

	// users_base: one uid lookup, then the size limit.
	switch ok, err := exists(t.UsersBase); {
	case err != nil:
		p.fail("users_base %s: %v", t.UsersBase, err)
	case !ok:
		p.fail("users_base %s does not exist; create it (Dolly never creates users_base)", t.UsersBase)
	default:
		res, err := conn.Search(ldap.NewSearchRequest(t.UsersBase, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 1, 0, false, "(uid=*)", []string{"uid"}, nil))
		if ldap.IsErrorWithCode(err, ldap.LDAPResultSizeLimitExceeded) && res != nil && len(res.Entries) > 0 {
			err = nil
		}
		switch {
		case err != nil:
			p.fail("users_base %s: uid lookup: %v", t.UsersBase, err)
		case len(res.Entries) == 0:
			hint := ""
			if o.GroupsOnly {
				hint = " (groups-only: every member is skipped until its user entry exists)"
			}
			p.warn("users_base %s: readable, but no entry has a uid yet%s", t.UsersBase, hint)
		default:
			p.ok("users_base %s: readable (uid lookup found %s)", t.UsersBase, res.Entries[0].DN)
		}
		switch n, limited, err := sizeCheck(t.UsersBase); {
		case err != nil:
			p.fail("users_base %s: reading it: %v", t.UsersBase, err)
		case limited && o.GroupsOnly:
			p.warn("users_base %s: the server truncates a full read after %d entries (sizeLimitExceeded); fine for groups-only runs (dolly sync --groups), which never read it in full, but runs that sync users would abort", t.UsersBase, n)
			p.note("%s", advice)
		case limited:
			p.fail("users_base %s: the server truncated a full read after %d entries (sizeLimitExceeded); runs that sync users read it in full and would abort (for a groups-only deployment, run dolly check --groups)", t.UsersBase, n)
			p.note("%s", advice)
		default:
			p.ok("users_base %s: all %d entries readable in one search (no size limit hit)", t.UsersBase, n)
		}
	}

	// state_base: exists, or the first real run can create it.
	switch ok, err := exists(t.StateBase); {
	case err != nil:
		p.fail("state_base %s: %v", t.StateBase, err)
	case ok:
		p.ok("state_base %s exists", t.StateBase)
		checkLock(p, conn, cfg)
	default:
		attr, _, _ := model.RDN(t.StateBase)
		parent := parentDN(t.StateBase)
		switch a := strings.ToLower(attr); {
		case a != "ou" && a != "cn":
			p.fail("state_base %s does not exist, and Dolly creates it only with an ou= or cn= RDN; create it by hand", t.StateBase)
		case parent == "":
			p.fail("state_base %s does not exist and has no parent to create it under", t.StateBase)
		default:
			switch pok, err := exists(parent); {
			case err != nil:
				p.fail("state_base parent %s: %v", parent, err)
			case !pok:
				p.fail("state_base %s does not exist, and neither does its parent %s", t.StateBase, parent)
			default:
				p.ok("state_base %s does not exist yet; the first real run creates it under %s (needs write access there; not tested, check never writes)", t.StateBase, parent)
			}
		}
	}
}

// checkLock warns about a run lock held longer than lock_ttl (by the
// server's createTimestamp) or one no run will judge (clock skew): both
// can stop syncing without a failed run to show for it. A younger lock is
// just reported (a run is probably active).
func checkLock(p *printer, conn Conn, cfg *config.Config) {
	info, err := target.ReadLock(conn, target.LockDN(cfg.Target.StateBase))
	if err != nil {
		p.warn("%v", err)
		return
	}
	if info == nil {
		return
	}
	now := time.Now()
	ttl := cfg.Sync.LockTTL.Duration
	switch err := info.Skew(now); {
	case err != nil:
		p.warn("%v", err)
	case info.Age(now) >= ttl:
		p.warn("a run lock has been held for %s (lock_ttl %s) by %s; if no dolly run is active, run `dolly unlock`",
			info.Age(now), ttl, info.HolderString())
	default:
		p.info("a run lock is held by %s since %s (%s); a run is probably active",
			info.HolderString(), info.Created.UTC().Format(time.RFC3339), info.Age(now))
	}
}

func parentDN(dn string) string {
	d, err := ldap.ParseDN(dn)
	if err != nil || len(d.RDNs) < 2 {
		return ""
	}
	return (&ldap.DN{RDNs: d.RDNs[1:]}).String()
}

// checkSMTP probes the mail server and optionally sends a test mail.
func checkSMTP(ctx context.Context, p *printer, cfg *config.Config, o Options, timeout time.Duration) {
	n := cfg.Notify
	if n.SMTPHost == "" {
		p.section("SMTP")
		if o.SendTestMail {
			p.fail("--send-test-mail: notifications are disabled (notify.smtp_host is empty)")
		} else {
			p.info("notifications disabled (notify.smtp_host is empty)")
		}
		return
	}
	p.section("SMTP %s:%d", n.SMTPHost, n.SMTPPort)
	so := o.SMTP
	so.Timeout = timeout
	steps, err := notify.Probe(ctx, n, so)
	for _, s := range steps {
		if s.Text == "no authentication (username empty)" {
			p.ok("SMTP: no authentication (username empty)")
			continue
		}
		p.ok("%s", s.Text)
	}
	if err != nil {
		p.fail("%v", err)
		return
	}
	if !o.SendTestMail {
		p.info("no mail sent (use --send-test-mail to send one to %s)", strings.Join(n.To, ", "))
		return
	}
	host, _ := os.Hostname()
	msg := notify.Message{
		Subject: "test mail from dolly check on " + host,
		Body: fmt.Sprintf("This is a test mail sent by `dolly check --send-test-mail` on %s with the config %s.\n\n"+
			"Dolly sends at most one mail per run, as configured by notify.on (%s).\n", host, cfg.Path, n.On),
	}
	if err := notify.Send(ctx, n, so, msg); err != nil {
		p.fail("test mail: %v", err)
		return
	}
	p.ok("test mail sent to %s", strings.Join(n.To, ", "))
}
