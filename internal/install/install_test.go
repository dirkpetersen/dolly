package install

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
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
}

func (f *fakeSystemctl) Run(env []string, args ...string) (string, error) {
	c := strings.Join(args, " ")
	f.calls = append(f.calls, c)
	f.envs = append(f.envs, env)
	if r, ok := f.fail[c]; ok {
		return r.out, r.err
	}
	return "", nil
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
	f.env = Env{
		GOOS: "linux", Home: home, Path: "/usr/bin:" + filepath.Join(home, ".local", "bin"),
		RuntimeDir: "/run/user/1234", Environ: []string{"HOME=" + home, "PATH=/usr/bin"},
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
	if read(t, p.Binary) != "binary v1" || mode(t, p.Binary) != 0o755 || mode(t, p.BinDir) != 0o700 {
		t.Errorf("binary: mode %v, dir %v", mode(t, p.Binary), mode(t, p.BinDir))
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
	if strings.Join(f.sc.calls, ";") != "daemon-reload" || f.sc.envs[0] != nil {
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
	if strings.Join(f.sc.calls, ";") != "disable --now dolly.timer;daemon-reload" {
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
	if strings.Join(f.sc.calls, ";") != "disable --now dolly.timer" || !strings.Contains(f.out.String(), "was not loaded") {
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
