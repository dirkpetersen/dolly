package ldapconn

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	ber "github.com/go-asn1-ber/asn1-ber"
	"github.com/go-ldap/ldap/v3"
)

func TestDialRefusesEmptyPassword(t *testing.T) {
	_, err := Dial(context.Background(), Options{URL: "ldaps://127.0.0.1:1", Timeout: time.Second, BindDN: "cn=x"})
	if err == nil || !strings.Contains(err.Error(), "password is empty") {
		t.Errorf("err = %v", err)
	}
}

func TestTLSConfig(t *testing.T) {
	c, err := TLSConfig("dc01.example.edu", "")
	if err != nil || c.ServerName != "dc01.example.edu" || c.InsecureSkipVerify || c.RootCAs == nil {
		t.Fatalf("config %+v, %v", c, err)
	}
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.pem")
	if err := os.WriteFile(bad, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := TLSConfig("h", bad); err == nil || !strings.Contains(err.Error(), "no PEM certificates") {
		t.Errorf("bad ca_file: %v", err)
	}
	if _, err := TLSConfig("h", filepath.Join(dir, "missing.pem")); err == nil {
		t.Error("missing ca_file must be an error")
	}
}

func TestDialConnectFailureIsReported(t *testing.T) {
	_, err := Dial(context.Background(), Options{URL: "ldap://127.0.0.1:1", StartTLS: true, Timeout: time.Second, BindDN: "cn=x", Password: "p"})
	if err == nil {
		t.Error("connecting to a closed port must fail")
	}
}

// stallServer listens on 127.0.0.1 and runs serve for each connection. The
// connections are closed when the test ends.
func stallServer(t *testing.T, serve func(net.Conn)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	t.Cleanup(func() {
		ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			c.Close()
		}
	})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
			go serve(c)
		}
	}()
	return ln.Addr().String()
}

// readRequest reads one LDAP request and returns its message ID and
// protocol op tag.
func readRequest(c net.Conn) (int64, ber.Tag, error) {
	p, err := ber.ReadPacket(c)
	if err != nil {
		return 0, 0, err
	}
	if len(p.Children) < 2 {
		return 0, 0, errors.New("short LDAP message")
	}
	id, _ := p.Children[0].Value.(int64)
	return id, p.Children[1].Tag, nil
}

// extendedOK encodes a successful ExtendedResponse (the StartTLS answer).
func extendedOK(id int64) []byte {
	p := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSequence, nil, "LDAP Response")
	p.AppendChild(ber.NewInteger(ber.ClassUniversal, ber.TypePrimitive, ber.TagInteger, id, "MessageID"))
	r := ber.Encode(ber.ClassApplication, ber.TypeConstructed, ldap.ApplicationExtendedResponse, nil, "Extended Response")
	r.AppendChild(ber.NewInteger(ber.ClassUniversal, ber.TypePrimitive, ber.TagEnumerated, int64(ldap.LDAPResultSuccess), "resultCode"))
	r.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, "", "matchedDN"))
	r.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, "", "diagnosticMessage"))
	p.AppendChild(r)
	return p.Bytes()
}

// block keeps a connection open without answering, until the peer or the
// test closes it.
func block(c net.Conn) { _, _ = io.Copy(io.Discard, c) }

// dialWithin dials o and fails the test unless Dial returns an error within
// about o.Timeout (a hang is caught by the outer limit).
func dialWithin(t *testing.T, o Options, want string) {
	t.Helper()
	o.BindDN, o.Password = "cn=x", "p"
	type result struct {
		err     error
		elapsed time.Duration
	}
	done := make(chan result, 1)
	go func() {
		start := time.Now()
		c, err := Dial(context.Background(), o)
		if c != nil {
			c.Close()
		}
		done <- result{err, time.Since(start)}
	}()
	select {
	case r := <-done:
		if r.err == nil {
			t.Fatal("Dial succeeded against a stalled server")
		}
		if !strings.Contains(r.err.Error(), want) {
			t.Errorf("err = %v, want it to mention %q", r.err, want)
		}
		if r.elapsed > o.Timeout+2*time.Second {
			t.Errorf("Dial took %s with a %s timeout", r.elapsed, o.Timeout)
		}
	case <-time.After(o.Timeout + 10*time.Second):
		t.Fatal("Dial hung past network_timeout")
	}
}

func TestDialTimesOutWhenStartTLSGetsNoAnswer(t *testing.T) {
	addr := stallServer(t, func(c net.Conn) {
		if _, _, err := readRequest(c); err != nil {
			return
		}
		block(c)
	})
	dialWithin(t, Options{URL: "ldap://" + addr, StartTLS: true, Timeout: 500 * time.Millisecond}, "StartTLS")
}

func TestDialTimesOutWhenTheStartTLSHandshakeStalls(t *testing.T) {
	addr := stallServer(t, func(c net.Conn) {
		id, _, err := readRequest(c)
		if err != nil {
			return
		}
		if _, err := c.Write(extendedOK(id)); err != nil {
			return
		}
		block(c) // read the ClientHello and never answer it
	})
	dialWithin(t, Options{URL: "ldap://" + addr, StartTLS: true, Timeout: 500 * time.Millisecond}, "TLS handshake")
}

func TestDialTimesOutWhenTheLDAPSHandshakeStalls(t *testing.T) {
	addr := stallServer(t, block)
	dialWithin(t, Options{URL: "ldaps://" + addr, Timeout: 500 * time.Millisecond}, "TLS handshake")
}

func TestDialTimesOutWhenTheBindGetsNoAnswer(t *testing.T) {
	addr := stallServer(t, func(c net.Conn) {
		if _, _, err := readRequest(c); err != nil {
			return
		}
		block(c)
	})
	dialWithin(t, Options{URL: "ldap://" + addr, Timeout: 500 * time.Millisecond}, "bind as cn=x")
}

// A bind that succeeds must leave no deadline on the socket: an operation
// after network_timeout has passed still works.
func TestDialClearsTheSetupDeadline(t *testing.T) {
	addr := stallServer(t, func(c net.Conn) {
		for {
			id, tag, err := readRequest(c)
			if err != nil {
				return
			}
			var op ber.Tag
			switch tag {
			case ldap.ApplicationBindRequest:
				op = ldap.ApplicationBindResponse
			case ldap.ApplicationExtendedRequest:
				op = ldap.ApplicationExtendedResponse
			default:
				return
			}
			p := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSequence, nil, "LDAP Response")
			p.AppendChild(ber.NewInteger(ber.ClassUniversal, ber.TypePrimitive, ber.TagInteger, id, "MessageID"))
			r := ber.Encode(ber.ClassApplication, ber.TypeConstructed, op, nil, "Response")
			r.AppendChild(ber.NewInteger(ber.ClassUniversal, ber.TypePrimitive, ber.TagEnumerated, int64(ldap.LDAPResultSuccess), "resultCode"))
			r.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, "", "matchedDN"))
			r.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, "", "diagnosticMessage"))
			p.AppendChild(r)
			if _, err := c.Write(p.Bytes()); err != nil {
				return
			}
		}
	})
	timeout := 300 * time.Millisecond
	conn, err := Dial(context.Background(), Options{URL: "ldap://" + addr, Timeout: timeout, BindDN: "cn=x", Password: "p"})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	time.Sleep(2 * timeout)
	if err := conn.Bind("cn=x", "p"); err != nil {
		t.Errorf("a bind after the setup deadline failed: %v", err)
	}
}
