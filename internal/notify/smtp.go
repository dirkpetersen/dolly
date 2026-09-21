// Package notify sends Dolly's email notifications: at most one plain-text
// mail per run, decided from the cn=status notes (see Decide), over SMTP
// with optional StartTLS and AUTH PLAIN.
//
// SMTP without authentication (username, password, and password_file all
// empty) is plain relay: Dolly never sends AUTH then, with or without
// StartTLS. AUTH is sent only when a username is set, and only over TLS
// (StartTLS or implicit TLS on port 465); config validation rejects the
// combination, and the client refuses it again before authenticating.
// Server certificates are verified against the system roots (or
// Options.RootCAs in tests); there is no way to skip verification.
package notify

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/dirkpetersen/dolly/internal/config"
)

// ImplicitTLSPort is the SMTP submission port with implicit TLS (SMTPS):
// the connection is TLS from the start, and start_tls must be off.
const ImplicitTLSPort = 465

// implicitTLSPort is ImplicitTLSPort; tests point it at a fake server.
var implicitTLSPort = ImplicitTLSPort

// Options are the parts of a send that don't come from the config.
type Options struct {
	// RootCAs verifies the server's certificate; nil uses the system roots.
	// Only tests set it (to trust a self-signed test certificate).
	RootCAs *x509.CertPool
	// Timeout bounds the connect and every SMTP step (network_timeout).
	Timeout time.Duration
	// Hostname is sent in EHLO and used in the Message-ID; default
	// os.Hostname.
	Hostname string
	// Now is the clock for the Date header (default time.Now).
	Now func() time.Time
	// Dial opens the TCP connection (default a net.Dialer with Timeout).
	// Tests use it to make sure nothing but a fake server is contacted.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

func (o Options) withDefaults() Options {
	if o.Timeout <= 0 {
		o.Timeout = 30 * time.Second
	}
	if o.Hostname == "" {
		o.Hostname, _ = os.Hostname()
		if o.Hostname == "" {
			o.Hostname = "localhost"
		}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Dial == nil {
		o.Dial = (&net.Dialer{Timeout: o.Timeout}).DialContext
	}
	return o
}

// dialGuard, if set, vets every SMTP connection before it is dialed,
// whatever Options.Dial is. It is nil in production; the package's tests
// set it to refuse anything but a loopback address, so no test can reach a
// real mail server.
var dialGuard func(network, addr string) error

// Message is one mail.
type Message struct {
	Subject string
	Body    string // plain text, "\n" line endings
}

// Step is one step of an SMTP session, for dolly check.
type Step struct {
	OK   bool
	Text string
}

// Send delivers m to notify.to through notify.smtp_host.
func Send(ctx context.Context, n config.Notify, o Options, m Message) error {
	o = o.withDefaults()
	data, from, to, err := Build(n, o, m)
	if err != nil {
		return err
	}
	_, err = session(ctx, n, o, func(c *smtp.Client, step func(string) error) error {
		if err := step("MAIL FROM"); err != nil {
			return err
		}
		if err := c.Mail(from); err != nil {
			return fmt.Errorf("MAIL FROM:<%s>: %w", from, err)
		}
		for _, r := range to {
			if err := c.Rcpt(r); err != nil {
				return fmt.Errorf("RCPT TO:<%s>: %w", r, err)
			}
		}
		if err := step("DATA"); err != nil {
			return err
		}
		w, err := c.Data()
		if err != nil {
			return fmt.Errorf("DATA: %w", err)
		}
		if _, err := w.Write(data); err != nil {
			w.Close()
			return fmt.Errorf("DATA: %w", err)
		}
		if err := w.Close(); err != nil {
			return fmt.Errorf("DATA: %w", err)
		}
		return nil
	})
	return err
}

// Probe connects, says EHLO, does StartTLS and AUTH as configured, and
// quits without sending anything. It returns the steps that succeeded and
// the first error.
func Probe(ctx context.Context, n config.Notify, o Options) ([]Step, error) {
	return session(ctx, n, o.withDefaults(), nil)
}

// session runs one SMTP session: connect (implicit TLS on port 465), EHLO,
// StartTLS if configured, AUTH PLAIN if a username is set, then fn (if
// any) and QUIT. Every step gets a fresh deadline of o.Timeout; ctx ending
// closes the connection.
func session(ctx context.Context, n config.Notify, o Options, fn func(c *smtp.Client, step func(string) error) error) ([]Step, error) {
	var steps []Step
	ok := func(format string, a ...any) { steps = append(steps, Step{OK: true, Text: fmt.Sprintf(format, a...)}) }
	if err := ctx.Err(); err != nil {
		return steps, err
	}
	host := n.SMTPHost
	addr := net.JoinHostPort(host, strconv.Itoa(n.SMTPPort))
	implicit := n.SMTPPort == implicitTLSPort
	if n.Username != "" && !n.StartTLS && !implicit {
		// Config validation rejects this too; never send a password in the clear.
		return steps, fmt.Errorf("refusing SMTP AUTH as %s over an unencrypted connection: set notify.start_tls: true (or use port %d, implicit TLS)", n.Username, ImplicitTLSPort)
	}
	tlsCfg := &tls.Config{ServerName: host, RootCAs: o.RootCAs, MinVersion: tls.VersionTLS12}

	if dialGuard != nil {
		if err := dialGuard("tcp", addr); err != nil {
			return steps, err
		}
	}
	conn, err := o.Dial(ctx, "tcp", addr)
	if err != nil {
		return steps, fmt.Errorf("connecting to %s: %w", addr, err)
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	defer conn.Close()
	step := func(what string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return conn.SetDeadline(time.Now().Add(o.Timeout))
	}
	wrap := func(err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	if err := step("connect"); err != nil {
		return steps, err
	}
	if implicit {
		tc := tls.Client(conn, tlsCfg)
		if err := tc.HandshakeContext(ctx); err != nil {
			return steps, wrap(fmt.Errorf("TLS handshake with %s: %w", addr, explainCert(err)))
		}
		conn = tc
	}
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		return steps, wrap(fmt.Errorf("connecting to %s: %w", addr, err))
	}
	defer c.Close()
	tlsNote := ""
	if implicit {
		tlsNote = " (implicit TLS)"
	}
	ok("connected to %s%s", addr, tlsNote)

	if err := step("EHLO"); err != nil {
		return steps, err
	}
	if err := c.Hello(o.Hostname); err != nil {
		return steps, wrap(fmt.Errorf("EHLO: %w", err))
	}
	ok("EHLO %s", o.Hostname)

	if n.StartTLS && !implicit {
		if has, _ := c.Extension("STARTTLS"); !has {
			return steps, fmt.Errorf("start_tls is set, but %s doesn't offer STARTTLS", addr)
		}
		if err := step("STARTTLS"); err != nil {
			return steps, err
		}
		if err := c.StartTLS(tlsCfg); err != nil {
			return steps, wrap(fmt.Errorf("STARTTLS: %w", explainCert(err)))
		}
		ok("STARTTLS (certificate verified for %s)", host)
	}

	if n.Username == "" {
		ok("no authentication (username empty)")
	} else {
		if _, isTLS := c.TLSConnectionState(); !isTLS {
			return steps, fmt.Errorf("refusing SMTP AUTH as %s: the connection is not encrypted", n.Username)
		}
		pw, err := n.SMTPPassword()
		if err != nil {
			return steps, fmt.Errorf("SMTP password: %w", err)
		}
		if pw == "" {
			return steps, errors.New("SMTP password is empty")
		}
		if has, mechs := c.Extension("AUTH"); !has || !hasWord(mechs, "PLAIN") {
			return steps, fmt.Errorf("%s doesn't offer AUTH PLAIN (offers %q)", addr, mechs)
		}
		if err := step("AUTH"); err != nil {
			return steps, err
		}
		if err := c.Auth(smtp.PlainAuth("", n.Username, pw, host)); err != nil {
			return steps, wrap(fmt.Errorf("AUTH PLAIN as %s: %w", n.Username, err))
		}
		ok("AUTH PLAIN as %s", n.Username)
	}

	if fn != nil {
		if err := fn(c, step); err != nil {
			return steps, wrap(err)
		}
	}
	if err := step("QUIT"); err != nil {
		return steps, err
	}
	if err := c.Quit(); err != nil {
		return steps, wrap(fmt.Errorf("QUIT: %w", err))
	}
	return steps, nil
}

func hasWord(list, w string) bool {
	for _, f := range strings.Fields(list) {
		if strings.EqualFold(f, w) {
			return true
		}
	}
	return false
}

func explainCert(err error) error {
	var ua x509.UnknownAuthorityError
	if errors.As(err, &ua) || strings.Contains(err.Error(), "certificate signed by unknown authority") {
		return fmt.Errorf("%w (the mail server's CA isn't in the system trust store; Dolly never skips certificate verification)", err)
	}
	return err
}

// Build returns the message as sent (headers and quoted-printable body,
// CRLF line endings), the envelope sender, and the recipients.
func Build(n config.Notify, o Options, m Message) (data []byte, from string, to []string, err error) {
	o = o.withDefaults()
	fromAddr, err := mail.ParseAddress(n.From)
	if err != nil {
		return nil, "", nil, fmt.Errorf("notify.from %q: %w", n.From, err)
	}
	var toHdr []string
	for _, r := range n.To {
		a, err := mail.ParseAddress(r)
		if err != nil {
			return nil, "", nil, fmt.Errorf("notify.to %q: %w", r, err)
		}
		to = append(to, a.Address)
		toHdr = append(toHdr, a.String())
	}
	if len(to) == 0 {
		return nil, "", nil, errors.New("notify.to: no recipients")
	}
	subject := m.Subject
	if n.SubjectPrefix != "" {
		subject = n.SubjectPrefix + " " + subject
	}
	now := o.Now()
	var b bytes.Buffer
	hdr := func(k, v string) { b.WriteString(k + ": " + v + "\r\n") }
	hdr("From", fromAddr.String())
	hdr("To", strings.Join(toHdr, ", "))
	hdr("Subject", mime.QEncoding.Encode("utf-8", subject))
	hdr("Date", now.Format(time.RFC1123Z))
	hdr("Message-ID", fmt.Sprintf("<dolly.%d.%d@%s>", now.UnixNano(), os.Getpid(), o.Hostname))
	hdr("MIME-Version", "1.0")
	hdr("Content-Type", "text/plain; charset=utf-8")
	hdr("Content-Transfer-Encoding", "quoted-printable")
	hdr("Auto-Submitted", "auto-generated")
	b.WriteString("\r\n")
	qp := quotedprintable.NewWriter(&b)
	body := strings.ReplaceAll(m.Body, "\r\n", "\n")
	if !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	if _, err := qp.Write([]byte(strings.ReplaceAll(body, "\n", "\r\n"))); err != nil {
		return nil, "", nil, err
	}
	if err := qp.Close(); err != nil {
		return nil, "", nil, err
	}
	return b.Bytes(), fromAddr.Address, to, nil
}
