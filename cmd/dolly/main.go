// Command dolly replicates users and groups one way from Active Directory to
// OpenLDAP. See README.md for the full spec.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"time"

	"github.com/dirkpetersen/dolly/internal/config"
	"github.com/dirkpetersen/dolly/internal/fixture"
	"github.com/dirkpetersen/dolly/internal/planner"
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
             [--users|--groups] [--dry-run] [--force]
  adopt      One-time takeover of an existing tree [--dry-run]
  unlock     Show and remove the run lock after a crash [--yes]
  check      Test connectivity, binds, search scopes, size limits, and SMTP
  install    Install the binary, config, and systemd --user units
  uninstall  Remove the units and binary, keep the config
  version    Print the version

Every command that reads the config accepts --config FILE. Without it Dolly
uses ./dolly.yaml, then $XDG_CONFIG_HOME/dolly/dolly.yaml (~/.config/dolly/).

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
		{"unlock", "Show the run lock and remove it after confirmation.", notImplemented("unlock", "yes")},
		{"check", "Test connectivity, binds, search scopes, the target's containers and size limit, and SMTP.", notImplemented("check")},
		{"install", "Install the binary to ~/.local/bin, create the config from the template if missing, and write the systemd --user units.", notImplemented("install")},
		{"uninstall", "Remove the systemd --user units and the binary. The config is kept.", notImplemented("uninstall")},
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
Development only (until the AD and target readers exist):
  --fixture FILE  Plan from a YAML or JSON snapshot file instead of reading AD
                  and the target. Only valid with --dry-run. In this build,
                  --dry-run requires it. See internal/fixture for the format.
`

func runSync(args []string, stdout, stderr io.Writer) int {
	fs := newFlags("sync", "Read all in-scope AD users and groups and apply the differences to the target.\nWithout --users or --groups (or with both), both are synced.", stderr, fixtureHelp)
	cfgPath := fs.String("config", "", "config file")
	users := fs.Bool("users", false, "sync users only")
	groups := fs.Bool("groups", false, "sync groups only")
	dryRun := fs.Bool("dry-run", false, "print the plan without writing or taking the lock")
	force := fs.Bool("force", false, "apply even if the mass-deletion guard trips")
	fix := fs.String("fixture", "", "plan from a snapshot file (development only)")
	if code, ok := parse(fs, args); !ok {
		return code
	}
	// --users and --groups select phases; both together, like neither, mean both.
	opt := planner.Options{Users: *users || !*groups, Groups: *groups || !*users}
	return plan("sync", *cfgPath, *fix, *dryRun, *force, opt, stdout, stderr)
}

func runAdopt(args []string, stdout, stderr io.Writer) int {
	fs := newFlags("adopt", "Create ownership records for an existing tree, such as one written by ad2openldap.\nRun it once before the first sync, and check --dry-run first.", stderr, fixtureHelp)
	cfgPath := fs.String("config", "", "config file")
	dryRun := fs.Bool("dry-run", false, "print the records adopt would create without writing")
	fix := fs.String("fixture", "", "plan from a snapshot file (development only)")
	if code, ok := parse(fs, args); !ok {
		return code
	}
	return plan("adopt", *cfgPath, *fix, *dryRun, false, planner.Options{Adopt: true}, stdout, stderr)
}

// plan loads the config and snapshots, builds the plan, and prints it.
func plan(cmd, cfgPath, fix string, dryRun, force bool, opt planner.Options, stdout, stderr io.Writer) int {
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
	if !dryRun {
		fmt.Fprintf(stderr, "dolly %s: not implemented yet: this build can only plan; use --dry-run\n", cmd)
		return exitError
	}
	if fix == "" {
		fmt.Fprintf(stderr, "dolly %s: not implemented yet: reading AD and the target; use --dry-run --fixture FILE\n", cmd)
		return exitError
	}
	snaps, err := fixture.Load(fix, cfg)
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

// notImplemented returns a command that accepts its documented flags and
// reports that it isn't available yet.
func notImplemented(name string, boolFlags ...string) func([]string, io.Writer, io.Writer) int {
	return func(args []string, stdout, stderr io.Writer) int {
		var help string
		for _, c := range commands() {
			if c.name == name {
				help = c.help
			}
		}
		fs := newFlags(name, help, stderr, "")
		if name == "unlock" || name == "check" {
			fs.String("config", "", "config file")
		}
		for _, b := range boolFlags {
			fs.Bool(b, false, "skip the confirmation prompt")
		}
		if code, ok := parse(fs, args); !ok {
			return code
		}
		fmt.Fprintf(stderr, "dolly %s: not implemented yet\n", name)
		return exitError
	}
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
	s := "dolly " + v
	if c != "" {
		s += " (commit " + c
		if d != "" {
			s += ", built " + d
		}
		s += ")"
	}
	return s
}
