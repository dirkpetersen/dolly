package notify

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dirkpetersen/dolly/internal/config"
)

// TestMain makes sure no test can reach a real mail server: every SMTP dial
// to a non-loopback address fails.
func TestMain(m *testing.M) {
	dialGuard = loopbackOnly
	os.Exit(m.Run())
}

func loopbackOnly(network, addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("test guard: no SMTP connection to %s: %v", addr, err)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("test guard: no SMTP connection to %s", addr)
	}
	return nil
}

// The guard refuses a non-loopback server before any dial, even with a
// custom Dial.
func TestDialGuard(t *testing.T) {
	dialed := false
	o := Options{Timeout: time.Second, Hostname: "dollyhost", Dial: func(context.Context, string, string) (net.Conn, error) {
		dialed = true
		return nil, fmt.Errorf("unreachable")
	}}
	for _, host := range []string{"mx.example.edu", "192.0.2.1", "localhost"} {
		n := config.Notify{SMTPHost: host, SMTPPort: 25, From: "a@example.edu", To: []string{"b@example.edu"}}
		err := Send(context.Background(), n, o, Message{Subject: "x", Body: "y\n"})
		if err == nil || !strings.Contains(err.Error(), "test guard") {
			t.Errorf("%s: got %v, want the test guard", host, err)
		}
	}
	if dialed {
		t.Error("Dial was called for a non-loopback address")
	}
	for _, addr := range []string{"127.0.0.1:25", "[::1]:25"} {
		if err := loopbackOnly("tcp", addr); err != nil {
			t.Errorf("%s: %v", addr, err)
		}
	}
}
