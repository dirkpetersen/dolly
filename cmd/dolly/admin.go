package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"runtime"
	"syscall"

	dolly "github.com/dirkpetersen/dolly"
	"github.com/dirkpetersen/dolly/internal/check"
	"github.com/dirkpetersen/dolly/internal/config"
	"github.com/dirkpetersen/dolly/internal/install"
)

// checkDial opens dolly check's LDAP connections; tests replace it.
var checkDial check.DialFunc = check.Dial

func runCheck(args []string, stdout, stderr io.Writer) int {
	fs := newFlags("check", "Test the config, every AD domain controller (TLS, bind, paged searches of both bases),\n"+
		"the target (TLS, bind, groups_base, users_base, state_base, and the server's size limit),\n"+
		"and SMTP (connect, EHLO, StartTLS, AUTH). Nothing is written, and no mail is sent\n"+
		"without --send-test-mail. Exits 1 if any check fails; warnings (!) don't.", stderr, "")
	cfgPath := fs.String("config", "", "config file")
	groups := fs.Bool("groups", false, "groups-only deployment (dolly sync --groups): a size-limited users_base is a warning")
	sendTest := fs.Bool("send-test-mail", false, "send one test message to notify.to")
	if code, ok := parse(fs, args); !ok {
		return code
	}
	path, err := config.Find(*cfgPath)
	if err != nil {
		fmt.Fprintf(stdout, "Config\n  ✗ %v\n", err)
		return exitError
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	res := check.Run(ctx, check.Options{
		ConfigPath: path, GroupsOnly: *groups, SendTestMail: *sendTest,
		Out: stdout, Dial: checkDial, SMTP: notifyOptions,
	})
	if res.Failed > 0 {
		return exitError
	}
	return exitOK
}

// installEnv and systemctl are dolly install's environment; tests replace
// them.
var (
	installEnv                   = realInstallEnv
	systemctl  install.Systemctl = install.ExecSystemctl{}
)

func realInstallEnv() (install.Env, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return install.Env{}, err
	}
	exe, err := os.Executable()
	if err != nil {
		return install.Env{}, fmt.Errorf("finding the running binary: %w", err)
	}
	name := os.Getenv("USER")
	if u, err := user.Current(); err == nil {
		name = u.Username
	}
	return install.Env{
		GOOS: runtime.GOOS, Home: home, ConfigHome: os.Getenv("XDG_CONFIG_HOME"), Path: os.Getenv("PATH"),
		RuntimeDir: os.Getenv("XDG_RUNTIME_DIR"), Environ: os.Environ(), UID: os.Getuid(), User: name,
		RunUser: "/run/user", Systemd: install.SystemdBooted, Executable: exe,
	}, nil
}

func runInstall(args []string, stdout, stderr io.Writer) int {
	fs := newFlags("install", "Copy this binary to ~/.local/bin/dolly, create the config from the built-in template if it\n"+
		"doesn't exist (never overwritten), and write dolly.service and dolly.timer to\n"+
		"$XDG_CONFIG_HOME/systemd/user/ (rewritten only when their content changes). The timer is\n"+
		"not enabled. --users/--groups and --config are baked into the service's ExecStart.\n"+
		"Safe to re-run; needs no root.", stderr, "")
	cfgPath := fs.String("config", "", "config file for the service (made absolute; created from the template if missing)")
	users := fs.Bool("users", false, "the timer syncs users only (dolly sync --users)")
	groups := fs.Bool("groups", false, "the timer syncs groups only (dolly sync --groups)")
	if code, ok := parse(fs, args); !ok {
		return code
	}
	env, err := installEnv()
	if err != nil {
		fmt.Fprintf(stderr, "dolly install: %v\n", err)
		return exitError
	}
	o := install.Options{Users: *users, Groups: *groups, Template: dolly.ConfigTemplate, Systemctl: systemctl, Out: stdout}
	if *cfgPath != "" {
		abs, err := filepath.Abs(*cfgPath)
		if err != nil {
			fmt.Fprintf(stderr, "dolly install: %v\n", err)
			return exitError
		}
		o.ConfigPath = abs
	}
	if err := install.Install(env, o); err != nil {
		fmt.Fprintf(stderr, "dolly install: %v\n", err)
		return exitError
	}
	return exitOK
}

func runUninstall(args []string, stdout, stderr io.Writer) int {
	fs := newFlags("uninstall", "Stop and disable dolly.timer, remove the systemd --user units and ~/.local/bin/dolly.\n"+
		"The config, and any secrets next to it, are kept.", stderr, "")
	cfgPath := fs.String("config", "", "the config file to report as kept (it is never removed)")
	if code, ok := parse(fs, args); !ok {
		return code
	}
	env, err := installEnv()
	if err != nil {
		fmt.Fprintf(stderr, "dolly uninstall: %v\n", err)
		return exitError
	}
	o := install.Options{Systemctl: systemctl, Out: stdout}
	if *cfgPath != "" {
		if o.ConfigPath, err = filepath.Abs(*cfgPath); err != nil {
			fmt.Fprintf(stderr, "dolly uninstall: %v\n", err)
			return exitError
		}
	}
	if err := install.Uninstall(env, o); err != nil {
		fmt.Fprintf(stderr, "dolly uninstall: %v\n", err)
		return exitError
	}
	return exitOK
}
