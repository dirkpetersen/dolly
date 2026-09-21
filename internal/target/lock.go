package target

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-ldap/ldap/v3"

	"github.com/dirkpetersen/dolly/internal/ldapconn"
	"github.com/dirkpetersen/dolly/internal/model"
)

// LockInfo describes a run lock entry.
type LockInfo struct {
	DN      string
	Holder  []string  // description values: host=, pid=, started=, command=
	Created time.Time // the server's createTimestamp
}

// Age returns how old the lock is at now, by the server's createTimestamp.
func (l *LockInfo) Age(now time.Time) time.Duration { return now.Sub(l.Created).Round(time.Second) }

// maxClockSkew is how far in the local future a lock's createTimestamp may
// be before Dolly refuses to judge it: the clocks of this host and the
// server disagree, and breaking the lock could break a live run's.
const maxClockSkew = 5 * time.Minute

// Started returns the holder's started= note, if it has a parseable one.
func (l *LockInfo) Started() (time.Time, bool) {
	for _, h := range l.Holder {
		if v, ok := strings.CutPrefix(h, "started="); ok {
			if t, err := time.Parse(time.RFC3339, v); err == nil {
				return t, true
			}
		}
	}
	return time.Time{}, false
}

// stale reports whether the lock is at least ttl old at now. Both clocks
// must agree: the server's createTimestamp and the holder's started= note
// (written by the holder's clock) must both be ttl old, so one skewed clock
// can't break a live run's lock. A lock without a started= note is judged
// by createTimestamp alone. A createTimestamp more than maxClockSkew in the
// local future is an error: the clocks disagree and nothing is judged.
func (l *LockInfo) stale(now time.Time, ttl time.Duration) (bool, error) {
	if ahead := l.Created.Sub(now); ahead > maxClockSkew {
		return false, fmt.Errorf("the run lock %s was created at %s by the server's clock, %s in this host's future: "+
			"the clocks of this host and the LDAP server disagree (keep both NTP-synced); not judging the lock",
			l.DN, l.Created.UTC().Format(time.RFC3339), ahead.Round(time.Second))
	}
	if now.Sub(l.Created) < ttl {
		return false, nil
	}
	if started, ok := l.Started(); ok && now.Sub(started) < ttl {
		return false, nil
	}
	return true, nil
}

// HolderString renders the holder notes on one line.
func (l *LockInfo) HolderString() string {
	if len(l.Holder) == 0 {
		return "(unknown holder)"
	}
	return strings.Join(l.Holder, " ")
}

// Lock is a run lock held by this process.
type Lock struct {
	conn   Conn
	dn     string
	holder []string
	// Broke is the stale lock this acquire broke, or nil. The caller warns
	// and (once notifications exist) sends a notification.
	Broke *LockInfo
}

// LockOptions configure AcquireLock.
type LockOptions struct {
	StateBase string
	TTL       time.Duration // sync.lock_ttl
	Command   string        // e.g. "sync", recorded in the lock
	// Now and Host default to time.Now and os.Hostname; PID to os.Getpid.
	Now  func() time.Time
	Host string
	PID  int
	// Log receives warnings (a broken stale lock).
	Log io.Writer
}

// LockDN returns the run lock's DN under stateBase.
func LockDN(stateBase string) string { return model.LockRDN + "," + stateBase }

// AcquireLock takes the run lock cn=lock,<state_base> with an atomic LDAP
// add. It returns the lock, or held (the current lock) if another run holds
// a lock younger than TTL, or an error.
//
// A lock is stale when both the server's createTimestamp (requested
// explicitly since it is operational) and the holder's started= note are
// at least TTL old; a createTimestamp too far in the future is an error
// (see LockInfo.stale). It is broken by renaming
// it to a unique name first: only one host can win that modrdn. The winner
// deletes the renamed entry, warns, and retries the add once; a loser
// reports the lock as held. If the renamed entry turns out to be fresh
// (another host broke the stale lock and took a new one between our read
// and our rename), it is renamed back and reported as held.
func AcquireLock(ctx context.Context, c Conn, o LockOptions) (lock *Lock, held *LockInfo, err error) {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Host == "" {
		o.Host, _ = os.Hostname()
	}
	if o.PID == 0 {
		o.PID = os.Getpid()
	}
	if o.Log == nil {
		o.Log = io.Discard
	}
	dn := LockDN(o.StateBase)
	l := &Lock{conn: c, dn: dn, holder: []string{
		"host=" + o.Host,
		"pid=" + strconv.Itoa(o.PID),
		"started=" + o.Now().UTC().Format(time.RFC3339),
		"command=dolly " + o.Command,
	}}
	for attempt := 0; attempt < 2; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		err := c.Add(lockRequest(dn, l.holder))
		if err == nil {
			return l, nil, nil
		}
		if !ldap.IsErrorWithCode(err, ldap.LDAPResultEntryAlreadyExists) {
			return nil, nil, fmt.Errorf("taking the run lock %s: %w", dn, err)
		}
		cur, err := ReadLock(c, dn)
		if err != nil {
			return nil, nil, err
		}
		if cur == nil {
			continue // released between our add and our read
		}
		stale, err := cur.stale(o.Now(), o.TTL)
		if err != nil {
			return nil, nil, err
		}
		if !stale {
			return nil, cur, nil
		}
		if attempt > 0 {
			return nil, cur, nil // broke one stale lock already; don't loop
		}
		won, err := breakStale(c, cur, o)
		if err != nil {
			return nil, nil, err
		}
		if !won {
			// Another host renamed it first and will take the lock.
			return nil, cur, nil
		}
		l.Broke = cur
		fmt.Fprintf(o.Log, "dolly: warning: broke a stale run lock %s held by %s since %s (%s old, lock_ttl %s)\n",
			dn, cur.HolderString(), cur.Created.UTC().Format(time.RFC3339), cur.Age(o.Now()), o.TTL)
	}
	cur, err := ReadLock(c, dn)
	if err != nil {
		return nil, nil, err
	}
	if cur == nil {
		return nil, nil, fmt.Errorf("taking the run lock %s: the lock keeps changing; try again", dn)
	}
	return nil, cur, nil
}

// breakStale renames the stale lock cur to a unique name and deletes it.
// It reports whether this host won the rename.
func breakStale(c Conn, cur *LockInfo, o LockOptions) (bool, error) {
	now := o.Now()
	rdn := fmt.Sprintf("cn=lock-stale-%s-%d-%d", shortHost(o.Host), now.Unix(), o.PID)
	stale := rdn + "," + o.StateBase
	err := c.ModifyDN(ldap.NewModifyDNRequest(cur.DN, rdn, true, ""))
	switch {
	case ldapconn.IsNoSuchObject(err):
		return false, nil // another host renamed it first
	case ldap.IsErrorWithCode(err, ldap.LDAPResultEntryAlreadyExists):
		return false, nil // the stale name is taken: treat as a lost rename
	case err != nil:
		return false, fmt.Errorf("breaking the stale run lock %s: %w", cur.DN, err)
	}
	// The rename has no precondition: make sure what we renamed is the
	// stale lock we read, not a fresh one another host took meanwhile.
	got, err := ReadLock(c, stale)
	if err != nil {
		return false, fmt.Errorf("breaking the stale run lock %s: %w", cur.DN, err)
	}
	if got != nil && !isStale(got, now, o.TTL) {
		if err := c.ModifyDN(ldap.NewModifyDNRequest(stale, model.LockRDN, true, "")); err != nil {
			return false, fmt.Errorf("renamed a fresh run lock to %s by mistake and could not rename it back: %w", stale, err)
		}
		return false, nil
	}
	if err := c.Del(ldap.NewDelRequest(stale, nil)); err != nil && !ldapconn.IsNoSuchObject(err) {
		fmt.Fprintf(o.Log, "dolly: warning: could not delete the broken lock %s: %v\n", stale, err)
	}
	return true, nil
}

// isStale is LockInfo.stale for the renamed entry, where a clock-skew error
// counts as fresh: rename it back rather than delete it.
func isStale(l *LockInfo, now time.Time, ttl time.Duration) bool {
	s, err := l.stale(now, ttl)
	return err == nil && s
}

// shortHost returns the first label of host, reduced to [A-Za-z0-9-], for
// use in an RDN value; "unknown" if nothing is left.
func shortHost(host string) string {
	host, _, _ = strings.Cut(host, ".")
	var b strings.Builder
	for _, r := range host {
		if r < 0x80 && (r == '-' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "unknown"
	}
	return b.String()
}

func lockRequest(dn string, holder []string) *ldap.AddRequest {
	req := ldap.NewAddRequest(dn, nil)
	req.Attribute("objectClass", []string{"organizationalRole"})
	req.Attribute("cn", []string{model.RDNValue(dn)})
	req.Attribute("description", holder)
	return req
}

// ReadLock reads the lock entry at dn with its createTimestamp. It returns
// nil, nil if there is no lock.
func ReadLock(c Conn, dn string) (*LockInfo, error) {
	e, err := readEntry(c, dn, "description", "createTimestamp")
	if err != nil {
		return nil, fmt.Errorf("reading the run lock %s: %w", dn, err)
	}
	if e == nil {
		return nil, nil
	}
	info := &LockInfo{DN: e.DN, Holder: e.GetEqualFoldAttributeValues("description")}
	ts := e.GetEqualFoldAttributeValue("createTimestamp")
	if ts == "" {
		return nil, fmt.Errorf("reading the run lock %s: the server returned no createTimestamp", dn)
	}
	if info.Created, err = parseGeneralizedTime(ts); err != nil {
		return nil, fmt.Errorf("reading the run lock %s: %w", dn, err)
	}
	return info, nil
}

// Release deletes the lock if it is still ours. The caller passes a fresh
// short-timeout context (network_timeout), never the run's context, which
// may already be cancelled (SIGTERM, run_timeout). Calling it twice is
// harmless.
func (l *Lock) Release(ctx context.Context) error {
	if l == nil || l.conn == nil {
		return nil
	}
	c := l.conn
	l.conn = nil
	done := make(chan error, 1)
	go func() { done <- release(c, l.dn, l.holder) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return fmt.Errorf("releasing the run lock %s: %w", l.dn, ctx.Err())
	}
}

func release(c Conn, dn string, holder []string) error {
	cur, err := ReadLock(c, dn)
	switch {
	case err != nil:
		return err
	case cur == nil:
		return fmt.Errorf("the run lock %s is already gone", dn)
	case !sameHolder(cur.Holder, holder):
		return fmt.Errorf("the run lock %s is now held by %s; leaving it", dn, cur.HolderString())
	}
	if err := c.Del(ldap.NewDelRequest(dn, nil)); err != nil {
		return fmt.Errorf("releasing the run lock %s: %w", dn, err)
	}
	return nil
}

func sameHolder(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]int{}
	for _, v := range a {
		seen[v]++
	}
	for _, v := range b {
		if seen[v] == 0 {
			return false
		}
		seen[v]--
	}
	return true
}

// RemoveLock deletes the lock entry (dolly unlock).
func RemoveLock(c Conn, dn string) error {
	if err := c.Del(ldap.NewDelRequest(dn, nil)); err != nil {
		return fmt.Errorf("removing the run lock %s: %w", dn, err)
	}
	return nil
}
