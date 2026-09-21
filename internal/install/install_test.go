package install

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// fakeSystemctl records calls instead of running systemctl.
type fakeSystemctl struct {
	calls []string
	envs  [][]string
	fail  map[string]struct {
		out string
		err error
	}
	// showEnv is the output of show-environment.
	showEnv string
	// linkDir, if set, is the manager's unit directory: link <path>
	// creates linkDir/<base> -> path there, as systemd does.
	linkDir string
}

func (f *fakeSystemctl) Run(env []string, args ...string) (string, error) {
	c := strings.Join(args, " ")
	f.calls = append(f.calls, c)
	f.envs = append(f.envs, env)
	if r, ok := f.fail[c]; ok {
		return r.out, r.err
	}
	switch {
	case c == "show-environment":
		return f.showEnv, nil
	case len(args) == 2 && args[0] == "link" && f.linkDir != "":
		if err := os.MkdirAll(f.linkDir, 0o755); err != nil {
			return "", err
		}
		return "Created symlink.", os.Symlink(args[1], filepath.Join(f.linkDir, filepath.Base(args[1])))
	}
	return "", nil
}

// setManagerHome makes show-environment report home as the manager's HOME.
func (f *fixture) setManagerHome(home string, extra ...string) {
	f.sc.showEnv = strings.Join(append([]string{"HOME=" + home, "LANG=C.UTF-8", "PATH=/usr/bin"}, extra...), "\n") + "\n"
}

type fixture struct {
	env  Env
	sc   *fakeSystemctl
	out  bytes.Buffer
	home string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "home")
	exe := filepath.Join(root, "build", "dolly")
	if err := os.MkdirAll(filepath.Dir(exe), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, []byte("binary v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	runUser := filepath.Join(root, "run", "user")
	if err := os.MkdirAll(filepath.Join(runUser, "1234"), 0o700); err != nil {
		t.Fatal(err)
	}
	f := &fixture{home: home, sc: &fakeSystemctl{}}
	f.setManagerHome(home)
	f.env = Env{
		GOOS: "linux", Home: home, Path: "/usr/bin:" + filepath.Join(home, ".local", "bin"),
		RuntimeDir: filepath.Join(runUser, "1234"), Environ: []string{"HOME=" + home, "PATH=/usr/bin"},
		UID: 1234, User: "svc-dolly", RunUser: runUser, Systemd: func() bool { return true }, Executable: exe,
	}
	return f
}

func (f *fixture) opts() Options {
	return Options{Template: []byte("template: yes\n"), Systemctl: f.sc, Out: &f.out}
}

func (f *fixture) paths() Paths { return PathsFor(f.env, "") }

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return st.Mode().Perm()
}

// The units must match README "Running it on a schedule" byte for byte.
func TestUnitsMatchREADME(t *testing.T) {
	readme := read(t, "../../README.md")
	_, sec, ok := strings.Cut(readme, "## Running it on a schedule")
	if !ok {
		t.Fatal("README has no scheduling section")
	}
	_, block, ok := strings.Cut(sec, "```ini\n")
	if !ok {
		t.Fatal("no ini block")
	}
	block, _, _ = strings.Cut(block, "```")
	units := map[string]string{}
	var name string
	for _, line := range strings.SplitAfter(block, "\n") {
		if strings.HasPrefix(line, "# ~/.config/systemd/user/") {
			name = strings.TrimSpace(strings.TrimPrefix(line, "# ~/.config/systemd/user/"))
			continue
		}
		units[name] += line
	}
	service, timer := Units(ExecStart(false, false, ""))
	if got := strings.TrimRight(units["dolly.service"], "\n") + "\n"; got != service {
		t.Errorf("dolly.service differs from README:\n%s\nvs\n%s", service, got)
	}
	if got := strings.TrimRight(units["dolly.timer"], "\n") + "\n"; got != timer {
		t.Errorf("dolly.timer differs from README:\n%s\nvs\n%s", timer, got)
	}
	if !strings.Contains(sec, "ExecStart=%h/.local/bin/dolly sync --groups") {
		t.Error("README must show the dolly install --groups ExecStart")
	}
}

func TestExecStart(t *testing.T) {
	for _, tc := range []struct {
		users, groups bool
		config, want  string
	}{
		{false, false, "", "%h/.local/bin/dolly sync"},
		{true, true, "", "%h/.local/bin/dolly sync"},
		{false, true, "", "%h/.local/bin/dolly sync --groups"},
		{true, false, "", "%h/.local/bin/dolly sync --users"},
		{false, true, "/srv/dolly/a.yaml", "%h/.local/bin/dolly sync --groups --config /srv/dolly/a.yaml"},
		{false, false, "/srv/my dolly/100%.yaml", `%h/.local/bin/dolly sync --config "/srv/my dolly/100%%.yaml"`},
	} {
		if got := ExecStart(tc.users, tc.groups, tc.config); got != tc.want {
			t.Errorf("ExecStart(%v, %v, %q) = %q, want %q", tc.users, tc.groups, tc.config, got, tc.want)
		}
	}
}

func TestInstallFreshAndIdempotent(t *testing.T) {
	f := newFixture(t)
	p := f.paths()
	if err := Install(f.env, f.opts()); err != nil {
		t.Fatalf("%v\n%s", err, f.out.String())
	}
	// MkdirAll honors the umask, so only require that the owner has full
	// access and the dirs are not the config's 0700-only mode under umask 022.
	um := currentUmask()
	if read(t, p.Binary) != "binary v1" || mode(t, p.Binary) != 0o755 || mode(t, p.BinDir) != 0o755&^um ||
		mode(t, filepath.Dir(p.BinDir)) != 0o755&^um {
		t.Errorf("binary: mode %v, dir %v, ~/.local %v", mode(t, p.Binary), mode(t, p.BinDir), mode(t, filepath.Dir(p.BinDir)))
	}
	if read(t, p.Config) != "template: yes\n" || mode(t, p.Config) != 0o600 || mode(t, filepath.Dir(p.Config)) != 0o700 {
		t.Errorf("config: mode %v", mode(t, p.Config))
	}
	if p.Config != filepath.Join(f.home, ".config", "dolly", "dolly.yaml") || p.UnitDir != filepath.Join(f.home, ".config", "systemd", "user") {
		t.Errorf("paths %+v", p)
	}
	service, timer := Units("%h/.local/bin/dolly sync")
	if read(t, p.Service) != service || read(t, p.Timer) != timer || mode(t, p.Service) != 0o644 {
		t.Error("units")
	}
	out := f.out.String()
	for _, want := range []string{"✓ binary: installed", "✓ config: created", "✓ unit: wrote", "ExecStart=%h/.local/bin/dolly sync\n",
		"daemon-reload (units changed)", "systemctl --user enable --now dolly.timer", "loginctl enable-linger svc-dolly", "dolly check\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Join(f.sc.calls, ";") != "show-environment;daemon-reload" || f.sc.envs[0] != nil || strings.Contains(out, "note:") {
		t.Errorf("systemctl calls %v (env %v); the timer must not be enabled", f.sc.calls, f.sc.envs)
	}

	// Re-run: the config is edited by the user and never overwritten; the
	// units are unchanged; the binary is up to date.
	if err := os.WriteFile(p.Config, []byte("edited\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(p.Service)
	f.out.Reset()
	if err := Install(f.env, f.opts()); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(p.Service)
	out = f.out.String()
	if read(t, p.Config) != "edited\n" || !strings.Contains(out, "exists, left unchanged") || !strings.Contains(out, "is up to date") ||
		!strings.Contains(out, "unchanged") || !after.ModTime().Equal(before.ModTime()) || !strings.Contains(out, "daemon-reload (units unchanged)") {
		t.Errorf("re-run:\n%s", out)
	}

	// A new binary replaces the old one.
	if err := os.WriteFile(f.env.Executable, []byte("binary v2"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.out.Reset()
	if err := Install(f.env, f.opts()); err != nil || read(t, p.Binary) != "binary v2" {
		t.Errorf("upgrade: %v\n%s", err, f.out.String())
	}
	// Running the installed binary itself.
	f.env.Executable = p.Binary
	f.out.Reset()
	if err := Install(f.env, f.opts()); err != nil || !strings.Contains(f.out.String(), "running from there already") {
		t.Errorf("self: %v\n%s", err, f.out.String())
	}
	if left, _ := filepath.Glob(filepath.Join(p.BinDir, ".dolly*")); len(left) != 0 {
		t.Errorf("temp files left: %v", left)
	}
}

// --groups and --config are baked into the unit; changing them rewrites it.
func TestInstallFlagsRewriteUnit(t *testing.T) {
	f := newFixture(t)
	p := f.paths()
	o := f.opts()
	o.Groups = true
	if err := Install(f.env, o); err != nil {
		t.Fatal(err)
	}
	service, _ := Units("%h/.local/bin/dolly sync --groups")
	if read(t, p.Service) != service || !strings.Contains(f.out.String(), "ExecStart=%h/.local/bin/dolly sync --groups\n") ||
		!strings.Contains(f.out.String(), "dolly check --groups") {
		t.Errorf("--groups:\n%s\n%s", read(t, p.Service), f.out.String())
	}

	cfg := filepath.Join(f.home, "targets", "a.yaml")
	o.ConfigPath = cfg
	f.out.Reset()
	if err := Install(f.env, o); err != nil {
		t.Fatal(err)
	}
	service, _ = Units("%h/.local/bin/dolly sync --groups --config " + cfg)
	if read(t, p.Service) != service || !strings.Contains(f.out.String(), "✓ unit: wrote "+p.Service) ||
		!strings.Contains(f.out.String(), "daemon-reload (units changed)") || read(t, cfg) != "template: yes\n" {
		t.Errorf("--config:\n%s\n%s", read(t, p.Service), f.out.String())
	}
	if !strings.Contains(f.out.String(), "✓ unit: "+p.Timer+" unchanged") {
		t.Errorf("the timer must not be rewritten:\n%s", f.out.String())
	}
}

func TestInstallXDGAndPath(t *testing.T) {
	f := newFixture(t)
	xdg := filepath.Join(f.home, "xdg")
	f.env.ConfigHome = xdg
	f.env.Path = "/usr/bin"
	if err := Install(f.env, f.opts()); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{filepath.Join(xdg, "dolly", "dolly.yaml"), filepath.Join(xdg, "systemd", "user", "dolly.timer")} {
		if _, err := os.Stat(p); err != nil {
			t.Error(err)
		}
	}
	out := f.out.String()
	bin := filepath.Join(f.home, ".local", "bin", "dolly")
	if !strings.Contains(out, "is not on PATH") || !strings.Contains(out, bin+" check") {
		t.Errorf("PATH warning and full path in next steps:\n%s", out)
	}
	// A relative XDG_CONFIG_HOME is invalid and ignored.
	f.env.ConfigHome = "relative"
	if p := PathsFor(f.env, ""); p.Config != filepath.Join(f.home, ".config", "dolly", "dolly.yaml") {
		t.Errorf("relative XDG_CONFIG_HOME: %s", p.Config)
	}
}

func TestInstallRuntimeDirFallback(t *testing.T) {
	f := newFixture(t)
	f.env.RuntimeDir = ""
	f.env.Environ = append(f.env.Environ, "DBUS_SESSION_BUS_ADDRESS=stale")
	if err := Install(f.env, f.opts()); err != nil {
		t.Fatalf("%v\n%s", err, f.out.String())
	}
	dir := filepath.Join(f.env.RunUser, "1234")
	env := strings.Join(f.sc.envs[0], "\n")
	if !strings.Contains(env, "XDG_RUNTIME_DIR="+dir) || !strings.Contains(env, "DBUS_SESSION_BUS_ADDRESS=unix:path="+dir+"/bus") ||
		strings.Contains(env, "stale") || !strings.Contains(env, "HOME="+f.home) {
		t.Errorf("systemctl env:\n%s", env)
	}
}

// An XDG_RUNTIME_DIR that isn't this user's (inherited after sudo -u svc)
// counts as unset: systemctl gets /run/user/<own uid>, or the linger hint.
func TestInstallForeignRuntimeDir(t *testing.T) {
	f := newFixture(t)
	f.env.RuntimeDir = filepath.Join(f.env.RunUser, "1000") // the admin's
	f.env.Environ = append(f.env.Environ, "XDG_RUNTIME_DIR="+f.env.RuntimeDir, "DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/1000/bus")
	if err := Install(f.env, f.opts()); err != nil {
		t.Fatalf("%v\n%s", err, f.out.String())
	}
	dir := filepath.Join(f.env.RunUser, "1234")
	env := strings.Join(f.sc.envs[0], "\n")
	if !strings.Contains(env, "XDG_RUNTIME_DIR="+dir+"\n") || !strings.Contains(env, "DBUS_SESSION_BUS_ADDRESS=unix:path="+dir+"/bus") ||
		strings.Contains(env, "1000") {
		t.Errorf("systemctl env:\n%s", env)
	}

	// Own runtime dir, spelled with a trailing slash: inherited as is.
	if senv, err := SystemctlEnv(Env{RuntimeDir: dir + "/", RunUser: f.env.RunUser, UID: 1234}); err != nil || senv != nil {
		t.Errorf("own XDG_RUNTIME_DIR: env %v, err %v", senv, err)
	}

	// Foreign and no /run/user/<uid>: the linger hint, naming the foreign dir.
	f2 := newFixture(t)
	f2.env.RuntimeDir = "/run/user/1000"
	f2.env.UID = 999
	err := Install(f2.env, f2.opts())
	var nm *NoManagerError
	if !errors.As(err, &nm) || !strings.Contains(err.Error(), "loginctl enable-linger svc-dolly") || !strings.Contains(err.Error(), "XDG_RUNTIME_DIR=/run/user/1000 is not this user's") {
		t.Fatalf("err = %v", err)
	}
	if len(f2.sc.calls) != 0 {
		t.Errorf("systemctl was called: %v", f2.sc.calls)
	}
}

// Paths with spaces or shell metacharacters are quoted in the next steps.
func TestNextStepsQuoting(t *testing.T) {
	f := newFixture(t)
	f.env.Path = "/usr/bin"
	f.env.Home = filepath.Join(f.home, "my home")
	f.setManagerHome(f.env.Home)
	o := f.opts()
	o.ConfigPath = filepath.Join(f.env.Home, "it's $x.yaml")
	if err := Install(f.env, o); err != nil {
		t.Fatalf("%v\n%s", err, f.out.String())
	}
	bin := "'" + filepath.Join(f.env.Home, ".local", "bin", "dolly") + "'"
	cfg := `'` + filepath.Join(f.env.Home, "it") + `'\''s $x.yaml'`
	out := f.out.String()
	for _, want := range []string{"$EDITOR " + cfg + "\n", bin + " check --config " + cfg + "\n", bin + " sync --dry-run --config " + cfg + "\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	for in, want := range map[string]string{
		"/home/svc/.config/dolly/a.yaml": "/home/svc/.config/dolly/a.yaml",
		"/tmp/a b":                       "'/tmp/a b'",
		"/tmp/a;rm":                      "'/tmp/a;rm'",
		"/tmp/$(x)":                      "'/tmp/$(x)'",
		"":                               "''",
	} {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestInstallNoUserManager(t *testing.T) {
	f := newFixture(t)
	f.env.RuntimeDir = ""
	f.env.UID = 999 // no /run/user/999
	err := Install(f.env, f.opts())
	var nm *NoManagerError
	if !errors.As(err, &nm) || !strings.Contains(err.Error(), "loginctl enable-linger svc-dolly") {
		t.Fatalf("err = %v", err)
	}
	if len(f.sc.calls) != 0 {
		t.Errorf("systemctl was called: %v", f.sc.calls)
	}
	// Binary, config, and units are in place; the next install after
	// enable-linger only has to reload.
	p := f.paths()
	for _, x := range []string{p.Binary, p.Config, p.Service, p.Timer} {
		if _, err := os.Stat(x); err != nil {
			t.Error(err)
		}
	}
}

func TestInstallNonLinux(t *testing.T) {
	for _, tc := range []struct {
		goos    string
		systemd bool
		want    string
	}{{"darwin", true, "not Linux (darwin)"}, {"linux", false, "this host doesn't run systemd"}} {
		f := newFixture(t)
		f.env.GOOS = tc.goos
		f.env.Systemd = func() bool { return tc.systemd }
		if err := Install(f.env, f.opts()); err != nil {
			t.Fatal(err)
		}
		p := f.paths()
		out := f.out.String()
		if _, err := os.Stat(p.UnitDir); err == nil || len(f.sc.calls) != 0 || !strings.Contains(out, "units: skipped, "+tc.want) ||
			strings.Contains(out, "enable --now") {
			t.Errorf("%s: units written or systemctl called:\n%s", tc.goos, out)
		}
		if read(t, p.Binary) != "binary v1" || read(t, p.Config) != "template: yes\n" {
			t.Errorf("%s: binary and config must be installed", tc.goos)
		}
	}
}

func TestUninstall(t *testing.T) {
	f := newFixture(t)
	if err := Install(f.env, f.opts()); err != nil {
		t.Fatal(err)
	}
	p := f.paths()
	f.sc.calls = nil
	f.out.Reset()
	if err := Uninstall(f.env, f.opts()); err != nil {
		t.Fatalf("%v\n%s", err, f.out.String())
	}
	if strings.Join(f.sc.calls, ";") != "show-environment;disable --now dolly.timer;daemon-reload" {
		t.Errorf("calls %v", f.sc.calls)
	}
	for _, x := range []string{p.Binary, p.Service, p.Timer} {
		if _, err := os.Stat(x); err == nil {
			t.Errorf("%s still exists", x)
		}
	}
	if read(t, p.Config) != "template: yes\n" || !strings.Contains(f.out.String(), "Kept the config "+p.Config) {
		t.Errorf("config must be kept:\n%s", f.out.String())
	}

	// Again: nothing loaded, nothing to remove, still success.
	f.sc.calls = nil
	f.sc.fail = map[string]struct {
		out string
		err error
	}{"disable --now dolly.timer": {"Failed to disable unit: Unit file dolly.timer does not exist.", errors.New("exit status 1")}}
	f.out.Reset()
	if err := Uninstall(f.env, f.opts()); err != nil {
		t.Fatalf("second uninstall: %v\n%s", err, f.out.String())
	}
	if strings.Join(f.sc.calls, ";") != "show-environment;disable --now dolly.timer" || !strings.Contains(f.out.String(), "was not loaded") {
		t.Errorf("calls %v\n%s", f.sc.calls, f.out.String())
	}

	// Another systemctl error is reported.
	f.sc.fail["disable --now dolly.timer"] = struct {
		out string
		err error
	}{"Access denied", errors.New("exit status 1")}
	if err := Uninstall(f.env, f.opts()); err == nil || !strings.Contains(err.Error(), "Access denied") {
		t.Errorf("err = %v", err)
	}

	// No user manager: no systemctl calls, files still removed.
	g := newFixture(t)
	if err := Install(g.env, g.opts()); err != nil {
		t.Fatal(err)
	}
	g.env.RuntimeDir, g.env.UID = "", 999
	g.sc.calls = nil
	if err := Uninstall(g.env, g.opts()); err != nil || len(g.sc.calls) != 0 {
		t.Errorf("err %v, calls %v", err, g.sc.calls)
	}
	if _, err := os.Stat(g.paths().Service); err == nil {
		t.Error("service not removed")
	}
}

// currentUmask returns the process umask (read by setting and restoring it).
func currentUmask() os.FileMode {
	m := syscall.Umask(0)
	syscall.Umask(m)
	return os.FileMode(m)
}

// splitFixture is a host where the systemd user manager has another HOME
// than the shell (AD/SSSD: /home/<DOMAIN>/<user> vs /home/<user>).
func splitFixture(t *testing.T) (*fixture, string) {
	f := newFixture(t)
	mhome := filepath.Join(filepath.Dir(f.home), "home-DOMAIN", "svc-dolly")
	f.setManagerHome(mhome)
	f.sc.linkDir = filepath.Join(mhome, ".config", "systemd", "user")
	return f, mhome
}

// "$HOME always wins": files stay under the shell's HOME, the units are
// linked into the manager's unit directory, and ExecStart is absolute.
func TestInstallSplitHome(t *testing.T) {
	f, mhome := splitFixture(t)
	p := f.paths()
	if err := Install(f.env, f.opts()); err != nil {
		t.Fatalf("%v\n%s", err, f.out.String())
	}
	if want := strings.Join([]string{"show-environment", "link " + p.Service, "link " + p.Timer, "daemon-reload"}, ";"); strings.Join(f.sc.calls, ";") != want {
		t.Errorf("calls %v, want %s", f.sc.calls, want)
	}
	execStart := p.Binary + " sync --config " + filepath.Join(f.home, ".config", "dolly", "dolly.yaml")
	service, _ := Units(execStart)
	if read(t, p.Service) != service || read(t, p.Config) != "template: yes\n" || read(t, p.Binary) != "binary v1" {
		t.Errorf("service:\n%s", read(t, p.Service))
	}
	out := f.out.String()
	for _, want := range []string{"ExecStart=" + execStart + "\n", "systemd user manager uses HOME=" + mhome + ", the shell uses HOME=" + f.home,
		"linked into " + f.sc.linkDir, "systemctl --user enable --now dolly.timer"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	// Nothing written by Dolly into the manager's HOME but systemd's links.
	for _, name := range []string{"dolly.service", "dolly.timer"} {
		if ok, err := linksTo(filepath.Join(f.sc.linkDir, name), filepath.Join(p.UnitDir, name)); err != nil || !ok {
			t.Errorf("%s: link %v %v", name, ok, err)
		}
	}
	if _, err := os.Stat(filepath.Join(mhome, ".local")); err == nil {
		t.Error("the binary must not go to the manager's HOME")
	}

	// Re-run with --groups: the file is rewritten, the links are kept.
	f.sc.calls = nil
	f.out.Reset()
	o := f.opts()
	o.Groups = true
	if err := Install(f.env, o); err != nil {
		t.Fatalf("%v\n%s", err, f.out.String())
	}
	if strings.Join(f.sc.calls, ";") != "show-environment;daemon-reload" || !strings.Contains(f.out.String(), "links to it already") {
		t.Errorf("re-run calls %v\n%s", f.sc.calls, f.out.String())
	}
	if !strings.Contains(read(t, filepath.Join(f.sc.linkDir, "dolly.service")), p.Binary+" sync --groups --config ") {
		t.Error("the link must show the rewritten unit")
	}

	// A missing link is re-created.
	if err := os.Remove(filepath.Join(f.sc.linkDir, "dolly.timer")); err != nil {
		t.Fatal(err)
	}
	f.sc.calls = nil
	if err := Install(f.env, o); err != nil || strings.Join(f.sc.calls, ";") != "show-environment;link "+p.Timer+";daemon-reload" {
		t.Errorf("relink: %v, calls %v", err, f.sc.calls)
	}

	// An explicit --config is used as is.
	cfg := filepath.Join(f.home, "t", "a.yaml")
	o.ConfigPath = cfg
	f.out.Reset()
	if err := Install(f.env, o); err != nil || !strings.Contains(f.out.String(), "ExecStart="+p.Binary+" sync --groups --config "+cfg+"\n") {
		t.Errorf("--config: %v\n%s", err, f.out.String())
	}
}

// A regular file (or a foreign link) in the manager's unit directory is
// left alone, with a warning.
func TestInstallSplitHomeForeignUnit(t *testing.T) {
	f, _ := splitFixture(t)
	p := f.paths()
	if err := os.MkdirAll(f.sc.linkDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mine := filepath.Join(f.sc.linkDir, "dolly.service")
	if err := os.WriteFile(mine, []byte("[Service]\nExecStart=/bin/true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Install(f.env, f.opts()); err != nil {
		t.Fatalf("%v\n%s", err, f.out.String())
	}
	if read(t, mine) != "[Service]\nExecStart=/bin/true\n" || !strings.Contains(f.out.String(), "warning: "+mine+" exists and is not a link") {
		t.Errorf("foreign unit touched or no warning:\n%s", f.out.String())
	}
	if strings.Join(f.sc.calls, ";") != "show-environment;link "+p.Timer+";daemon-reload" {
		t.Errorf("calls %v", f.sc.calls)
	}

	// Uninstall removes only the link to our timer, never the foreign file.
	f.sc.calls = nil
	f.out.Reset()
	if err := Uninstall(f.env, f.opts()); err != nil {
		t.Fatalf("%v\n%s", err, f.out.String())
	}
	if read(t, mine) != "[Service]\nExecStart=/bin/true\n" {
		t.Error("uninstall removed a foreign unit")
	}
	if _, err := os.Lstat(filepath.Join(f.sc.linkDir, "dolly.timer")); err == nil {
		t.Error("timer link not removed")
	}
	for _, x := range []string{p.Service, p.Timer, p.Binary} {
		if _, err := os.Stat(x); err == nil {
			t.Errorf("%s still exists", x)
		}
	}
	out := f.out.String()
	if !strings.Contains(out, "✓ removed the link "+filepath.Join(f.sc.linkDir, "dolly.timer")) || !strings.Contains(out, "warning: "+mine+" is not a link") {
		t.Errorf("uninstall output:\n%s", out)
	}
	if strings.Join(f.sc.calls, ";") != "show-environment;disable --now dolly.timer;daemon-reload" {
		t.Errorf("calls %v", f.sc.calls)
	}
}

// Uninstall also copes with systemctl disable having removed the links
// already, and leaves a link that points elsewhere.
func TestUninstallSplitHomeLinksGone(t *testing.T) {
	f, _ := splitFixture(t)
	if err := Install(f.env, f.opts()); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(f.sc.linkDir, "dolly.timer")); err != nil { // as disable does
		t.Fatal(err)
	}
	other := filepath.Join(f.sc.linkDir, "dolly.service")
	if err := os.Remove(other); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/elsewhere/dolly.service", other); err != nil {
		t.Fatal(err)
	}
	f.out.Reset()
	if err := Uninstall(f.env, f.opts()); err != nil {
		t.Fatalf("%v\n%s", err, f.out.String())
	}
	if dest, err := os.Readlink(other); err != nil || dest != "/elsewhere/dolly.service" {
		t.Errorf("foreign link touched: %q %v", dest, err)
	}
	if _, err := os.Stat(f.paths().Service); err == nil {
		t.Error("our service not removed")
	}
}

// Same HOME but another XDG_CONFIG_HOME in the manager: %h stays, the
// units are linked, and --config is baked in.
func TestInstallManagerXDGConfigHome(t *testing.T) {
	f := newFixture(t)
	mxdg := filepath.Join(f.home, "mgr-xdg")
	f.setManagerHome(f.home, "XDG_CONFIG_HOME="+mxdg)
	f.sc.linkDir = filepath.Join(mxdg, "systemd", "user")
	p := f.paths()
	if err := Install(f.env, f.opts()); err != nil {
		t.Fatalf("%v\n%s", err, f.out.String())
	}
	want := "%h/.local/bin/dolly sync --config " + p.Config
	if service, _ := Units(want); read(t, p.Service) != service {
		t.Errorf("service:\n%s", read(t, p.Service))
	}
	if !strings.Contains(strings.Join(f.sc.calls, ";"), "link "+p.Service) || strings.Contains(f.out.String(), "uses HOME=") {
		t.Errorf("calls %v\n%s", f.sc.calls, f.out.String())
	}
}

// The manager's HOME is the shell's through a symlink: nothing changes.
func TestInstallManagerHomeSymlink(t *testing.T) {
	f := newFixture(t)
	if err := os.MkdirAll(f.home, 0o755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(filepath.Dir(f.home), "alias")
	if err := os.Symlink(f.home, alias); err != nil {
		t.Fatal(err)
	}
	f.setManagerHome(alias)
	if err := Install(f.env, f.opts()); err != nil {
		t.Fatal(err)
	}
	if strings.Join(f.sc.calls, ";") != "show-environment;daemon-reload" || !strings.Contains(f.out.String(), "ExecStart=%h/.local/bin/dolly sync\n") {
		t.Errorf("calls %v\n%s", f.sc.calls, f.out.String())
	}
}

// show-environment failing (or without HOME) falls back to the shell's
// view: %h, no links.
func TestInstallShowEnvironmentFails(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*fakeSystemctl)
	}{
		{"error", func(sc *fakeSystemctl) {
			sc.fail = map[string]struct {
				out string
				err error
			}{"show-environment": {"Failed to connect to bus", errors.New("exit status 1")}}
		}},
		{"no HOME", func(sc *fakeSystemctl) { sc.showEnv = "LANG=C\n" }},
	} {
		f := newFixture(t)
		tc.set(f.sc)
		p := f.paths()
		if err := Install(f.env, f.opts()); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		service, _ := Units("%h/.local/bin/dolly sync")
		if read(t, p.Service) != service || strings.Join(f.sc.calls, ";") != "show-environment;daemon-reload" ||
			!strings.Contains(f.out.String(), "note: couldn't read the systemd user manager's environment") {
			t.Errorf("%s: calls %v\n%s", tc.name, f.sc.calls, f.out.String())
		}
		f.sc.calls = nil
		if err := Uninstall(f.env, f.opts()); err != nil {
			t.Errorf("%s: uninstall %v", tc.name, err)
		}
		if _, err := os.Stat(p.Service); err == nil {
			t.Errorf("%s: service not removed", tc.name)
		}
	}
}

func TestParseEnvironment(t *testing.T) {
	got := parseEnvironment("HOME=/home/DOM/a\nXDG_CONFIG_HOME=$'/x/a b\\'c'\nQ=\"/q r\"\nS='/s t'\nE=/p\\ q\nbad\n")
	for k, want := range map[string]string{"HOME": "/home/DOM/a", "XDG_CONFIG_HOME": "/x/a b'c", "Q": "/q r", "S": "/s t", "E": "/p q"} {
		if got[k] != want {
			t.Errorf("%s = %q, want %q", k, got[k], want)
		}
	}
}
