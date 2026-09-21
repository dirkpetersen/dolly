package notify

import (
	"bytes"
	"context"
	"io"
	"mime"
	"mime/quotedprintable"
	"net/mail"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dirkpetersen/dolly/internal/config"
	"github.com/dirkpetersen/dolly/internal/planner"
	"github.com/dirkpetersen/dolly/internal/smtpfake"
	"github.com/dirkpetersen/dolly/internal/target"
)

var t0 = time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)

func ts(d time.Duration) string { return t0.Add(d).Format(time.RFC3339) }

func TestDecide(t *testing.T) {
	failing := []string{"failure-since=" + ts(-2*time.Hour), "failure=AD down", "last-notified=" + ts(-2*time.Hour)}
	for _, tc := range []struct {
		name   string
		on     string
		before []string
		out    Outcome
		want   []Reason // nil: no mail
	}{
		{"healthy, no changes", "failure", nil, Outcome{}, nil},
		{"healthy, changes, on failure", "failure", nil, Outcome{Changed: true}, nil},
		{"new failure", "failure", nil, Outcome{Failure: "AD down"}, []Reason{ReasonFailure}},
		{"new failure, on changes", "changes", nil, Outcome{Failure: "AD down"}, []Reason{ReasonFailure}},
		{"persisting, no reminder yet", "failure", failing, Outcome{Failure: "AD down"}, nil},
		{"persisting, reminder due", "failure", []string{"failure-since=" + ts(-30*time.Hour), "last-notified=" + ts(-24*time.Hour)}, Outcome{Failure: "AD down"}, []Reason{ReasonReminder}},
		{"persisting, reminder just short", "failure", []string{"failure-since=" + ts(-30*time.Hour), "last-notified=" + ts(-24*time.Hour+time.Second)}, Outcome{Failure: "AD down"}, nil},
		{"persisting, never mailed (send failed)", "failure", []string{"failure-since=" + ts(-time.Hour)}, Outcome{Failure: "AD down"}, []Reason{ReasonFailure}},
		{"persisting, mailed before this failure", "failure", []string{"failure-since=" + ts(-time.Hour), "last-notified=" + ts(-48*time.Hour)}, Outcome{Failure: "AD down"}, []Reason{ReasonFailure}},
		{"recovered", "failure", failing, Outcome{}, []Reason{ReasonRecovered}},
		{"recovered with changes, on changes", "changes", failing, Outcome{Changed: true}, []Reason{ReasonRecovered, ReasonChanges}},
		{"changes, on changes", "changes", nil, Outcome{Changed: true}, []Reason{ReasonChanges}},
		{"no changes, on changes", "changes", nil, Outcome{}, nil},
		{"always", "always", nil, Outcome{}, []Reason{ReasonAlways}},
		{"always with changes", "always", nil, Outcome{Changed: true}, []Reason{ReasonChanges, ReasonAlways}},
		{"held lock never mails", "always", failing, Outcome{Held: true, Failure: "x", Changed: true, BrokeLock: true}, nil},
		{"stale lock broken", "failure", nil, Outcome{BrokeLock: true}, []Reason{ReasonStaleLock}},
		{"warnings appeared", "failure", nil, Outcome{WarningsKnown: true, WarningsHash: "abc", WarningsKey: "warnings-hash"}, []Reason{ReasonWarnings}},
		{"warnings unchanged", "failure", []string{"warnings-hash=abc"}, Outcome{WarningsKnown: true, WarningsHash: "abc", WarningsKey: "warnings-hash"}, nil},
		{"warnings changed", "failure", []string{"warnings-hash=abc"}, Outcome{WarningsKnown: true, WarningsHash: "def", WarningsKey: "warnings-hash"}, []Reason{ReasonWarnings}},
		{"warnings cleared", "failure", []string{"warnings-hash=abc"}, Outcome{WarningsKnown: true, WarningsKey: "warnings-hash"}, []Reason{ReasonWarnings}},
		{"warnings unknown (read failed)", "failure", append([]string{"warnings-hash=abc"}, failing...), Outcome{Failure: "AD down", WarningsKey: "warnings-hash"}, nil},
		{"other scope's hash ignored", "failure", []string{"warnings-hash-groups=abc"}, Outcome{WarningsKnown: true, WarningsHash: "abc", WarningsKey: "warnings-hash-groups"}, nil},
		{"failure changed", "failure", failing, Outcome{Failure: "LDAP down"}, []Reason{ReasonFailureChanged}},
		{"failure changed, on changes with changes", "changes", failing, Outcome{Failure: "LDAP down", Changed: true}, []Reason{ReasonFailureChanged, ReasonChanges}},
		{"only counts differ: same failure", "failure", []string{"failure-since=" + ts(-2*time.Hour), "failure=run incomplete: 10 operations: 3 applied, 1 failed",
			"failure-notified=" + FailureNotifiedNote(t0.Add(-2*time.Hour), "run incomplete: 12 operations: 5 applied, 2 failed")},
			Outcome{Failure: "run incomplete: 9 operations: 0 applied, 4 failed"}, nil},
		{"guard counts differ: same failure", "failure", []string{"failure-since=" + ts(-2*time.Hour), "failure=x",
			"failure-notified=" + FailureNotifiedNote(t0.Add(-2*time.Hour), "mass-deletion guard tripped: 40 user removals exceed max_delete_min (10) and 5% of 300 owned")},
			Outcome{Failure: "mass-deletion guard tripped: 41 user removals exceed max_delete_min (10) and 5% of 290 owned"}, nil},
		{"flapping failure: changed text within the minimum gap is not mailed", "failure", []string{"failure-since=" + ts(-3*time.Hour), "failure=AD down",
			"failure-notified=" + FailureNotifiedNote(t0.Add(-15*time.Minute), "AD down")},
			Outcome{Failure: "LDAP down"}, nil},
		{"flapping failure: changed text after the minimum gap is mailed", "failure", []string{"failure-since=" + ts(-3*time.Hour), "failure=AD down",
			"failure-notified=" + FailureNotifiedNote(t0.Add(-61*time.Minute), "AD down")},
			Outcome{Failure: "LDAP down"}, []Reason{ReasonFailureChanged}},
		{"compared with the failure last mailed, not the last run's", "failure", []string{"failure-since=" + ts(-2*time.Hour), "failure=LDAP down",
			"failure-notified=" + FailureNotifiedNote(t0.Add(-2*time.Hour), "AD down")},
			Outcome{Failure: "LDAP down"}, []Reason{ReasonFailureChanged}},
		{"a changes mail during a failure doesn't reset the reminder", "changes", []string{"failure-since=" + ts(-30*time.Hour), "failure=AD down",
			"failure-notified=" + FailureNotifiedNote(t0.Add(-24*time.Hour), "AD down"), "last-notified=" + ts(-time.Hour)},
			Outcome{Failure: "AD down", Changed: true}, []Reason{ReasonReminder, ReasonChanges}},
		{"failure mailed recently, no reminder", "failure", []string{"failure-since=" + ts(-30*time.Hour), "failure=AD down",
			"failure-notified=" + FailureNotifiedNote(t0.Add(-time.Hour), "AD down"), "last-notified=" + ts(-48*time.Hour)},
			Outcome{Failure: "AD down"}, nil},
		{"failure mail predates this failure", "failure", []string{"failure-since=" + ts(-time.Hour), "failure=AD down",
			"failure-notified=" + FailureNotifiedNote(t0.Add(-48*time.Hour), "AD down"), "last-notified=" + ts(-time.Minute)},
			Outcome{Failure: "AD down"}, []Reason{ReasonFailure}},
		{"guard trip is a failure", "failure", nil, Outcome{Failure: "mass-deletion guard tripped"}, []Reason{ReasonFailure}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.out.Now = t0
			d := Decide(tc.on, 24*time.Hour, tc.before, tc.out)
			if d.Send != (tc.want != nil) || strings.Join(reasons(d.Reasons), ",") != strings.Join(reasons(tc.want), ",") {
				t.Errorf("got send=%v %v, want %v", d.Send, d.Reasons, tc.want)
			}
		})
	}
}

func TestNormalizeFailure(t *testing.T) {
	for _, tc := range []struct{ a, b string }{
		{"run incomplete: 10 operations: 3 applied, 1 failed", "run incomplete:  12 operations: 5 applied, 0 failed"},
		{"stopped by run_timeout after 25m0s", "stopped by run_timeout after 24m59.5s"},
		{"lock taken at 2026-09-21T12:00:00Z (3 runs)", "lock taken at 2026-09-22T01:02:03Z (14 runs)"},
		{"guard: 40 removals exceed 5% of 300", "guard: 41 removals exceed 7% of 290"},
	} {
		if NormalizeFailure(tc.a) != NormalizeFailure(tc.b) || FailureHash(tc.a) != FailureHash(tc.b) {
			t.Errorf("%q and %q should normalize alike: %q vs %q", tc.a, tc.b, NormalizeFailure(tc.a), NormalizeFailure(tc.b))
		}
	}
	for _, tc := range []struct{ a, b string }{
		{"AD down", "LDAP down"},
		{"connecting to dc01.example.edu:636: refused", "connecting to dc02.example.edu:636: refused"},
		{"dial ldap://10.0.0.1:389 failed", "dial ldap://10.0.0.2:389 failed"},
		{"reading AD: sizeLimitExceeded", "reading target: sizeLimitExceeded"},
	} {
		if FailureHash(tc.a) == FailureHash(tc.b) {
			t.Errorf("%q and %q must differ, both normalize to %q", tc.a, tc.b, NormalizeFailure(tc.a))
		}
	}
}

func reasons(rs []Reason) []string {
	var out []string
	for _, r := range rs {
		out = append(out, string(r))
	}
	return out
}

func TestWarningsHash(t *testing.T) {
	a := planner.Warning{Kind: planner.WarnConflict, Subject: "cn=x", Message: "no record"}
	b := planner.Warning{Kind: planner.WarnIgnored, Subject: "CN=y", Message: "missing gidNumber"}
	id := planner.Warning{Kind: planner.WarnIDChanged, Subject: "uid=z", Message: "uidNumber 1 -> 2"}
	if WarningsHash(nil) != "" || WarningsHash([]planner.Warning{id}) != "" {
		t.Error("no standing warnings must hash to empty")
	}
	if WarningsHash([]planner.Warning{a, b}) != WarningsHash([]planner.Warning{b, id, a}) {
		t.Error("the hash must ignore order and id-changed warnings")
	}
	if WarningsHash([]planner.Warning{a}) == WarningsHash([]planner.Warning{a, b}) {
		t.Error("a new warning must change the hash")
	}
	if WarningsKey("sync", true, true) != "warnings-hash" || WarningsKey("sync", false, true) != "warnings-hash-groups" ||
		WarningsKey("sync", true, false) != "warnings-hash-users" || WarningsKey("adopt", false, false) != "warnings-hash-adopt" {
		t.Error("warnings keys")
	}
}

func notifyCfg(s *smtpfake.Server) config.Notify {
	return config.Notify{
		SMTPHost: s.Host, SMTPPort: s.Port, From: "Dolly <dolly@example.edu>", To: []string{"admins@example.edu", "Ops <ops@example.edu>"},
		SubjectPrefix: "[dolly]", On: "failure", RemindEvery: config.Duration{Duration: 24 * time.Hour},
	}
}

func opts(s *smtpfake.Server) Options {
	return Options{RootCAs: s.Pool(), Timeout: 5 * time.Second, Hostname: "dollyhost", Now: func() time.Time { return t0 }}
}

// SMTP without a username never sends AUTH, with or without StartTLS.
func TestSendWithoutAuth(t *testing.T) {
	for _, startTLS := range []bool{false, true} {
		s := smtpfake.Start(t, smtpfake.Options{StartTLS: true})
		n := notifyCfg(s)
		n.StartTLS = startTLS
		if err := Send(context.Background(), n, opts(s), Message{Subject: "hi", Body: "body\n"}); err != nil {
			t.Fatalf("start_tls=%v: %v", startTLS, err)
		}
		msgs := s.Messages()
		if len(msgs) != 1 || msgs[0].TLS != startTLS || msgs[0].Auth != "" {
			t.Fatalf("start_tls=%v: messages %+v", startTLS, msgs)
		}
		if s.Received("AUTH") {
			t.Errorf("start_tls=%v: AUTH was sent without a username: %v", startTLS, s.Commands())
		}
		if s.Received("STARTTLS") != startTLS {
			t.Errorf("start_tls=%v: commands %v", startTLS, s.Commands())
		}
		if got := strings.Join(msgs[0].To, ","); got != "admins@example.edu,ops@example.edu" || msgs[0].From != "dolly@example.edu" {
			t.Errorf("envelope %q -> %q", msgs[0].From, got)
		}
	}
}

func TestSendWithAuth(t *testing.T) {
	s := smtpfake.Start(t, smtpfake.Options{StartTLS: true, User: "dolly", Password: "s3cret"})
	n := notifyCfg(s)
	n.StartTLS, n.Username = true, "dolly"
	n.PasswordFile = filepath.Join(t.TempDir(), "smtp.secret")
	if err := os.WriteFile(n.PasswordFile, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Send(context.Background(), n, opts(s), Message{Subject: "hi", Body: "b"}); err != nil {
		t.Fatal(err)
	}
	if m := s.Messages(); len(m) != 1 || m[0].Auth != "dolly" || !m[0].TLS {
		t.Fatalf("messages %+v", m)
	}
	steps, err := Probe(context.Background(), n, opts(s))
	if err != nil || len(steps) != 4 || !strings.Contains(steps[3].Text, "AUTH PLAIN as dolly") {
		t.Errorf("probe: %v %+v", err, steps)
	}
	if len(s.Messages()) != 1 {
		t.Error("Probe must not send mail")
	}

	// Wrong password: the server's rejection is the error.
	n.PasswordFile = ""
	n.Password = "wrong"
	if err := Send(context.Background(), n, opts(s), Message{Subject: "hi", Body: "b"}); err == nil || !strings.Contains(err.Error(), "AUTH PLAIN as dolly") {
		t.Errorf("wrong password: %v", err)
	}
}

func TestSendImplicitTLS(t *testing.T) {
	s := smtpfake.Start(t, smtpfake.Options{Implicit: true, User: "dolly", Password: "pw"})
	old := implicitTLSPort
	implicitTLSPort = s.Port
	defer func() { implicitTLSPort = old }()
	n := notifyCfg(s)
	n.Username, n.Password = "dolly", "pw"
	if err := Send(context.Background(), n, opts(s), Message{Subject: "hi", Body: "b"}); err != nil {
		t.Fatal(err)
	}
	if m := s.Messages(); len(m) != 1 || m[0].Auth != "dolly" || !m[0].TLS || s.Received("STARTTLS") {
		t.Fatalf("messages %+v, commands %v", m, s.Commands())
	}
}

// AUTH is never sent over an unencrypted connection.
func TestAuthRefusedWithoutTLS(t *testing.T) {
	s := smtpfake.Start(t, smtpfake.Options{StartTLS: true, User: "dolly", Password: "pw"})
	n := notifyCfg(s)
	n.Username, n.Password = "dolly", "pw" // start_tls false, not port 465
	err := Send(context.Background(), n, opts(s), Message{Subject: "hi", Body: "b"})
	if err == nil || !strings.Contains(err.Error(), "refusing SMTP AUTH as dolly over an unencrypted connection") {
		t.Fatalf("err = %v", err)
	}
	if s.Received("AUTH") || len(s.Messages()) != 0 {
		t.Errorf("commands %v", s.Commands())
	}
}

func TestStartTLSRequiredAndVerified(t *testing.T) {
	s := smtpfake.Start(t, smtpfake.Options{})
	n := notifyCfg(s)
	n.StartTLS = true
	if err := Send(context.Background(), n, opts(s), Message{Subject: "hi"}); err == nil || !strings.Contains(err.Error(), "doesn't offer STARTTLS") {
		t.Errorf("no STARTTLS offered: %v", err)
	}
	s2 := smtpfake.Start(t, smtpfake.Options{StartTLS: true})
	n2 := notifyCfg(s2)
	n2.StartTLS = true
	o := opts(s2)
	o.RootCAs = nil // system roots don't trust the test certificate
	if err := Send(context.Background(), n2, o, Message{Subject: "hi"}); err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Errorf("untrusted certificate: %v", err)
	}
	if len(s2.Messages()) != 0 {
		t.Error("mail sent over an unverified connection")
	}
}

func TestBuildHeadersAndBody(t *testing.T) {
	n := config.Notify{From: "Dolly <dolly@example.edu>", To: []string{"a@example.edu"}, SubjectPrefix: "[dolly]"}
	data, from, to, err := Build(n, Options{Hostname: "h", Now: func() time.Time { return t0 }}, Message{Subject: "Grüße", Body: "line one\nJörg = x\n.dot line\n"})
	if err != nil {
		t.Fatal(err)
	}
	if from != "dolly@example.edu" || len(to) != 1 || to[0] != "a@example.edu" {
		t.Errorf("envelope %q %v", from, to)
	}
	m, err := mail.ReadMessage(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	subj, _ := new(mime.WordDecoder).DecodeHeader(m.Header.Get("Subject"))
	if subj != "[dolly] Grüße" || m.Header.Get("Content-Type") != "text/plain; charset=utf-8" || m.Header.Get("Date") == "" ||
		!strings.HasSuffix(m.Header.Get("Message-ID"), "@h>") || m.Header.Get("To") != "<a@example.edu>" {
		t.Errorf("headers %v (subject %q)", m.Header, subj)
	}
	body, _ := io.ReadAll(quotedprintable.NewReader(m.Body))
	if string(body) != "line one\r\nJörg = x\r\n.dot line\r\n" {
		t.Errorf("body %q", body)
	}
}

func TestCapLines(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 2500; i++ {
		b.WriteString("x\n")
	}
	got := capLines(b.String(), MaxBodyLines)
	if strings.Count(got, "x\n") != MaxBodyLines || !strings.Contains(got, "[... 500 more lines not shown") {
		t.Errorf("cap: %d lines", strings.Count(got, "\n"))
	}
	if capLines("a\nb\n", 5) != "a\nb\n" {
		t.Error("short bodies must be unchanged")
	}
}

// Notes end to end: one mail with the run's output, and the notes that
// record it; a failed send records notify-error and nothing else.
func TestNotes(t *testing.T) {
	s := smtpfake.Start(t, smtpfake.Options{StartTLS: true})
	n := notifyCfg(s)
	n.StartTLS = true
	p := &planner.Plan{Mode: "sync", Users: true, Groups: true, Warnings: []planner.Warning{{Kind: planner.WarnConflict, Subject: "cn=x,ou=group,dc=local", Message: "no record"}}}
	var out strings.Builder
	for i := 0; i < 100; i++ {
		out.WriteString("  + uid=u" + string(rune('a'+i%26)) + ",ou=people,dc=local\n")
	}
	r := Report{Command: "sync", Host: "dollyhost", End: t0, Failure: "run incomplete: 1 failed", Changed: true, Plan: p, Output: out.String()}
	var log bytes.Buffer
	notes := Notes(context.Background(), n, opts(s), nil, r, &log)
	msgs := s.Messages()
	if len(msgs) != 1 {
		t.Fatalf("%d messages, log %q", len(msgs), log.String())
	}
	m, _ := mail.ReadMessage(strings.NewReader(msgs[0].Data))
	body, _ := io.ReadAll(quotedprintable.NewReader(m.Body))
	if subj := m.Header.Get("Subject"); subj != "[dolly] dollyhost dolly sync FAILED: run incomplete: 1 failed" {
		t.Errorf("subject %q", subj)
	}
	for _, want := range []string{"Outcome:  FAILED", "Failure: run incomplete: 1 failed", "a failure appeared", "list of warnings", "+ uid=ua,ou=people,dc=local"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("body lacks %q:\n%s", want, body)
		}
	}
	if notes[target.StatusLastNotified] != ts(0) || notes["warnings-hash"] != WarningsHash(p.Warnings) || notes[target.StatusNotifyError] != "" {
		t.Errorf("notes %v", notes)
	}
	if _, ok := notes[target.StatusNotifyError]; !ok {
		t.Error("a successful send must clear notify-error")
	}
	if notes[target.StatusFailureNotified] != FailureNotifiedNote(t0, r.Failure) {
		t.Errorf("a failure mail must set failure-notified: %v", notes)
	}

	// A persisting failure that changed is mailed at once and restarts
	// the failure clock.
	before := []string{"failure-since=" + ts(-time.Hour), "failure=AD down", "failure-notified=" + FailureNotifiedNote(t0.Add(-time.Hour), "AD down")}
	notes = Notes(context.Background(), n, opts(s), before, r, &log)
	msgs = s.Messages()
	if len(msgs) != 2 {
		t.Fatalf("%d messages, log %q", len(msgs), log.String())
	}
	m, _ = mail.ReadMessage(strings.NewReader(msgs[1].Data))
	body, _ = io.ReadAll(quotedprintable.NewReader(m.Body))
	if subj := m.Header.Get("Subject"); subj != "[dolly] dollyhost dolly sync FAILED (failure changed): run incomplete: 1 failed" {
		t.Errorf("subject %q", subj)
	}
	if !strings.Contains(string(body), "failure changed") || !strings.Contains(string(body), "Previous failure: AD down") {
		t.Errorf("body:\n%s", body)
	}
	if notes[target.StatusFailureNotified] != FailureNotifiedNote(t0, r.Failure) {
		t.Errorf("a failure-changed mail must set failure-notified: %v", notes)
	}

	// A changes mail during the same failure leaves failure-notified alone.
	nc := n
	nc.On = "changes"
	before = []string{"failure-since=" + ts(-time.Hour), "failure=" + r.Failure, "failure-notified=" + FailureNotifiedNote(t0.Add(-time.Hour), r.Failure),
		"warnings-hash=" + WarningsHash(p.Warnings)}
	notes = Notes(context.Background(), nc, opts(s), before, r, &log)
	if len(s.Messages()) != 3 {
		t.Fatalf("%d messages, log %q", len(s.Messages()), log.String())
	}
	if _, ok := notes[target.StatusFailureNotified]; ok || notes[target.StatusLastNotified] != ts(0) {
		t.Errorf("a changes-only mail must set last-notified but not failure-notified: %v", notes)
	}

	// Disabled: nothing at all.
	n2 := n
	n2.SMTPHost = ""
	if Notes(context.Background(), n2, opts(s), nil, r, &log) != nil || len(s.Messages()) != 3 {
		t.Error("an empty smtp_host must disable notifications")
	}

	// A failed send: notify-error only, logged, never last-notified.
	bad := smtpfake.Start(t, smtpfake.Options{RejectData: true})
	log.Reset()
	notes = Notes(context.Background(), notifyCfg(bad), opts(bad), nil, r, &log)
	if len(notes) != 1 || !strings.Contains(notes[target.StatusNotifyError], "rejected by test") || !strings.Contains(log.String(), "sending the notification") {
		t.Errorf("notes %v, log %q", notes, log.String())
	}
}
