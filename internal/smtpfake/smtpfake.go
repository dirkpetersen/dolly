// Package smtpfake is an in-process SMTP server for tests of the
// notifications and dolly check: EHLO, STARTTLS and implicit TLS with a
// self-signed certificate for 127.0.0.1 (trust it through Pool, never by
// skipping verification), AUTH PLAIN, MAIL, RCPT, DATA, RSET, NOOP, and
// QUIT. It records every command and every accepted message. It never
// relays anything.
package smtpfake

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"fmt"
	"math/big"
	"net"
	"net/textproto"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Options configure the server.
type Options struct {
	StartTLS bool // offer STARTTLS
	Implicit bool // TLS from the first byte (SMTPS)
	// User and Password, if User is set, make the server offer AUTH PLAIN
	// (only after TLS) and accept only these credentials.
	User, Password string
	// RejectData makes DATA fail with 554.
	RejectData bool
}

// Message is one accepted mail.
type Message struct {
	From string
	To   []string
	Data string // as received, CRLF line endings, dot-unstuffed
	TLS  bool   // the session was encrypted
	Auth string // the authenticated user, "" if none
}

// Server is a running fake SMTP server.
type Server struct {
	Host string // "127.0.0.1"
	Port int
	opt  Options
	cert tls.Certificate
	pool *x509.CertPool
	ln   net.Listener

	mu       sync.Mutex
	commands []string
	messages []Message
}

// Start starts a server on 127.0.0.1 and stops it when the test ends.
func Start(t testing.TB, opt Options) *Server {
	t.Helper()
	cert, pool, err := selfSigned()
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Host: "127.0.0.1", Port: ln.Addr().(*net.TCPAddr).Port, opt: opt, cert: cert, pool: pool, ln: ln}
	go s.serve()
	t.Cleanup(func() { ln.Close() })
	return s
}

// Pool returns a root pool that trusts the server's certificate.
func (s *Server) Pool() *x509.CertPool { return s.pool }

// Commands returns every command received, in order ("EHLO host", "AUTH
// PLAIN", "MAIL FROM:<a>", ...). AUTH arguments are not recorded.
func (s *Server) Commands() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.commands...)
}

// Messages returns every accepted message.
func (s *Server) Messages() []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Message(nil), s.messages...)
}

// Received reports whether a command starting with verb was received.
func (s *Server) Received(verb string) bool {
	for _, c := range s.Commands() {
		if strings.HasPrefix(strings.ToUpper(c), strings.ToUpper(verb)) {
			return true
		}
	}
	return false
}

func (s *Server) serve() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(c)
	}
}

type session struct {
	conn    net.Conn
	tp      *textproto.Conn
	tls     bool
	auth    string
	from    string
	to      []string
	greeted bool
}

func (s *Server) handle(c net.Conn) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(30 * time.Second))
	ss := &session{conn: c}
	if s.opt.Implicit {
		tc := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{s.cert}})
		if err := tc.Handshake(); err != nil {
			return
		}
		ss.conn, ss.tls = tc, true
	}
	ss.tp = textproto.NewConn(ss.conn)
	reply := func(code int, lines ...string) {
		for i, l := range lines {
			sep := "-"
			if i == len(lines)-1 {
				sep = " "
			}
			_ = ss.tp.PrintfLine("%d%s%s", code, sep, l)
		}
	}
	reply(220, "smtpfake ready")
	for {
		line, err := ss.tp.ReadLine()
		if err != nil {
			return
		}
		verb, arg, _ := strings.Cut(line, " ")
		verb = strings.ToUpper(verb)
		rec := line
		if verb == "AUTH" {
			mech, _, _ := strings.Cut(arg, " ")
			rec = "AUTH " + mech
		}
		s.mu.Lock()
		s.commands = append(s.commands, rec)
		s.mu.Unlock()
		switch verb {
		case "EHLO", "HELO":
			ss.greeted = true
			ext := []string{"smtpfake"}
			if s.opt.StartTLS && !ss.tls {
				ext = append(ext, "STARTTLS")
			}
			if s.opt.User != "" && ss.tls {
				ext = append(ext, "AUTH PLAIN")
			}
			reply(250, ext...)
		case "STARTTLS":
			if !s.opt.StartTLS || ss.tls {
				reply(502, "not available")
				continue
			}
			reply(220, "go ahead")
			tc := tls.Server(ss.conn, &tls.Config{Certificates: []tls.Certificate{s.cert}})
			if err := tc.Handshake(); err != nil {
				return
			}
			ss.conn, ss.tls, ss.greeted = tc, true, false
			ss.tp = textproto.NewConn(tc)
		case "AUTH":
			s.auth(ss, arg, reply)
		case "MAIL":
			ss.from = strings.TrimSuffix(strings.TrimPrefix(arg, "FROM:<"), ">")
			ss.to = nil
			reply(250, "ok")
		case "RCPT":
			ss.to = append(ss.to, strings.TrimSuffix(strings.TrimPrefix(arg, "TO:<"), ">"))
			reply(250, "ok")
		case "DATA":
			if s.opt.RejectData {
				reply(554, "rejected by test")
				continue
			}
			reply(354, "end with .")
			data, err := ss.tp.ReadDotBytes()
			if err != nil {
				return
			}
			// ReadDotBytes turns CRLF into LF; restore the wire form.
			msg := Message{From: ss.from, To: ss.to, Data: strings.ReplaceAll(string(data), "\n", "\r\n"), TLS: ss.tls, Auth: ss.auth}
			s.mu.Lock()
			s.messages = append(s.messages, msg)
			s.mu.Unlock()
			reply(250, "queued")
		case "RSET", "NOOP":
			reply(250, "ok")
		case "QUIT":
			reply(221, "bye")
			return
		default:
			reply(502, "unknown command")
		}
	}
}

func (s *Server) auth(ss *session, arg string, reply func(int, ...string)) {
	mech, resp, _ := strings.Cut(arg, " ")
	switch {
	case s.opt.User == "" || !ss.tls:
		reply(502, "AUTH not available")
		return
	case !strings.EqualFold(mech, "PLAIN"):
		reply(504, "mechanism not supported")
		return
	}
	if resp == "" {
		reply(334, "")
		line, err := ss.tp.ReadLine()
		if err != nil {
			return
		}
		resp = line
	}
	raw, err := base64.StdEncoding.DecodeString(resp)
	parts := strings.Split(string(raw), "\x00")
	if err != nil || len(parts) != 3 || parts[1] != s.opt.User || parts[2] != s.opt.Password {
		reply(535, "authentication failed")
		return
	}
	ss.auth = parts[1]
	reply(235, "authenticated")
}

// Addr returns host:port.
func (s *Server) Addr() string { return net.JoinHostPort(s.Host, strconv.Itoa(s.Port)) }

func selfSigned() (tls.Certificate, *x509.CertPool, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "smtpfake"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:              []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("parsing the test certificate: %w", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, pool, nil
}
