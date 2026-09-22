// Package install implements `dolly install` and `dolly uninstall`: the
// binary in ~/.local/bin, the config from the embedded template, and the
// systemd --user units, all without root and without editing shell startup
// files. Everything environment-dependent (paths, GOOS, the uid, systemctl)
// comes in through Env and Systemctl, so tests run against temp dirs and a
// fake systemctl.
package install

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/dirkpetersen/dolly/internal/config"
)

// Systemctl runs `systemctl --user <args>`.
type Systemctl interface {
	// Run runs systemctl --user with args. env is the complete environment
	// for the command; nil inherits Dolly's. It returns the combined output.
	Run(env []string, args ...string) (string, error)
}

// ExecSystemctl runs the real systemctl.
type ExecSystemctl struct{}

// Run implements Systemctl.
func (ExecSystemctl) Run(env []string, args ...string) (string, error) {
	cmd := exec.Command("systemctl", append([]string{"--user"}, args...)...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// Env is the environment install works in.
type Env struct {
	GOOS       string   // runtime.GOOS
	Home       string   // the home directory
	ConfigHome string   // $XDG_CONFIG_HOME as set ("" or relative: ~/.config)
	Path       string   // $PATH, for the "not on PATH" warning
	RuntimeDir string   // $XDG_RUNTIME_DIR as set
	Environ    []string // os.Environ(), the base of systemctl's environment
	UID        int
	User       string // the user name, for the enable-linger hint
	// RunUser is the parent of the per-user runtime directories
	// (/run/user); /run/user/<uid> exists while the user has a session or
	// linger.
	RunUser string
	// Systemd reports whether the host runs systemd (on Linux: whether
	// /run/systemd/system exists). Never consulted on other systems.
	Systemd func() bool
	// Executable is the running binary (os.Executable), the one installed.
	Executable string
}

// SystemdBooted is the default Env.Systemd: sd_booted(3)'s test.
func SystemdBooted() bool {
	st, err := os.Stat("/run/systemd/system")
	return err == nil && st.IsDir()
}

// Options are the install flags.
type Options struct {
	// Users and Groups select what the timer syncs, as for dolly sync:
	// both or neither is a full sync.
	Users, Groups bool
	// ConfigPath is --config made absolute, or "": the unit then relies on
	// the default lookup ($XDG_CONFIG_HOME/dolly/dolly.yaml), since the
	// service's working directory isn't where dolly install ran.
	ConfigPath string
	Template   []byte // the embedded dolly.yaml.template
	Systemctl  Systemctl
	Out        io.Writer
}

// Paths are the files install manages.
type Paths struct {
	BinDir, Binary string
	Config         string
	UnitDir        string
	Service, Timer string
}

// PathsFor returns the XDG paths for env and the config path option.
func PathsFor(env Env, configPath string) Paths {
	ch := env.ConfigHome
	if ch == "" || !filepath.IsAbs(ch) {
		ch = filepath.Join(env.Home, ".config") // a relative XDG_CONFIG_HOME is invalid
	}
	p := Paths{
		BinDir:  filepath.Join(env.Home, ".local", "bin"),
		Config:  filepath.Join(ch, "dolly", "dolly.yaml"),
		UnitDir: filepath.Join(ch, "systemd", "user"),
	}
	if configPath != "" {
		p.Config = configPath
	}
	p.Binary = filepath.Join(p.BinDir, "dolly")
	p.Service = filepath.Join(p.UnitDir, "dolly.service")
	p.Timer = filepath.Join(p.UnitDir, "dolly.timer")
	return p
}

// ExecStart returns the service's command line: dolly sync with --users or
// --groups (neither for a full sync) and --config if given, running
// %h/.local/bin/dolly.
func ExecStart(users, groups bool, configPath string) string {
	return execStartFor("%h/.local/bin/dolly", users, groups, configPath)
}

// execStartFor is ExecStart with bin (already escaped for a unit file) as
// the binary.
func execStartFor(bin string, users, groups bool, configPath string) string {
	s := bin + " sync"
	switch {
	case users && !groups:
		s += " --users"
	case groups && !users:
		s += " --groups"
	}
	if configPath != "" {
		s += " --config " + unitQuote(configPath)
	}
	return s
}

// unitQuote escapes a path for an ExecStart argument: % and $ are
// specifiers and variables in unit files, and a path with spaces or
// quotes is double-quoted.
func unitQuote(s string) string {
	s = strings.NewReplacer("%", "%%", "$", "$$").Replace(s)
	if !strings.ContainsAny(s, " \t\"'\\;") {
		return s
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// shellQuote quotes s for a POSIX shell command line the user copies from
// the "Next steps": unchanged if it holds only safe characters, otherwise
// single-quoted, each embedded single quote closed, escaped, and reopened.
func shellQuote(s string) string {
	safe := func(r rune) bool {
		return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("/._-+=:,@%", r)
	}
	if s != "" && !strings.ContainsFunc(s, func(r rune) bool { return !safe(r) }) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ServiceStartTimeout is dolly.service's TimeoutStartSec: a oneshot
// service that never finishes would otherwise block the timer forever.
// It is fixed rather than derived from the config's run_timeout, so the
// unit doesn't depend on a config that may change (or not load yet) after
// install: 1h is above the default run_timeout (45m) and equals the default
// lock_ttl, and install warns when the config's run_timeout isn't below it.
// systemd stops the service with SIGTERM, which Dolly handles like
// run_timeout (it stops, records cn=status, and releases the lock).
const ServiceStartTimeout = time.Hour

// Units returns the content of dolly.service and dolly.timer, exactly as
// README "Running it on a schedule" shows them (with execStart after
// ExecStart=).
func Units(execStart string) (service, timer string) {
	service = `[Unit]
Description=Dolly AD to LDAP sync
After=network-online.target

[Service]
Type=oneshot
TimeoutStartSec=1h
ExecStart=` + execStart + "\n"
	timer = `[Unit]
Description=Run Dolly every 15 minutes

[Timer]
OnCalendar=*:0/15
RandomizedDelaySec=60
Persistent=true

[Install]
WantedBy=timers.target
`
	return service, timer
}

// hasSystemd reports whether units are written: Linux with systemd.
func hasSystemd(env Env) bool {
	if env.GOOS != "linux" {
		return false
	}
	if env.Systemd == nil {
		return SystemdBooted()
	}
	return env.Systemd()
}

// NoManagerError is returned when systemctl --user can't reach a user
// manager: XDG_RUNTIME_DIR is unset (or belongs to another user) and
// /run/user/<uid> doesn't exist.
type NoManagerError struct {
	Dir, User string
	// Foreign is an XDG_RUNTIME_DIR that was set but isn't this user's
	// (inherited after sudo -u), or "".
	Foreign string
}

func (e *NoManagerError) Error() string {
	why := "XDG_RUNTIME_DIR is not set"
	if e.Foreign != "" {
		why = fmt.Sprintf("XDG_RUNTIME_DIR=%s is not this user's runtime directory (inherited from another user, e.g. after sudo -u)", e.Foreign)
	}
	return fmt.Sprintf("%s and %s does not exist: %s has no login session and no linger, so systemctl --user has no user manager to talk to. Run `loginctl enable-linger %s` (an admin may have to: sudo loginctl enable-linger %s), then run dolly install again",
		why, e.Dir, e.User, e.User, e.User)
}

// SystemctlEnv returns the environment for Dolly's own systemctl --user
// calls. With XDG_RUNTIME_DIR set to this user's /run/user/<uid> it is nil
// (inherit). Without it (typical after su - or sudo -iu), or with another
// user's XDG_RUNTIME_DIR (inherited after sudo -u, which would reach the
// wrong manager or none), if /run/user/<uid> exists, XDG_RUNTIME_DIR and
// DBUS_SESSION_BUS_ADDRESS point there; otherwise the user has no session
// and no linger, which is a NoManagerError. Only systemctl's environment
// changes: Dolly never edits shell startup files.
func SystemctlEnv(env Env) ([]string, error) {
	dir := filepath.Join(env.RunUser, strconv.Itoa(env.UID))
	if env.RuntimeDir != "" && filepath.Clean(env.RuntimeDir) == dir {
		return nil, nil
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return nil, &NoManagerError{Dir: dir, User: env.User, Foreign: env.RuntimeDir}
	}
	var out []string
	for _, kv := range env.Environ {
		if !strings.HasPrefix(kv, "XDG_RUNTIME_DIR=") && !strings.HasPrefix(kv, "DBUS_SESSION_BUS_ADDRESS=") {
			out = append(out, kv)
		}
	}
	return append(out, "XDG_RUNTIME_DIR="+dir, "DBUS_SESSION_BUS_ADDRESS=unix:path="+filepath.Join(dir, "bus")), nil
}

// Install installs the binary, creates the config if missing, writes the
// units if they changed, and reloads the user manager. It is idempotent.
// The error is the first step that failed; later steps still run where
// they don't depend on it.
func Install(env Env, o Options) error {
	w := o.Out
	p := PathsFor(env, o.ConfigPath)
	var errs []error
	systemd := hasSystemd(env)
	var mv managerView
	if systemd {
		var err error
		var nm *NoManagerError
		if mv, err = queryManager(env, o.Systemctl); err != nil && !errors.As(err, &nm) {
			fmt.Fprintf(w, "  note: couldn't read the systemd user manager's environment (%v); assuming it uses HOME=%s\n", err, env.Home)
		}
	}

	// Binary.
	switch msg, err := installBinary(env.Executable, p); {
	case err != nil:
		fmt.Fprintf(w, "✗ binary: %v\n", err)
		errs = append(errs, err)
	default:
		fmt.Fprintf(w, "✓ binary: %s\n", msg)
	}
	if !onPath(env.Path, p.BinDir) {
		fmt.Fprintf(w, "  warning: %s is not on PATH; run it by its full path or add the directory to PATH yourself (Dolly never edits shell startup files). The timer doesn't need PATH.\n", p.BinDir)
	}

	// Config: created from the template, never overwritten.
	switch created, err := createConfig(p.Config, o.Template); {
	case err != nil:
		fmt.Fprintf(w, "✗ config: %v\n", err)
		errs = append(errs, err)
	case created:
		fmt.Fprintf(w, "✓ config: created %s from the template (mode 0600)\n", p.Config)
	default:
		fmt.Fprintf(w, "✓ config: %s exists, left unchanged (dolly install never overwrites a config)\n", p.Config)
	}
	// The unit's TimeoutStartSec must leave the run time to stop on its own.
	if cfg, err := config.Load(p.Config); err == nil && cfg.Sync.RunTimeout.Duration >= ServiceStartTimeout {
		fmt.Fprintf(w, "  warning: sync.run_timeout (%s) is not below the service's TimeoutStartSec (%s): systemd stops a long run (SIGTERM) before run_timeout does; lower run_timeout, or raise TimeoutStartSec with systemctl --user edit dolly.service\n",
			cfg.Sync.RunTimeout.Duration, ServiceStartTimeout)
	}

	// Units. They always live under the shell's $HOME ("$HOME always
	// wins"); a manager with another HOME gets them linked into its unit
	// directory by systemctl --user link, and absolute paths in ExecStart,
	// because it would expand %h to its own HOME.
	execStart := ExecStart(o.Users, o.Groups, o.ConfigPath)
	splitHome := mv.known() && !sameDir(mv.Home, env.Home)
	link := mv.known() && !sameDir(mv.unitDir(), p.UnitDir)
	if mv.known() {
		bin := "%h/.local/bin/dolly"
		if splitHome {
			bin = unitQuote(p.Binary)
		}
		cfg := o.ConfigPath
		if cfg == "" && !sameConfig(mv.defaultConfig(), p.Config) {
			cfg = p.Config // the service's own default lookup would miss it
		}
		execStart = execStartFor(bin, o.Users, o.Groups, cfg)
	}
	if systemd && splitHome {
		fmt.Fprintf(w, "  note: the systemd user manager uses HOME=%s, the shell uses HOME=%s; the units stay in %s and are linked into %s with systemctl --user link; ExecStart uses absolute paths\n",
			mv.Home, env.Home, p.UnitDir, mv.unitDir())
	} else if systemd && link {
		fmt.Fprintf(w, "  note: the systemd user manager reads units from %s, not %s; the units are linked there with systemctl --user link\n",
			mv.unitDir(), p.UnitDir)
	}
	if !systemd {
		why := "not Linux (" + env.GOOS + ")"
		if env.GOOS == "linux" {
			why = "this host doesn't run systemd"
		}
		fmt.Fprintf(w, "- units: skipped, %s; schedule `%s` with cron or launchd instead\n",
			why, strings.Replace(execStart, "%h", env.Home, 1))
	} else {
		service, timer := Units(execStart)
		changed := false
		for _, u := range []struct{ path, content string }{{p.Service, service}, {p.Timer, timer}} {
			c, err := writeIfChanged(u.path, []byte(u.content), 0o644)
			switch {
			case err != nil:
				fmt.Fprintf(w, "✗ unit: %v\n", err)
				errs = append(errs, err)
			case c:
				changed = true
				fmt.Fprintf(w, "✓ unit: wrote %s\n", u.path)
			default:
				fmt.Fprintf(w, "✓ unit: %s unchanged\n", u.path)
			}
		}
		fmt.Fprintf(w, "  ExecStart=%s\n", execStart)
		senv, err := SystemctlEnv(env)
		if err == nil && link {
			for _, u := range []string{p.Service, p.Timer} {
				errs = append(errs, linkUnit(w, o.Systemctl, senv, mv.unitDir(), u))
			}
		}
		// Reload on every install, not only after a change: a previous
		// install may have written the units but failed to reload.
		if err != nil {
			fmt.Fprintf(w, "✗ systemctl --user daemon-reload: %v\n", err)
			errs = append(errs, err)
		} else if out, err := o.Systemctl.Run(senv, "daemon-reload"); err != nil {
			err = fmt.Errorf("systemctl --user daemon-reload: %v%s", err, outputSuffix(out))
			fmt.Fprintf(w, "✗ %v\n", err)
			errs = append(errs, err)
		} else {
			what := "units unchanged"
			if changed {
				what = "units changed"
			}
			fmt.Fprintf(w, "✓ systemctl --user daemon-reload (%s)\n", what)
		}
	}

	// Next steps. The timer is never enabled automatically.
	dolly := "dolly"
	if !onPath(env.Path, p.BinDir) {
		dolly = shellQuote(p.Binary)
	}
	cfgFlag := ""
	if o.ConfigPath != "" {
		cfgFlag = " --config " + shellQuote(o.ConfigPath)
	}
	scope := ""
	switch {
	case o.Users && !o.Groups:
		scope = " --users"
	case o.Groups && !o.Users:
		scope = " --groups"
	}
	fmt.Fprintf(w, "\nNext steps:\n")
	fmt.Fprintf(w, "  1. Edit the config:     $EDITOR %s\n", shellQuote(p.Config))
	fmt.Fprintf(w, "  2. Test it:             %s check%s%s\n", dolly, scope, cfgFlag)
	fmt.Fprintf(w, "  3. Preview a run:       %s sync%s --dry-run%s\n", dolly, scope, cfgFlag)
	if systemd {
		fmt.Fprintf(w, "  4. Enable the timer:    systemctl --user enable --now dolly.timer\n")
		user := env.User
		if user == "" {
			user = "$USER"
		}
		fmt.Fprintf(w, "  5. Run while logged out: loginctl enable-linger %s   (may need an admin)\n", user)
	}
	return errors.Join(errs...)
}

// Uninstall stops and disables the timer, removes the units and the
// binary, and keeps the config (and any secrets next to it).
func Uninstall(env Env, o Options) error {
	w := o.Out
	p := PathsFor(env, o.ConfigPath)
	var errs []error
	if hasSystemd(env) {
		mv, _ := queryManager(env, o.Systemctl) // on error: nothing is linked we could know of
		senv, envErr := SystemctlEnv(env)
		if envErr != nil {
			// No user manager is running, so nothing is loaded to stop.
			fmt.Fprintf(w, "- systemctl --user: skipped, no user manager is running (no XDG_RUNTIME_DIR of this user's, no %s/%d), so no timer is active\n", env.RunUser, env.UID)
		} else {
			switch out, err := o.Systemctl.Run(senv, "disable", "--now", "dolly.timer"); {
			case err == nil:
				fmt.Fprintf(w, "✓ systemctl --user disable --now dolly.timer\n")
			case notLoaded(out):
				fmt.Fprintf(w, "✓ dolly.timer was not loaded\n")
			default:
				err = fmt.Errorf("systemctl --user disable --now dolly.timer: %v%s", err, outputSuffix(out))
				fmt.Fprintf(w, "✗ %v\n", err)
				errs = append(errs, err)
			}
		}
		removed := false
		// Links into another manager unit directory (systemctl disable
		// often removes them already): only symlinks to our files.
		if mv.known() && !sameDir(mv.unitDir(), p.UnitDir) {
			for _, f := range []string{p.Timer, p.Service} {
				l := filepath.Join(mv.unitDir(), filepath.Base(f))
				switch ours, err := linksTo(l, f); {
				case errors.Is(err, fs.ErrNotExist):
				case err != nil:
					fmt.Fprintf(w, "✗ %v\n", err)
					errs = append(errs, err)
				case !ours:
					fmt.Fprintf(w, "  warning: %s is not a link to %s; left alone\n", l, f)
				default:
					if err := os.Remove(l); err != nil {
						fmt.Fprintf(w, "✗ %v\n", err)
						errs = append(errs, err)
					} else {
						removed = true
						fmt.Fprintf(w, "✓ removed the link %s\n", l)
					}
				}
			}
		}
		for _, f := range []string{p.Timer, p.Service} {
			switch err := os.Remove(f); {
			case err == nil:
				removed = true
				fmt.Fprintf(w, "✓ removed %s\n", f)
			case errors.Is(err, fs.ErrNotExist):
				fmt.Fprintf(w, "✓ %s not present\n", f)
			default:
				fmt.Fprintf(w, "✗ %v\n", err)
				errs = append(errs, err)
			}
		}
		if removed && envErr == nil {
			if out, err := o.Systemctl.Run(senv, "daemon-reload"); err != nil {
				err = fmt.Errorf("systemctl --user daemon-reload: %v%s", err, outputSuffix(out))
				fmt.Fprintf(w, "✗ %v\n", err)
				errs = append(errs, err)
			} else {
				fmt.Fprintf(w, "✓ systemctl --user daemon-reload\n")
			}
		}
	}
	switch err := os.Remove(p.Binary); {
	case err == nil:
		fmt.Fprintf(w, "✓ removed %s\n", p.Binary)
	case errors.Is(err, fs.ErrNotExist):
		fmt.Fprintf(w, "✓ %s not present\n", p.Binary)
	default:
		fmt.Fprintf(w, "✗ %v\n", err)
		errs = append(errs, err)
	}
	if _, err := os.Stat(p.Config); err == nil {
		fmt.Fprintf(w, "Kept the config %s and any secret and CA files next to it; delete them yourself if you no longer need them.\n", p.Config)
	} else {
		fmt.Fprintf(w, "No config at %s; nothing kept.\n", p.Config)
	}
	return errors.Join(errs...)
}

// managerView is the systemd user manager's environment, which can differ
// from the shell's: on AD/SSSD hosts the login shell's $HOME may be
// /home/<user> while the passwd entry, and so the manager, has
// /home/<DOMAIN>/<user>. The manager reads units from its own
// $XDG_CONFIG_HOME/systemd/user (or <its HOME>/.config/systemd/user) and
// expands %h to its own HOME. The zero value means unknown: assume the
// shell's view.
type managerView struct {
	Home       string // the manager's HOME, absolute (the shell's spelling if the same directory)
	ConfigHome string // the manager's XDG_CONFIG_HOME if absolute, else ""
}

func (m managerView) known() bool { return m.Home != "" }

func (m managerView) configHome() string {
	if m.ConfigHome != "" {
		return m.ConfigHome
	}
	return filepath.Join(m.Home, ".config")
}

// unitDir is where the manager reads user units.
func (m managerView) unitDir() string { return filepath.Join(m.configHome(), "systemd", "user") }

// defaultConfig is the config the service finds without --config.
func (m managerView) defaultConfig() string {
	return filepath.Join(m.configHome(), "dolly", "dolly.yaml")
}

// sameConfig reports whether a and b are the same config path (or the
// same directory under another name).
func sameConfig(a, b string) bool {
	return filepath.Clean(a) == filepath.Clean(b) || filepath.Base(a) == filepath.Base(b) && sameFile(filepath.Dir(a), filepath.Dir(b))
}

// queryManager asks the user manager for its HOME and XDG_CONFIG_HOME via
// systemctl --user show-environment. On any error (no manager, no
// systemctl, no absolute HOME in the output) the view is unknown.
func queryManager(env Env, sc Systemctl) (managerView, error) {
	senv, err := SystemctlEnv(env)
	if err != nil {
		return managerView{}, err
	}
	out, err := sc.Run(senv, "show-environment")
	if err != nil {
		return managerView{}, fmt.Errorf("systemctl --user show-environment: %v%s", err, outputSuffix(out))
	}
	vars := parseEnvironment(out)
	m := managerView{Home: vars["HOME"], ConfigHome: vars["XDG_CONFIG_HOME"]}
	if m.Home == "" || !filepath.IsAbs(m.Home) {
		return managerView{}, errors.New("systemctl --user show-environment has no absolute HOME")
	}
	m.Home = filepath.Clean(m.Home)
	if m.ConfigHome != "" && filepath.IsAbs(m.ConfigHome) {
		m.ConfigHome = filepath.Clean(m.ConfigHome)
	} else {
		m.ConfigHome = "" // unset, or relative and so invalid
	}
	if sameDir(m.Home, env.Home) {
		// The same directory, maybe under another name (a symlink): keep
		// the shell's spelling so the paths compare.
		if m.ConfigHome == filepath.Join(m.Home, ".config") {
			m.ConfigHome = ""
		}
		m.Home = env.Home
	}
	return m, nil
}

// parseEnvironment parses show-environment output: one KEY=VALUE per line,
// the value possibly shell-quoted by systemd ("...", '...', or $'...').
func parseEnvironment(out string) map[string]string {
	vars := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimRight(line, "\r"), "=")
		if ok && k != "" {
			vars[k] = unquoteValue(v)
		}
	}
	return vars
}

func unquoteValue(v string) string {
	switch {
	case len(v) >= 3 && strings.HasPrefix(v, "$'") && strings.HasSuffix(v, "'"):
		v = v[2 : len(v)-1]
	case len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"':
		v = v[1 : len(v)-1]
	case len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'':
		return v[1 : len(v)-1]
	}
	if !strings.Contains(v, `\`) {
		return v
	}
	var b strings.Builder
	for i := 0; i < len(v); i++ {
		if v[i] == '\\' && i+1 < len(v) {
			i++
			switch v[i] {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			default:
				b.WriteByte(v[i])
			}
			continue
		}
		b.WriteByte(v[i])
	}
	return b.String()
}

// sameDir reports whether a and b name the same directory (equal paths, or
// the same inode through a symlink).
func sameDir(a, b string) bool {
	return filepath.Clean(a) == filepath.Clean(b) || sameFile(a, b)
}

func sameFile(a, b string) bool {
	sa, err1 := os.Stat(a)
	sb, err2 := os.Stat(b)
	return err1 == nil && err2 == nil && os.SameFile(sa, sb)
}

// linksTo reports whether l is a symlink to target. A missing l is
// fs.ErrNotExist; anything else at l (a regular file, a link elsewhere)
// is false.
func linksTo(l, target string) (bool, error) {
	st, err := os.Lstat(l)
	if err != nil {
		return false, err
	}
	if st.Mode()&fs.ModeSymlink == 0 {
		return false, nil
	}
	dest, err := os.Readlink(l)
	if err != nil {
		return false, err
	}
	if !filepath.IsAbs(dest) {
		dest = filepath.Join(filepath.Dir(l), dest)
	}
	return filepath.Clean(dest) == filepath.Clean(target) || sameFile(dest, target), nil
}

// linkUnit makes sure the manager's unit directory has a link to unit,
// with systemctl --user link (systemd writes the link, Dolly never writes
// into the manager's HOME). A link already there is kept; a regular file
// or a link elsewhere is left alone with a warning.
func linkUnit(w io.Writer, sc Systemctl, senv []string, dir, unit string) error {
	l := filepath.Join(dir, filepath.Base(unit))
	switch ours, err := linksTo(l, unit); {
	case err == nil && ours:
		fmt.Fprintf(w, "✓ unit: %s links to it already\n", l)
		return nil
	case err == nil:
		fmt.Fprintf(w, "  warning: %s exists and is not a link to %s; left alone, so the user manager won't run Dolly's unit until you remove it and re-run dolly install\n", l, unit)
		return nil
	case !errors.Is(err, fs.ErrNotExist):
		err = fmt.Errorf("checking %s: %w", l, err)
		fmt.Fprintf(w, "✗ unit: %v\n", err)
		return err
	}
	if out, err := sc.Run(senv, "link", unit); err != nil {
		err = fmt.Errorf("systemctl --user link %s: %v%s", unit, err, outputSuffix(out))
		fmt.Fprintf(w, "✗ %v\n", err)
		return err
	}
	fmt.Fprintf(w, "✓ systemctl --user link %s\n", unit)
	return nil
}

// notLoaded reports whether systemctl said the timer doesn't exist.
func notLoaded(out string) bool {
	o := strings.ToLower(out)
	for _, s := range []string{"not loaded", "does not exist", "not found", "no such file"} {
		if strings.Contains(o, s) {
			return true
		}
	}
	return false
}

func outputSuffix(out string) string {
	if out == "" {
		return ""
	}
	return ": " + out
}

func onPath(pathEnv, dir string) bool {
	for _, d := range filepath.SplitList(pathEnv) {
		if d != "" && filepath.Clean(d) == filepath.Clean(dir) {
			return true
		}
	}
	return false
}

// installBinary copies src to p.Binary atomically (temp file and rename),
// mode 0755, creating ~/.local and ~/.local/bin (0755) if missing.
func installBinary(src string, p Paths) (string, error) {
	if src == "" {
		return "", errors.New("can't find the running binary")
	}
	if r, err := filepath.EvalSymlinks(src); err == nil {
		src = r
	}
	if err := os.MkdirAll(p.BinDir, 0o755); err != nil {
		return "", err
	}
	if a, err := os.Stat(src); err == nil {
		if b, err := os.Stat(p.Binary); err == nil && os.SameFile(a, b) {
			return fmt.Sprintf("%s (running from there already)", p.Binary), nil
		}
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return "", fmt.Errorf("reading the running binary: %w", err)
	}
	if old, err := os.ReadFile(p.Binary); err == nil && bytes.Equal(old, data) {
		if st, err := os.Stat(p.Binary); err == nil && st.Mode().Perm() == 0o755 {
			return fmt.Sprintf("%s is up to date", p.Binary), nil
		}
	}
	if err := atomicWrite(p.Binary, data, 0o755); err != nil {
		return "", err
	}
	return fmt.Sprintf("installed %s (from %s)", p.Binary, src), nil
}

// createConfig writes the template to path with mode 0600 unless a file
// exists there. The directory is created 0700.
func createConfig(path string, template []byte) (bool, error) {
	if _, err := os.Lstat(path); err == nil {
		return false, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return false, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return false, nil // created meanwhile: still never overwritten
	}
	if err != nil {
		return false, err
	}
	if _, err := f.Write(template); err != nil {
		f.Close()
		os.Remove(path)
		return false, err
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return false, err
	}
	return true, nil
}

// writeIfChanged writes content to path (atomically) unless it already
// holds exactly that content. The directory is created 0700.
func writeIfChanged(path string, content []byte, mode fs.FileMode) (bool, error) {
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, content) {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return false, err
	}
	return true, atomicWrite(path, content, mode)
}

// atomicWrite writes data to a temp file in path's directory and renames
// it over path.
func atomicWrite(path string, data []byte, mode fs.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	fail := func(err error) error {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if _, err := f.Write(data); err != nil {
		return fail(err)
	}
	if err := f.Chmod(mode); err != nil {
		return fail(err)
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
