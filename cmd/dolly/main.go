// Command dolly replicates users and groups one way from Active Directory to
// OpenLDAP. See README.md for the full spec.
package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/dirkpetersen/dolly/internal/config"
	"github.com/dirkpetersen/dolly/internal/fixture"
	"github.com/dirkpetersen/dolly/internal/model"
	"github.com/dirkpetersen/dolly/internal/notify"
	"github.com/dirkpetersen/dolly/internal/planner"
	"github.com/dirkpetersen/dolly/internal/source"
	"github.com/dirkpetersen/dolly/internal/target"
)

// Set by GoReleaser via -ldflags "-X main.version=... -X main.commit=... -X main.date=...".
var version, commit, date string

// Exit codes.
const (
	exitOK    = 0 // success, or the lock is held by another host
	exitError = 1 // any error
	exitGuard = 2 // the mass-deletion guard stopped the run
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

const usage = `Dolly replicates users and groups one way from Active Directory to OpenLDAP.

Usage:
  dolly <command> [flags]

Commands:
  sync       Read AD and the target, then apply the differences
             [--users|--groups] [--dry-run] [--force] [--debug]
  adopt      One-time takeover of an existing tree [--dry-run] [--debug]
  unlock     Show and remove the run lock after a crash [--yes]
  check      Test connectivity, binds, search scopes, size limits, and SMTP
             [--groups] [--send-test-mail]
  install    Install the binary, config, and systemd --user units
             [--users|--groups] [--config FILE]
  uninstall  Remove the units and binary, keep the config
  version    Print the version

Every command that reads the config accepts --config FILE. Without it Dolly
uses ./dolly.yaml, then $XDG_CONFIG_HOME/dolly/dolly.yaml (~/.config/dolly/).
Every command that plans (sync, adopt) accepts --debug, which prints debug
details to stderr, such as each group member skipped for having no entry on
the target.

Exit codes: 0 success (or the lock is held elsewhere), 1 error, 2 guard tripped.
Run "dolly <command> -h" for a command's flags.
`

type command struct {
	name string
	help string
	run  func(args []string, stdout, stderr io.Writer) int
}

func commands() []command {
	return []command{
		{"sync", "Read all in-scope AD users and groups and apply the differences to the target.", runSync},
		{"adopt", "Create ownership records for an existing tree, such as one written by ad2openldap. Run once before the first sync.", runAdopt},
		{"unlock", "Show the run lock and remove it after confirmation.", runUnlock},
		{"check", "Test connectivity, binds, search scopes, the target's containers and size limit, and SMTP.", runCheck},
		{"install", "Install the binary to ~/.local/bin, create the config from the template if missing, and write the systemd --user units.", runInstall},
		{"uninstall", "Remove the systemd --user units and the binary. The config is kept.", runUninstall},
		{"version", "Print the version.", runVersion},
	}
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return exitError
	}
	switch args[0] {
	case "-h", "--help", "-help", "help":
		fmt.Fprint(stdout, usage)
		return exitOK
	case "--version", "-version":
		return runVersion(nil, stdout, stderr)
	}
	for _, c := range commands() {
		if c.name == args[0] {
			return c.run(args[1:], stdout, stderr)
		}
	}
	fmt.Fprintf(stderr, "dolly: unknown command %q\n\n%s", args[0], usage)
	return exitError
}

// newFlags returns a FlagSet whose -h output shows the command's help.
func newFlags(name, help string, stderr io.Writer, extra string) *flag.FlagSet {
	fs := flag.NewFlagSet("dolly "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: dolly %s [flags]\n\n%s\n\nFlags:\n", name, help)
		fs.VisitAll(func(f *flag.Flag) {
			if f.Name == "fixture" {
				return // documented in extra
			}
			fmt.Fprintf(stderr, "  --%-10s %s\n", f.Name, f.Usage)
		})
		fmt.Fprint(stderr, extra)
	}
	return fs
}

// parse parses flags and maps -h to exit 0 and bad flags to exit 1.
func parse(fs *flag.FlagSet, args []string) (int, bool) {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK, false
		}
		return exitError, false
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(fs.Output(), "%s: unexpected argument %q\n", fs.Name(), fs.Arg(0))
		return exitError, false
	}
	return 0, true
}

const fixtureHelp = `
Development only:
  --fixture FILE  Plan from a YAML or JSON snapshot file instead of reading AD
                  and the target. Only valid with --dry-run. See
                  internal/fixture for the format.
`

func runSync(args []string, stdout, stderr io.Writer) int {
	fs := newFlags("sync", "Read all in-scope AD users and groups and apply the differences to the target.\nWithout --users or --groups (or with both), both are synced.", stderr, fixtureHelp)
	cfgPath := fs.String("config", "", "config file")
	users := fs.Bool("users", false, "sync users only")
	groups := fs.Bool("groups", false, "sync groups only")
	dryRun := fs.Bool("dry-run", false, "print the plan without writing or taking the lock")
	force := fs.Bool("force", false, "apply even if the mass-deletion guard trips")
	dbg := debugFlag(fs)
	fix := fs.String("fixture", "", "plan from a snapshot file (development only)")
	if code, ok := parse(fs, args); !ok {
		return code
	}
	// --users and --groups select phases; both together, like neither, mean both.
	opt := planner.Options{Users: *users || !*groups, Groups: *groups || !*users}
	return plan("sync", *cfgPath, *fix, *dryRun, *force, *dbg, opt, stdout, stderr)
}

func runAdopt(args []string, stdout, stderr io.Writer) int {
	fs := newFlags("adopt", "Create ownership records for an existing tree, such as one written by ad2openldap.\nRun it once before the first sync, and check --dry-run first.", stderr, fixtureHelp)
	cfgPath := fs.String("config", "", "config file")
	dryRun := fs.Bool("dry-run", false, "print the records adopt would create without writing")
	dbg := debugFlag(fs)
	fix := fs.String("fixture", "", "plan from a snapshot file (development only)")
	if code, ok := parse(fs, args); !ok {
		return code
	}
	return plan("adopt", *cfgPath, *fix, *dryRun, false, *dbg, planner.Options{Adopt: true}, stdout, stderr)
}

// debugFlag adds --debug, which every command that plans accepts.
func debugFlag(fs *flag.FlagSet) *bool {
	return fs.Bool("debug", false, "print debug details to stderr, such as each group member skipped for having no entry on the target")
}

// plan loads the config, then either prints the plan (--dry-run) or runs
// for real (see realRun). With showDebug, debug details go to stderr.
func plan(cmd, cfgPath, fix string, dryRun, force, showDebug bool, opt planner.Options, stdout, stderr io.Writer) int {
	path, err := config.Find(cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "dolly %s: %v\n", cmd, err)
		return exitError
	}
	cfg, err := config.Load(path)
	if err != nil {
		fmt.Fprintf(stderr, "dolly %s: %v\n", cmd, err)
		return exitError
	}
	if fix != "" && !dryRun {
		fmt.Fprintf(stderr, "dolly %s: --fixture is only valid with --dry-run\n", cmd)
		return exitError
	}
	if !dryRun {
		return realRun(cmd, cfg, force, showDebug, opt, stdout, stderr)
	}
	var snaps *fixture.Snapshots
	if fix != "" {
		snaps, err = fixture.Load(fix, cfg, opt.Users || opt.Adopt)
	} else {
		ctx, cleanup := runContext(cfg)
		snaps, err = readLive(ctx, cfg, opt, stderr)
		cleanup()
	}
	if err != nil {
		fmt.Fprintf(stderr, "dolly %s: %v\n", cmd, err)
		return exitError
	}
	opt.Now = snaps.Now
	if opt.Now.IsZero() {
		opt.Now = time.Now()
	}
	p, err := planner.Build(snaps.AD, snaps.Target, snaps.Records, cfg, opt)
	if err != nil {
		fmt.Fprintf(stderr, "dolly %s: %v\n", cmd, err)
		return exitError
	}
	if showDebug {
		p.PrintDebug(stderr)
	}
	p.Print(stdout)
	if p.Guard.Tripped {
		if force {
			fmt.Fprintf(stdout, "\n--force: a real run would apply this plan despite the guard.\n")
			return exitOK
		}
		// spec: a dry run exits 2 when a real run would have been stopped by
		// the guard, so scripts can detect it before running for real.
		fmt.Fprintf(stderr, "dolly %s: mass-deletion guard tripped; a real run would change nothing (use --force to override)\n", cmd)
		return exitGuard
	}
	return exitOK
}

// runContext returns the run's context: cancelled on SIGTERM or SIGINT and
// after run_timeout. A cancelled run stops between operations and still
// releases its lock (with a fresh context).
func runContext(cfg *config.Config) (context.Context, func()) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	ctx, cancel := context.WithTimeout(ctx, cfg.Sync.RunTimeout.Duration)
	return ctx, func() { cancel(); stop() }
}

// realRun is a real sync or adopt: create state_base if missing, take the
// run lock, read AD and the target, plan, check the guard, apply, write
// cn=status, and release the lock.
//
// spec: the lock is taken before reading, so the plan is made from data no
// other Dolly run changes meanwhile. If another run holds the lock, exit 0
// quietly (one line on stderr, no status, no mail). Every outcome after the
// lock (success, read error, guard, per-entry errors, a stop by signal or
// run_timeout) is recorded in cn=status, and so is a lock Dolly refuses to
// judge because of clock skew (exit 1).
func realRun(cmd string, cfg *config.Config, force, showDebug bool, opt planner.Options, stdout, stderr io.Writer) int {
	ctx, cleanup := runContext(cfg)
	defer cleanup()
	start := time.Now()
	// The run's output (stdout and stderr, as written) becomes the body of
	// the run's single notification.
	capture := &lockedBuffer{}
	stdout, stderr = io.MultiWriter(stdout, capture), io.MultiWriter(stderr, capture)
	sb := cfg.Target.StateBase
	conn, err := dialWriter(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "dolly %s: %v\n", cmd, err)
		return exitError
	}
	defer conn.Close()
	created, err := target.EnsureStateBase(ctx, conn, sb)
	if err != nil {
		fmt.Fprintf(stderr, "dolly %s: target %s: %v\n", cmd, cfg.Target.URL, err)
		return exitError
	}
	if created {
		fmt.Fprintf(stderr, "dolly: created state_base %s\n", sb)
	}
	// The run context may be cancelled by now (signal, run_timeout), so the
	// release and the status write use fresh short-timeout contexts.
	fresh := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.Background(), cfg.Sync.NetworkTimeout.Duration)
	}

	// status records the outcome in cn=status and, through the Notes hook,
	// sends the run's single notification (decided from the notes before
	// this run). A broken stale lock and a tripped guard are mailed as
	// failures are. A mail failure is logged and recorded as notify-error;
	// it never changes the exit code.
	var lock *target.Lock
	var p *planner.Plan
	var res *target.Result
	status := func(failure, result string) {
		sctx, cancel := fresh()
		defer cancel()
		end := time.Now()
		notes := func(before []string) map[string]string {
			r := notify.Report{
				Command: cmd, ConfigPath: cfg.Path, Start: start, End: end, Failure: failure,
				Changed: res != nil && res.Applied > 0, Plan: p, Output: capture.String(),
			}
			if lock != nil {
				r.BrokeLock = lock.Broke
			}
			mctx, mcancel := context.WithTimeout(context.Background(), 2*cfg.Sync.NetworkTimeout.Duration)
			defer mcancel()
			o := notifyOptions
			o.Timeout = cfg.Sync.NetworkTimeout.Duration
			return notify.Notes(mctx, cfg.Notify, o, before, r, stderr)
		}
		if _, _, err := target.WriteStatus(sctx, conn, sb, target.RunStatus{End: end, Failure: failure, Result: result, Notes: notes}); err != nil {
			fmt.Fprintf(stderr, "dolly %s: warning: %v\n", cmd, err)
		}
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "dolly %s: %v\n", cmd, err)
		status(err.Error(), "")
		return exitError
	}

	lock, held, err := target.AcquireLock(ctx, conn, target.LockOptions{StateBase: sb, TTL: cfg.Sync.LockTTL.Duration, Command: cmd, Log: stderr})
	if err != nil {
		var skew *target.ClockSkewError
		if errors.As(err, &skew) {
			// spec: a lock Dolly refuses to judge (clock skew) would stop
			// every run until someone acts, so it is recorded in cn=status
			// as a failure and mailed like one, although this run holds no
			// lock.
			return fail(fmt.Errorf("target %s: %w", cfg.Target.URL, err))
		}
		fmt.Fprintf(stderr, "dolly %s: target %s: %v\n", cmd, cfg.Target.URL, err)
		return exitError
	}
	if held != nil {
		fmt.Fprintf(stderr, "dolly %s: another run holds the lock (%s, created %s, %s ago); exiting\n",
			cmd, held.HolderString(), held.Created.UTC().Format(time.RFC3339), held.Age(time.Now()))
		return exitOK
	}
	defer func() {
		rctx, cancel := fresh()
		defer cancel()
		if err := lock.Release(rctx); err != nil {
			fmt.Fprintf(stderr, "dolly %s: warning: %v\n", cmd, err)
		}
	}()

	opt.Now = time.Now()
	snaps, err := readSnapshots(ctx, cfg, opt, stderr)
	if err != nil {
		return fail(err)
	}
	p, err = planner.Build(snaps.AD, snaps.Target, snaps.Records, cfg, opt)
	if err != nil {
		p = nil
		return fail(err)
	}
	if showDebug {
		p.PrintDebug(stderr)
	}
	p.Real = true
	p.Print(stdout)
	if p.Guard.Tripped {
		if !force {
			msg := "mass-deletion guard tripped, nothing was changed (use --force to override): " + strings.Join(p.Guard.Reasons, "; ")
			fmt.Fprintf(stderr, "dolly %s: %s\n", cmd, msg)
			status(msg, "")
			return exitGuard
		}
		fmt.Fprintf(stdout, "\n--force: applying the plan despite the guard.\n")
	}
	res = target.Apply(ctx, conn, p, stderr, showDebug)
	res.Print(stdout)
	summary := fmt.Sprintf("%d operations: %d applied, %d already in place, %d skipped, %d failed, %d not run",
		len(p.Ops), res.Applied, res.AlreadyDone, res.Skipped, res.Failed, res.NotRun)
	if !res.OK() {
		status("run incomplete: "+summary, summary)
		return exitError
	}
	status("", summary)
	return exitOK
}

// notifyOptions are the notification options other than the config
// (Timeout is set from network_timeout); tests set RootCAs.
var notifyOptions notify.Options

// lockedBuffer is a bytes.Buffer safe for concurrent writes.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// writerConn is the target connection of a real run.
type writerConn interface {
	target.Conn
	Close() error
}

// readSnapshots reads AD and the target for a real run; tests replace it.
var readSnapshots = readLive

// dialWriter opens the writer connection; tests replace it.
var dialWriter = func(cfg *config.Config) (writerConn, error) { return target.DialWriter(cfg) }

// readLive reads AD and the target for a plan, printing progress to
// stderr. It is read-only: it takes no lock and writes nothing (a real run
// takes the lock before calling it).
//
// spec: a run that syncs users (and adopt) reads the AD users base and the
// target's users_base in full. A groups-only run reads neither: AD members
// are fetched by DN, and the target is asked only about the uids those
// members need (batched lookups), so a huge or size-limited users_base
// never has to be read.
func readLive(ctx context.Context, cfg *config.Config, opt planner.Options, stderr io.Writer) (*fixture.Snapshots, error) {
	start := time.Now()
	step := time.Now()
	progress := func(format string, a ...any) {
		now := time.Now()
		fmt.Fprintf(stderr, "dolly: "+format+" (%s)\n", append(a, now.Sub(step).Round(time.Millisecond))...)
		step = now
	}
	readUsers := opt.Users || opt.Adopt

	ad, err := source.DialAD(ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer ad.Close()
	progress("connected to AD %s as %s", ad.URL, cfg.Source.BindDN)
	snap, err := source.Read(ctx, ad, readUsers)
	if err != nil {
		return nil, err
	}
	fetched := 0
	for _, o := range snap.Objects {
		if !o.InScope {
			fetched++
		}
	}
	if readUsers {
		progress("read AD: %d users and %d groups in scope, %d objects fetched by DN, %d unresolved, %d filtered",
			snap.Count(model.KindUser), snap.Count(model.KindGroup), fetched, len(snap.Unresolved), len(snap.Filtered))
	} else {
		progress("read AD: %d groups in scope (users base not read), %d members fetched by DN, %d unresolved, %d filtered",
			snap.Count(model.KindGroup), fetched, len(snap.Unresolved), len(snap.Filtered))
	}

	tr, err := target.Dial(ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer tr.Close()
	progress("connected to target %s as %s", tr.URL, cfg.Target.BindDN)
	tgt, recs, err := tr.Read(ctx, readUsers)
	if err != nil {
		return nil, err
	}
	users := fmt.Sprint(len(tgt.Users), " user entries")
	if !readUsers {
		users = "users_base not read"
	}
	progress("read target: %d group entries, %s, %d user and %d group ownership records",
		len(tgt.Groups), users, len(recs.Users), len(recs.Groups))

	// spec: a member is added only if its entry exists on the target, so a
	// groups-only run always looks up the candidates (by uid only: no
	// uidNumber is needed on either side).
	if opt.Groups && !opt.Adopt && !readUsers {
		cands, err := planner.MemberCandidates(snap, tgt, recs, cfg)
		if err != nil {
			return nil, err
		}
		uids := make([]string, len(cands))
		for i, c := range cands {
			uids[i] = c.UID
		}
		set, err := tr.LookupUsers(ctx, uids)
		if err != nil {
			return nil, err
		}
		tgt.ExistingUsers = set
		progress("looked up %d member uids under %s: %d found", len(uids), cfg.Target.UsersBase, len(set.UIDs))
	}
	fmt.Fprintf(stderr, "dolly: read complete in %s\n", time.Since(start).Round(time.Millisecond))
	return &fixture.Snapshots{AD: snap, Target: tgt, Records: recs}, nil
}

// stdin and stdinIsTTY are the confirmation input of dolly unlock; tests
// replace them.
var (
	stdin      io.Reader = os.Stdin
	stdinIsTTY           = func() bool { return isTerminal(os.Stdin.Fd()) }
)

func runUnlock(args []string, stdout, stderr io.Writer) int {
	fs := newFlags("unlock", "Show the run lock (holder and age by the server's createTimestamp) and remove it after confirmation.\nUse it after a crash, when no dolly run is active. Without --yes, stdin must be a terminal.", stderr, "")
	cfgPath := fs.String("config", "", "config file")
	yes := fs.Bool("yes", false, "remove the lock without asking")
	if code, ok := parse(fs, args); !ok {
		return code
	}
	path, err := config.Find(*cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "dolly unlock: %v\n", err)
		return exitError
	}
	cfg, err := config.Load(path)
	if err != nil {
		fmt.Fprintf(stderr, "dolly unlock: %v\n", err)
		return exitError
	}
	conn, err := dialWriter(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "dolly unlock: %v\n", err)
		return exitError
	}
	defer conn.Close()
	dn := target.LockDN(cfg.Target.StateBase)
	info, err := target.ReadLock(conn, dn)
	if err != nil {
		fmt.Fprintf(stderr, "dolly unlock: %v\n", err)
		return exitError
	}
	if info == nil {
		fmt.Fprintf(stdout, "No run lock is held (%s does not exist).\n", dn)
		return exitOK
	}
	now := time.Now()
	fmt.Fprintf(stdout, "Run lock %s\n  holder:  %s\n  created: %s (%s ago, by the server's createTimestamp)\n",
		info.DN, info.HolderString(), info.Created.UTC().Format(time.RFC3339), info.Age(now))
	if started, ok := info.Started(); ok {
		fmt.Fprintf(stdout, "  started: %s (by the holder's clock)\n", started.UTC().Format(time.RFC3339))
	}
	ttl := cfg.Sync.LockTTL.Duration
	if stale, err := info.Stale(now, ttl); err != nil {
		// A lock with a timestamp in the future is never judged stale:
		// every run fails with this error until the lock is removed.
		fmt.Fprintf(stdout, "  CLOCK SKEW: %v\n", err)
	} else if stale {
		fmt.Fprintf(stdout, "  stale?:  createTimestamp and started= are older than lock_ttl (%s); the next run breaks it\n", ttl)
	} else if now.Sub(info.Created) >= ttl {
		fmt.Fprintf(stdout, "  stale?:  createTimestamp is older than lock_ttl (%s), but the holder's started= time isn't yet; the next run breaks it once both are\n", ttl)
	}
	if !*yes {
		if !stdinIsTTY() {
			fmt.Fprintf(stderr, "dolly unlock: stdin is not a terminal; refusing to remove the lock without confirmation (use --yes)\n")
			return exitError
		}
		fmt.Fprint(stdout, "Remove it? Only do this if no dolly run is active. [y/N] ")
		answer, _ := bufio.NewReader(stdin).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(answer)); a != "y" && a != "yes" {
			fmt.Fprintln(stdout, "Lock left in place.")
			return exitOK
		}
	}
	if err := target.RemoveLock(conn, dn); err != nil {
		fmt.Fprintf(stderr, "dolly unlock: %v\n", err)
		return exitError
	}
	fmt.Fprintf(stdout, "Removed %s.\n", dn)
	return exitOK
}

func runVersion(args []string, stdout, stderr io.Writer) int {
	fs := newFlags("version", "Print the version.", stderr, "")
	if code, ok := parse(fs, args); !ok {
		return code
	}
	fmt.Fprintln(stdout, versionString())
	return exitOK
}

func versionString() string {
	v, c, d := version, commit, date
	if v == "" {
		v = "dev"
		if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
			v = bi.Main.Version // go install ...@version
		}
	}
	var parts []string
	if c != "" {
		parts = append(parts, "commit "+c)
		if d != "" {
			parts = append(parts, "built "+d)
		}
	}
	// The Go toolchain the binary was built with (its stdlib, crypto/tls
	// included).
	parts = append(parts, runtime.Version())
	return "dolly " + v + " (" + strings.Join(parts, ", ") + ")"
}
