package notify

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"time"

	"github.com/dirkpetersen/dolly/internal/planner"
	"github.com/dirkpetersen/dolly/internal/target"
)

// Reason is why a run sends mail. A mail can have several; the first in
// this order names it in the subject.
type Reason string

// Reasons, in subject precedence.
const (
	ReasonFailure   Reason = "failure"    // a failure appeared (or was never mailed)
	ReasonReminder  Reason = "reminder"   // the failure persists and remind_every has passed
	ReasonStaleLock Reason = "stale-lock" // this run broke a stale run lock
	ReasonRecovered Reason = "recovered"  // a run succeeded after a failure
	ReasonChanges   Reason = "changes"    // on: changes or always, and the run applied changes
	ReasonWarnings  Reason = "warnings"   // the warnings list (ignored entries, conflicts, ...) changed
	ReasonAlways    Reason = "always"     // on: always
)

// Outcome is what the decision needs to know about a run.
type Outcome struct {
	Now time.Time
	// Failure is the run's failure ("" for success). A tripped guard, a
	// read error, per-entry errors, and a stop by signal or run_timeout
	// are failures.
	Failure string
	// Changed: the run applied at least one operation.
	Changed bool
	// BrokeLock: the run broke a stale run lock (a crashed run before it).
	BrokeLock bool
	// Held: another run held the lock; such a run never sends mail.
	Held bool
	// WarningsKnown: the run got as far as planning, so WarningsHash is
	// meaningful. A run that failed to read has no warnings list and
	// leaves the stored hash alone.
	WarningsKnown bool
	WarningsHash  string // WarningsHash of the plan's warnings; "" for none
	WarningsKey   string // the status note holding the hash (WarningsKey)
}

// Decision is whether and why to send the run's single mail.
type Decision struct {
	Send    bool
	Reasons []Reason
}

// Has reports whether r is among the reasons.
func (d Decision) Has(r Reason) bool {
	for _, x := range d.Reasons {
		if x == r {
			return true
		}
	}
	return false
}

// Decide is the notification decision, a pure function of notify.on,
// notify.remind_every, the cn=status notes from before the run, and the
// run's outcome:
//
//   - Failures are always reported, whatever on says: a mail when a failure
//     first appears (failure-since absent before the run, or last-notified
//     older than failure-since, so a failure whose mail failed is retried),
//     then a reminder at most every remind_every (by last-notified) while
//     it persists, and one "recovered" mail when a run succeeds after a
//     failure. Breaking a stale lock is always mailed too.
//   - on: changes also mails when the run applied changes; on: always mails
//     every run.
//   - A changed warnings list (hash differs from the stored note) is
//     mailed once, whatever on says; an unchanged list never triggers mail.
//   - A run that found the lock held never mails.
func Decide(on string, remindEvery time.Duration, before []string, o Outcome) Decision {
	var d Decision
	if o.Held {
		return d
	}
	add := func(r Reason) { d.Reasons = append(d.Reasons, r) }
	failSince, prevFailing := parseTime(target.StatusNote(before, target.StatusFailureSince))
	if !prevFailing && target.StatusNote(before, target.StatusFailureSince) != "" {
		prevFailing = true // present but unparsable: still a failure in progress
	}
	lastNotified, notified := parseTime(target.StatusNote(before, target.StatusLastNotified))
	switch {
	case o.Failure != "":
		switch {
		case !prevFailing, !notified, lastNotified.Before(failSince):
			add(ReasonFailure)
		case remindEvery > 0 && o.Now.Sub(lastNotified) >= remindEvery:
			add(ReasonReminder)
		}
	case prevFailing:
		add(ReasonRecovered)
	}
	if o.BrokeLock {
		add(ReasonStaleLock)
	}
	if o.Changed && (on == "changes" || on == "always") {
		add(ReasonChanges)
	}
	if o.WarningsKnown && o.WarningsHash != target.StatusNote(before, o.WarningsKey) {
		add(ReasonWarnings)
	}
	if on == "always" {
		add(ReasonAlways)
	}
	d.Send = len(d.Reasons) > 0
	return d
}

func parseTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, s)
	return t, err == nil
}

// WarningsKey returns the status note that holds the warnings hash for a
// run scope. Runs of different scopes see different warnings (ignored
// users are listed only by runs that sync users), so each scope has its
// own note, and alternating --users and --groups runs don't flip one hash.
func WarningsKey(mode string, users, groups bool) string {
	switch {
	case mode == "adopt":
		return target.StatusWarningsHash + "-adopt"
	case users && !groups:
		return target.StatusWarningsHash + "-users"
	case groups && !users:
		return target.StatusWarningsHash + "-groups"
	}
	return target.StatusWarningsHash
}

// MailedWarnings returns the warnings whose list is tracked by the hash:
// every standing condition (ignored entries, conflicts, duplicates, pending
// groups, invalid values). A changed uidNumber or gidNumber is an event of
// this run, reported as a change, not a standing condition.
func MailedWarnings(ws []planner.Warning) []planner.Warning {
	var out []planner.Warning
	for _, w := range ws {
		if w.Kind != planner.WarnIDChanged {
			out = append(out, w)
		}
	}
	return out
}

// WarningsHash returns a short, order-independent hash of the warnings
// that MailedWarnings keeps, or "" if there are none.
func WarningsHash(ws []planner.Warning) string {
	ws = MailedWarnings(ws)
	if len(ws) == 0 {
		return ""
	}
	lines := make([]string, len(ws))
	for i, w := range ws {
		lines[i] = string(w.Kind) + "\x00" + w.Subject + "\x00" + w.Message
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:])[:16]
}
