package main

import (
	"context"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	dolly "github.com/dirkpetersen/dolly"
	"github.com/dirkpetersen/dolly/internal/check"
	"github.com/dirkpetersen/dolly/internal/config"
	"github.com/dirkpetersen/dolly/internal/install"
	"github.com/dirkpetersen/dolly/internal/ldapconn"
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
		"not enabled. --users/--groups and --config are baked into the service's ExecStart. If the\n"+
		"systemd user manager has another HOME than $HOME (AD/SSSD hosts), everything still stays under\n"+
		"$HOME: the units are registered with systemctl --user link and ExecStart uses absolute paths.\n"+
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

// fetchTimeout bounds dolly fetch-cert's connection (it reads no config,
// so there is no network_timeout).
const fetchTimeout = 30 * time.Second

func runFetchCert(args []string, stdout, stderr io.Writer) int {
	fs := newFlags("fetch-cert URL", "Connect to an LDAP server (ldaps://host[:636], or ldap://host[:389] with StartTLS) without\n"+
		"verifying it and without sending credentials, and print the certificate chain it sends: subject,\n"+
		"issuer, validity, and SHA-256 fingerprint of each certificate, and a tls_fingerprint line for the\n"+
		"leaf. Compare the fingerprint with the server's own certificate (another channel) before you pin it.", stderr, "")
	out := fs.String("out", "", "also write the chain as PEM to this file (for ca_file); an existing file is never overwritten")
	var rawURL string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		rawURL, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitError
	}
	rest := fs.Args()
	if rawURL == "" && len(rest) > 0 {
		rawURL, rest = rest[0], rest[1:]
	}
	if rawURL == "" || len(rest) > 0 {
		fmt.Fprintf(stderr, "Usage: dolly fetch-cert URL [--out FILE]\n")
		return exitError
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	chain, err := ldapconn.FetchChain(ctx, rawURL, fetchTimeout)
	if err != nil {
		fmt.Fprintf(stderr, "dolly fetch-cert: %s: %v\n", rawURL, err)
		return exitError
	}
	if len(chain) == 0 {
		fmt.Fprintf(stderr, "dolly fetch-cert: %s sent no certificate\n", rawURL)
		return exitError
	}
	now := time.Now()
	fmt.Fprintf(stdout, "Certificate chain sent by %s (leaf first, not verified):\n", rawURL)
	for i, c := range chain {
		state := ""
		switch {
		case now.After(c.NotAfter):
			state = " EXPIRED"
		case now.Before(c.NotBefore):
			state = " NOT YET VALID"
		}
		fmt.Fprintf(stdout, "\n  [%d] subject: %s\n      issuer:  %s\n      valid:   %s to %s%s\n      sha256:  %s\n",
			i, c.Subject, c.Issuer, c.NotBefore.UTC().Format(time.RFC3339), c.NotAfter.UTC().Format(time.RFC3339), state, ldapconn.Fingerprint(c))
	}
	fmt.Fprintf(stdout, "\nTo pin the server's own certificate, set in dolly.yaml (instead of ca_file):\n  tls_fingerprint: [\"%s\"]\n", ldapconn.Fingerprint(chain[0]))
	if *out != "" {
		f, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			fmt.Fprintf(stderr, "dolly fetch-cert: %v (an existing file is never overwritten)\n", err)
			return exitError
		}
		for _, c := range chain {
			err = errors.Join(err, pem.Encode(f, &pem.Block{Type: "CERTIFICATE", Bytes: c.Raw}))
		}
		if err = errors.Join(err, f.Close()); err != nil {
			fmt.Fprintf(stderr, "dolly fetch-cert: writing %s: %v\n", *out, err)
			return exitError
		}
		fmt.Fprintf(stdout, "Wrote %d certificates to %s (PEM; usable as ca_file).\n", len(chain), *out)
	}
	return exitOK
}
