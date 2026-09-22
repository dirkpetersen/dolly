package target

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/go-ldap/ldap/v3"

	"github.com/dirkpetersen/dolly/internal/ldapfake"
)

var lockDN = LockDN(state)

func lockOpts(log *bytes.Buffer, pid int) LockOptions {
	return LockOptions{StateBase: state, TTL: time.Hour, Command: "sync", Host: "h1", PID: pid, Log: log}
}

func withStateBase() *ldapfake.Dir {
	dir := ldapfake.New()
	dir.Put(state, map[string][]string{"objectClass": {"organizationalUnit"}, "ou": {"dolly"}}, time.Now())
	return dir
}

func putLock(dir *ldapfake.Dir, holder string, age time.Duration) {
	dir.Put(lockDN, map[string][]string{"objectClass": {"organizationalRole"}, "cn": {"lock"}, "description": {"host=" + holder}},
		time.Now().Add(-age))
}

func TestLockAcquireRelease(t *testing.T) {
	dir := withStateBase()
	var log bytes.Buffer
	l, held, err := AcquireLock(context.Background(), dir, lockOpts(&log, 7))
	if err != nil || held != nil || l == nil {
		t.Fatalf("acquire: %v %v", held, err)
	}
	desc := strings.Join(dir.Values(lockDN, "description"), " ")
	for _, want := range []string{"host=h1", "pid=7", "started=", "command=dolly sync"} {
		if !strings.Contains(desc, want) {
			t.Errorf("lock description %q lacks %q", desc, want)
		}
	}
	// A second run finds it held and fresh.
	l2, held, err := AcquireLock(context.Background(), dir, lockOpts(&log, 8))
	if err != nil || l2 != nil || held == nil || !strings.Contains(held.HolderString(), "pid=7") {
		t.Fatalf("second acquire: %v %v %v", l2, held, err)
	}
	if err := l.Release(context.Background()); err != nil || dir.Has(lockDN) {
		t.Fatalf("release: %v", err)
	}
	if err := l.Release(context.Background()); err != nil {
		t.Errorf("second release: %v", err)
	}
}

// createTimestamp must be requested explicitly: it is operational.
func TestLockReadsCreateTimestamp(t *testing.T) {
	dir := withStateBase()
	putLock(dir, "other", 10*time.Minute)
	var seen []string
	dir.Fail = func(op, dn string, req any) error {
		if s, ok := req.(*ldap.SearchRequest); ok && op == "search" && dn == lockDN {
			seen = s.Attributes
		}
		return nil
	}
	info, err := ReadLock(dir, lockDN)
	if err != nil || info == nil {
		t.Fatal(err)
	}
	if strings.Join(seen, ",") != "description,createTimestamp" {
		t.Errorf("requested %v", seen)
	}
	if age := info.Age(time.Now()); age < 9*time.Minute || age > 11*time.Minute {
		t.Errorf("age %s", age)
	}
}

func TestLockStale(t *testing.T) {
	t.Run("rename won", func(t *testing.T) {
		dir := withStateBase()
		putLock(dir, "crashed", 2*time.Hour)
		var log bytes.Buffer
		l, held, err := AcquireLock(context.Background(), dir, lockOpts(&log, 7))
		if err != nil || held != nil || l == nil || l.Broke == nil {
			t.Fatalf("acquire: %v %v %v", l, held, err)
		}
		if !strings.Contains(log.String(), "warning: broke a stale run lock") || !strings.Contains(log.String(), "host=crashed") {
			t.Errorf("log: %s", log.String())
		}
		if got := dir.DNs(); len(got) != 2 { // state_base and our lock; the renamed one is deleted
			t.Errorf("entries: %v", got)
		}
		if !strings.Contains(strings.Join(dir.Values(lockDN, "description"), " "), "host=h1") {
			t.Error("the new lock isn't ours")
		}
		var renamed bool
		for _, c := range dir.Calls {
			if c == "modifydn "+lockDN {
				renamed = true
			}
		}
		if !renamed {
			t.Errorf("stale lock not broken by modrdn: %v", dir.Calls)
		}
	})
	t.Run("rename lost", func(t *testing.T) {
		dir := withStateBase()
		putLock(dir, "crashed", 2*time.Hour)
		// Another host renames it just before us.
		dir.Before = func(op, dn string) {
			if op == "modifydn" {
				dir.Before = nil
				_ = dir.ModifyDN(ldap.NewModifyDNRequest(lockDN, "cn=lock-stale-other", true, ""))
			}
		}
		var log bytes.Buffer
		l, held, err := AcquireLock(context.Background(), dir, lockOpts(&log, 7))
		if err != nil || l != nil || held == nil {
			t.Fatalf("acquire: %v %v %v", l, held, err)
		}
		if dir.Has(lockDN) {
			t.Error("the loser took the lock")
		}
		if strings.Contains(log.String(), "broke") {
			t.Errorf("the loser claims it broke the lock: %s", log.String())
		}
	})
	t.Run("fresh lock renamed by mistake", func(t *testing.T) {
		// Between our read (stale) and our rename, another host broke the
		// stale lock and took a fresh one: we rename the fresh one back.
		dir := withStateBase()
		putLock(dir, "crashed", 2*time.Hour)
		dir.Before = func(op, dn string) {
			if op == "modifydn" {
				dir.Before = nil
				_ = dir.Del(ldap.NewDelRequest(lockDN, nil))
				putLock(dir, "winner", 0)
			}
		}
		l, held, err := AcquireLock(context.Background(), dir, lockOpts(&bytes.Buffer{}, 7))
		if err != nil || l != nil || held == nil {
			t.Fatalf("acquire: %v %v %v", l, held, err)
		}
		if got := dir.Values(lockDN, "description"); len(got) != 1 || got[0] != "host=winner" {
			t.Errorf("winner's lock: %v (entries %v)", got, dir.DNs())
		}
	})
}

// Release uses the context it is given, not the run's: a cancelled run
// still releases its lock. It never deletes another run's lock.
func TestLockReleaseAfterCancel(t *testing.T) {
	dir := withStateBase()
	runCtx, cancel := context.WithCancel(context.Background())
	l, _, err := AcquireLock(runCtx, dir, lockOpts(&bytes.Buffer{}, 7))
	if err != nil || l == nil {
		t.Fatal(err)
	}
	cancel() // SIGTERM
	fresh, done := context.WithTimeout(context.Background(), time.Second)
	defer done()
	if err := l.Release(fresh); err != nil || dir.Has(lockDN) {
		t.Fatalf("release after cancel: %v", err)
	}

	l, _, _ = AcquireLock(context.Background(), dir, lockOpts(&bytes.Buffer{}, 7))
	_ = dir.Del(ldap.NewDelRequest(lockDN, nil))
	putLock(dir, "someone-else", 0)
	if err := l.Release(context.Background()); err == nil || !strings.Contains(err.Error(), "now held by host=someone-else") {
		t.Errorf("release of a lock that isn't ours: %v", err)
	}
	if !dir.Has(lockDN) {
		t.Error("deleted another run's lock")
	}
}

func TestEnsureStateBase(t *testing.T) {
	dir := ldapfake.New()
	created, err := EnsureStateBase(context.Background(), dir, state)
	if err != nil || !created || strings.Join(dir.Values(state, "objectClass"), "") != "organizationalUnit" {
		t.Fatalf("create: %v %v %v", created, err, dir.Get(state))
	}
	created, err = EnsureStateBase(context.Background(), dir, state)
	if err != nil || created {
		t.Errorf("exists: %v %v", created, err)
	}
	// Another host creates it between our search and our add.
	dir = ldapfake.New()
	dir.Before = func(op, dn string) {
		if op == "add" {
			dir.Before = nil
			dir.Put(state, map[string][]string{"objectClass": {"organizationalUnit"}}, time.Now())
		}
	}
	if _, err := EnsureStateBase(context.Background(), dir, state); err != nil {
		t.Errorf("race: %v", err)
	}
	if _, err := EnsureStateBase(context.Background(), ldapfake.New(), "dc=dolly,dc=local"); err == nil {
		t.Error("created a dc= state_base")
	}
}

func TestStatus(t *testing.T) {
	dir := withStateBase()
	ctx := context.Background()
	t0 := time.Date(2026, 9, 21, 19, 0, 0, 0, time.UTC)
	dn := StatusDN(state)
	if _, _, err := WriteStatus(ctx, dir, state, RunStatus{End: t0, Result: "3 operations"}); err != nil {
		t.Fatal(err)
	}
	want := func(notes ...string) {
		t.Helper()
		if got := strings.Join(dir.Values(dn, "description"), "\n"); got != strings.Join(notes, "\n") {
			t.Errorf("status:\n%s\nwant:\n%s", got, strings.Join(notes, "\n"))
		}
	}
	want("last-run=2026-09-21T19:00:00Z", "last-result=3 operations", "last-success=2026-09-21T19:00:00Z")

	// A notification step would have added last-notified; it is kept.
	req := ldap.NewModifyRequest(dn, nil)
	req.Add("description", []string{"last-notified=2026-09-21T19:00:01Z"})
	if err := dir.Modify(req); err != nil {
		t.Fatal(err)
	}
	t1, t2 := t0.Add(time.Hour), t0.Add(2*time.Hour)
	_, _, _ = WriteStatus(ctx, dir, state, RunStatus{End: t1, Failure: "AD\nunreachable"})
	before, _, _ := WriteStatus(ctx, dir, state, RunStatus{End: t2, Failure: "still down"})
	if len(before) == 0 {
		t.Error("no previous notes returned")
	}
	want("last-run=2026-09-21T21:00:00Z", "last-result=3 operations", "last-success=2026-09-21T19:00:00Z",
		"last-notified=2026-09-21T19:00:01Z", "failure-since=2026-09-21T20:00:00Z", "failure=still down")

	_, _, _ = WriteStatus(ctx, dir, state, RunStatus{End: t2.Add(time.Hour), Result: "ok"})
	want("last-run=2026-09-21T22:00:00Z", "last-result=ok", "last-success=2026-09-21T22:00:00Z", "last-notified=2026-09-21T19:00:01Z")
}

// A lock is stale only when both the server's createTimestamp and the
// holder's started= note are lock_ttl old.
func TestLockStaleNeedsBothClocks(t *testing.T) {
	put := func(dir *ldapfake.Dir, created, started time.Duration) {
		dir.Put(lockDN, map[string][]string{"objectClass": {"organizationalRole"}, "cn": {"lock"},
			"description": {"host=other", "started=" + time.Now().Add(-started).UTC().Format(time.RFC3339)}},
			time.Now().Add(-created))
	}
	for _, tc := range []struct {
		name             string
		created, started time.Duration
		broken           bool
	}{
		{"server clock old, holder clock fresh", 2 * time.Hour, 10 * time.Minute, false},
		{"server clock fresh, holder clock old", 10 * time.Minute, 2 * time.Hour, false},
		{"both old", 2 * time.Hour, 2 * time.Hour, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := withStateBase()
			put(dir, tc.created, tc.started)
			l, held, err := AcquireLock(context.Background(), dir, lockOpts(&bytes.Buffer{}, 7))
			if err != nil {
				t.Fatal(err)
			}
			if got := l != nil && l.Broke != nil; got != tc.broken {
				t.Errorf("broken = %v, want %v (held %v)", got, tc.broken, held)
			}
			if !tc.broken && (held == nil || !strings.Contains(held.HolderString(), "host=other")) {
				t.Errorf("held = %v", held)
			}
		})
	}
}

// A createTimestamp more than 5 minutes in the local future means the
// clocks disagree: Dolly refuses to judge the lock and leaves it alone.
func TestLockClockSkewFuture(t *testing.T) {
	dir := withStateBase()
	putLock(dir, "other", -10*time.Minute) // created 10 minutes from now
	l, held, err := AcquireLock(context.Background(), dir, lockOpts(&bytes.Buffer{}, 7))
	if err == nil || l != nil || held != nil || !strings.Contains(err.Error(), "clocks") || !strings.Contains(err.Error(), "future") {
		t.Fatalf("acquire: %v %v %v", l, held, err)
	}
	if got := dir.Values(lockDN, "description"); len(got) != 1 || got[0] != "host=other" {
		t.Errorf("lock changed: %v", got)
	}
	// Up to 5 minutes ahead is ordinary skew: the lock is simply fresh.
	dir = withStateBase()
	putLock(dir, "other", -2*time.Minute)
	if l, held, err := AcquireLock(context.Background(), dir, lockOpts(&bytes.Buffer{}, 7)); err != nil || l != nil || held == nil {
		t.Errorf("small skew: %v %v %v", l, held, err)
	}
}

// A started= note more than 5 minutes in the local future is clock skew
// too, even with an old createTimestamp: otherwise the lock would look
// fresh forever and every run would exit 0 quietly.
func TestLockClockSkewFutureStarted(t *testing.T) {
	put := func(dir *ldapfake.Dir, startedIn time.Duration) {
		dir.Put(lockDN, map[string][]string{"objectClass": {"organizationalRole"}, "cn": {"lock"},
			"description": {"host=other", "started=" + time.Now().Add(startedIn).UTC().Format(time.RFC3339)}},
			time.Now().Add(-2*time.Hour))
	}
	dir := withStateBase()
	put(dir, 24*time.Hour)
	l, held, err := AcquireLock(context.Background(), dir, lockOpts(&bytes.Buffer{}, 7))
	var skew *ClockSkewError
	if !errors.As(err, &skew) || skew.What != "started=" || l != nil || held != nil ||
		!strings.Contains(err.Error(), "started=") || !strings.Contains(err.Error(), "future") {
		t.Fatalf("acquire: %v %v %v", l, held, err)
	}
	if got := dir.Values(lockDN, "description"); len(got) != 2 || got[0] != "host=other" {
		t.Errorf("lock changed: %v", got)
	}
	// Less than 5 minutes ahead is judged normally: with an old
	// createTimestamp, a started= 2 minutes in the future is fresh.
	dir = withStateBase()
	put(dir, 2*time.Minute)
	if l, held, err := AcquireLock(context.Background(), dir, lockOpts(&bytes.Buffer{}, 7)); err != nil || l != nil || held == nil {
		t.Errorf("small skew: %v %v %v", l, held, err)
	}
}

func TestLockInfoSkew(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	info := func(created, started time.Time) *LockInfo {
		return &LockInfo{DN: lockDN, Created: created, Holder: []string{"host=x", "started=" + started.Format(time.RFC3339)}}
	}
	for _, tc := range []struct {
		name             string
		created, started time.Duration // offsets from now
		what             string        // "" = no skew
		stale            bool
	}{
		{"both past, old", -2 * time.Hour, -2 * time.Hour, "", true},
		{"started 4m ahead", -2 * time.Hour, 4 * time.Minute, "", false},
		{"started 6m ahead", -2 * time.Hour, 6 * time.Minute, "started=", false},
		{"created 6m ahead", 6 * time.Minute, -2 * time.Hour, "createTimestamp", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := info(now.Add(tc.created), now.Add(tc.started))
			stale, err := l.Stale(now, time.Hour)
			var skew *ClockSkewError
			switch {
			case tc.what == "" && err != nil:
				t.Fatalf("unexpected error %v", err)
			case tc.what != "" && (!errors.As(err, &skew) || skew.What != tc.what):
				t.Fatalf("err = %v, want skew in %s", err, tc.what)
			}
			if stale != tc.stale {
				t.Errorf("stale = %v, want %v", stale, tc.stale)
			}
		})
	}
}

// The stale name carries the short host name; if it is already taken
// (entryAlreadyExists), the rename counts as lost, not as an error.
func TestLockStaleRDN(t *testing.T) {
	dir := withStateBase()
	putLock(dir, "crashed", 2*time.Hour)
	var renamedTo string
	dir.Fail = func(op, dn string, req any) error {
		if r, ok := req.(*ldap.ModifyDNRequest); ok && op == "modifydn" && dn == lockDN {
			renamedTo = r.NewRDN
		}
		return nil
	}
	o := lockOpts(&bytes.Buffer{}, 7)
	o.Host = "node_1.example.org"
	if l, _, err := AcquireLock(context.Background(), dir, o); err != nil || l == nil || l.Broke == nil {
		t.Fatalf("acquire: %v %v", l, err)
	}
	if !strings.HasPrefix(renamedTo, "cn=lock-stale-node1-") || !strings.HasSuffix(renamedTo, "-7") {
		t.Errorf("stale RDN %q", renamedTo)
	}

	dir = withStateBase()
	putLock(dir, "crashed", 2*time.Hour)
	dir.Fail = func(op, dn string, req any) error {
		if op == "modifydn" {
			return ldap.NewError(ldap.LDAPResultEntryAlreadyExists, nil)
		}
		return nil
	}
	var log bytes.Buffer
	l, held, err := AcquireLock(context.Background(), dir, lockOpts(&log, 7))
	if err != nil || l != nil || held == nil || strings.Contains(log.String(), "broke") {
		t.Fatalf("taken stale name: %v %v %v %s", l, held, err, log.String())
	}
	if !dir.Has(lockDN) {
		t.Error("the stale lock is gone")
	}
}

func TestEnsureStateBaseRace(t *testing.T) {
	dir := ldapfake.New()
	dir.Before = func(op, dn string) {
		if op == "add" {
			dir.Before = nil
			dir.Put(state, map[string][]string{"objectClass": {"organizationalUnit"}}, time.Now())
		}
	}
	if created, err := EnsureStateBase(context.Background(), dir, state); err != nil || created {
		t.Errorf("race: created=%v err=%v", created, err)
	}
}

func TestStatusKeepsPlainNotes(t *testing.T) {
	got := nextStatus([]string{"hand-written note", "last-run=x"}, RunStatus{End: time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC), Result: "ok"})
	for _, n := range got {
		if n == "hand-written note=" {
			t.Errorf("plain note became %q", n)
		}
	}
	if got[len(got)-1] != "hand-written note" {
		t.Errorf("plain note lost: %v", got)
	}
}

// The Notes hook sees the notes before the run and sets or deletes notes
// in the written entry.
func TestStatusNotesHook(t *testing.T) {
	dir := ldapfake.New()
	sb := "ou=dolly,dc=local"
	dn := StatusDN(sb)
	dir.Put(dn, map[string][]string{"objectClass": {"organizationalRole"}, "cn": {"status"},
		"description": {"last-run=x", "notify-error=old", "warnings-hash=aaa", "plain note"}}, time.Now())
	var seen []string
	_, after, err := WriteStatus(context.Background(), dir, sb, RunStatus{End: time.Now(), Result: "ok",
		Notes: func(before []string) map[string]string {
			seen = before
			return map[string]string{StatusNotifyError: "", StatusLastNotified: "2026-09-21T00:00:00Z", "warnings-hash": "bbb"}
		}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(seen, "|") != "last-run=x|notify-error=old|warnings-hash=aaa|plain note" {
		t.Errorf("before %v", seen)
	}
	got := strings.Join(dir.Values(dn, "description"), "|")
	if got != strings.Join(after, "|") || strings.Contains(got, "notify-error") || !strings.Contains(got, "warnings-hash=bbb") ||
		!strings.Contains(got, "last-notified=2026-09-21T00:00:00Z") || !strings.Contains(got, "plain note") || strings.Count(got, "warnings-hash") != 1 {
		t.Errorf("after %s", got)
	}
	if StatusNote(after, "warnings-hash") != "bbb" || StatusNote(after, "missing") != "" {
		t.Error("StatusNote")
	}
}
