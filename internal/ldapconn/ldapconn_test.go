package ldapconn

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
