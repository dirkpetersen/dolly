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
// --groups (neither for a full sync) and --config if given.
func ExecStart(users, groups bool, configPath string) string {
	s := "%h/.local/bin/dolly sync"
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

// Units returns the content of dolly.service and dolly.timer, exactly as
// README "Running it on a schedule" shows them (with execStart after
// ExecStart=).
func Units(execStart string) (service, timer string) {
	service = `[Unit]
Description=Dolly AD to LDAP sync
After=network-online.target

[Service]
Type=oneshot
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
// manager: XDG_RUNTIME_DIR is unset and /run/user/<uid> doesn't exist.
type NoManagerError struct{ Dir, User string }

func (e *NoManagerError) Error() string {
	return fmt.Sprintf("XDG_RUNTIME_DIR is not set and %s does not exist: %s has no login session and no linger, so systemctl --user has no user manager to talk to. Run `loginctl enable-linger %s` (an admin may have to: sudo loginctl enable-linger %s), then run dolly install again",
		e.Dir, e.User, e.User, e.User)
}

// SystemctlEnv returns the environment for Dolly's own systemctl --user
// calls. With XDG_RUNTIME_DIR set it is nil (inherit). Without it (typical
// after su - or sudo -iu), if /run/user/<uid> exists, XDG_RUNTIME_DIR and
// DBUS_SESSION_BUS_ADDRESS point there; otherwise the user has no session
// and no linger, which is a NoManagerError. Only systemctl's environment
// changes: Dolly never edits shell startup files.
func SystemctlEnv(env Env) ([]string, error) {
	if env.RuntimeDir != "" {
		return nil, nil
	}
	dir := filepath.Join(env.RunUser, strconv.Itoa(env.UID))
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return nil, &NoManagerError{Dir: dir, User: env.User}
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

	// Units.
	execStart := ExecStart(o.Users, o.Groups, o.ConfigPath)
	systemd := hasSystemd(env)
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
		// Reload on every install, not only after a change: a previous
		// install may have written the units but failed to reload.
		senv, err := SystemctlEnv(env)
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
		dolly = p.Binary
	}
	cfgFlag := ""
	if o.ConfigPath != "" {
		cfgFlag = " --config " + o.ConfigPath
	}
	scope := ""
	switch {
	case o.Users && !o.Groups:
		scope = " --users"
	case o.Groups && !o.Users:
		scope = " --groups"
	}
	fmt.Fprintf(w, "\nNext steps:\n")
	fmt.Fprintf(w, "  1. Edit the config:     $EDITOR %s\n", p.Config)
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
		senv, envErr := SystemctlEnv(env)
		if envErr != nil {
			// No user manager is running, so nothing is loaded to stop.
			fmt.Fprintf(w, "- systemctl --user: skipped, no user manager is running (XDG_RUNTIME_DIR unset, no %s/%d), so no timer is active\n", env.RunUser, env.UID)
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
// mode 0755, creating ~/.local/bin (0700) if missing.
func installBinary(src string, p Paths) (string, error) {
	if src == "" {
		return "", errors.New("can't find the running binary")
	}
	if r, err := filepath.EvalSymlinks(src); err == nil {
		src = r
	}
	if err := os.MkdirAll(p.BinDir, 0o700); err != nil {
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
