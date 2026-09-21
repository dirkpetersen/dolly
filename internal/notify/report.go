package notify

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/dirkpetersen/dolly/internal/config"
	"github.com/dirkpetersen/dolly/internal/planner"
	"github.com/dirkpetersen/dolly/internal/target"
)

// MaxBodyLines caps a mail body; the rest is cut with a note pointing at
// the journal, which has the full output.
const MaxBodyLines = 2000

// Report describes a finished real run (one that got the lock) for the
// notification.
type Report struct {
	Command    string // "sync" or "adopt"
	Host       string
	ConfigPath string
	Start, End time.Time
	Failure    string // "" for success
	Changed    bool   // at least one operation was applied
	// BrokeLock is the stale lock this run broke, or nil.
	BrokeLock *target.LockInfo
	// Plan is the run's plan, or nil if the run failed before planning
	// (then the warnings list is unknown and the stored hash is kept).
	Plan *planner.Plan
	// Output is the run's output, stdout and stderr interleaved as
	// written: the plan, the result with created and changed groups, and
	// the log lines. It becomes the mail body after the header.
	Output string
}

// scopeKey returns the warnings-hash note for the report's run.
func (r Report) scopeKey() string {
	if r.Plan == nil {
		return WarningsKey(r.Command, true, true)
	}
	return WarningsKey(r.Plan.Mode, r.Plan.Users, r.Plan.Groups)
}

// Notes decides whether the run sends its single mail and sends it. It
// returns the cn=status notes to set (see target.RunStatus.Notes): after a
// successful send last-notified, the warnings hash, an empty notify-error
// (deleted), and for a failure mail failure-notified; after a failed send
// only notify-error. A mail
// failure is logged to log and never returned: it must not change the
// run's exit code. An empty smtp_host disables notifications entirely.
func Notes(ctx context.Context, n config.Notify, o Options, before []string, r Report, log io.Writer) map[string]string {
	if n.SMTPHost == "" {
		return nil
	}
	o = o.withDefaults()
	if r.Host == "" {
		r.Host = o.Hostname
	}
	out := Outcome{Now: r.End, Failure: r.Failure, Changed: r.Changed, BrokeLock: r.BrokeLock != nil, WarningsKey: r.scopeKey()}
	if r.Plan != nil {
		out.WarningsKnown = true
		out.WarningsHash = WarningsHash(r.Plan.Warnings)
	}
	d := Decide(n.On, n.RemindEvery.Duration, before, out)
	if !d.Send {
		return nil
	}
	m := Compose(d, before, r)
	if err := Send(ctx, n, o, m); err != nil {
		fmt.Fprintf(log, "dolly: warning: sending the notification to %s failed: %v\n", strings.Join(n.To, ", "), err)
		return map[string]string{target.StatusNotifyError: o.Now().UTC().Format(time.RFC3339) + " " + err.Error()}
	}
	notes := map[string]string{
		target.StatusLastNotified: o.Now().UTC().Format(time.RFC3339),
		target.StatusNotifyError:  "",
	}
	if d.FailureMail() {
		notes[target.StatusFailureNotified] = FailureNotifiedNote(o.Now(), r.Failure)
	}
	if out.WarningsKnown {
		notes[out.WarningsKey] = out.WarningsHash
	}
	return notes
}

// Compose writes the mail: a subject naming the main reason, and a body
// with a short header (host, command, times, why this mail), the failure
// if any, then the run's output.
func Compose(d Decision, before []string, r Report) Message {
	failing := r.Failure != ""
	var subject string
	who := strings.TrimSpace(r.Host + " dolly " + r.Command)
	switch {
	case d.Has(ReasonFailure):
		subject = fmt.Sprintf("%s FAILED: %s", who, short(r.Failure, 100))
	case d.Has(ReasonFailureChanged):
		subject = fmt.Sprintf("%s FAILED (failure changed): %s", who, short(r.Failure, 80))
	case d.Has(ReasonReminder):
		subject = fmt.Sprintf("%s still failing since %s: %s", who, target.StatusNote(before, target.StatusFailureSince), short(r.Failure, 80))
	case d.Has(ReasonStaleLock):
		subject = fmt.Sprintf("%s broke a stale run lock", who)
	case d.Has(ReasonRecovered):
		subject = fmt.Sprintf("%s recovered", who)
	case d.Has(ReasonChanges):
		subject = fmt.Sprintf("%s applied changes", who)
	case d.Has(ReasonWarnings):
		subject = fmt.Sprintf("%s warnings changed", who)
	default:
		subject = fmt.Sprintf("%s ok", who)
	}
	if r.Plan != nil && !d.FailureMail() {
		if c := planCounts(r.Plan); c != "" {
			subject += " (" + c + ")"
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Dolly %s on %s", r.Command, r.Host)
	if r.ConfigPath != "" {
		fmt.Fprintf(&b, " (config %s)", r.ConfigPath)
	}
	b.WriteString("\n")
	if !r.Start.IsZero() {
		fmt.Fprintf(&b, "Started:  %s\n", r.Start.UTC().Format(time.RFC3339))
	}
	fmt.Fprintf(&b, "Ended:    %s\n", r.End.UTC().Format(time.RFC3339))
	if failing {
		fmt.Fprintf(&b, "Outcome:  FAILED\n")
	} else {
		fmt.Fprintf(&b, "Outcome:  succeeded\n")
	}
	var why []string
	for _, x := range d.Reasons {
		why = append(why, reasonText(x, before))
	}
	fmt.Fprintf(&b, "Why this mail: %s\n", strings.Join(why, "; "))
	if failing {
		fmt.Fprintf(&b, "\nFailure: %s\n", r.Failure)
		if since := target.StatusNote(before, target.StatusFailureSince); since != "" {
			fmt.Fprintf(&b, "Failing since: %s\n", since)
		}
		if d.Has(ReasonFailureChanged) {
			fmt.Fprintf(&b, "Previous failure: %s\n", target.StatusNote(before, target.StatusFailure))
		}
	}
	if d.Has(ReasonRecovered) {
		fmt.Fprintf(&b, "\nRecovered. The failure since %s was: %s\n",
			target.StatusNote(before, target.StatusFailureSince), target.StatusNote(before, target.StatusFailure))
	}
	if l := r.BrokeLock; l != nil {
		fmt.Fprintf(&b, "\nThis run broke a stale run lock held by %s since %s: an earlier run crashed or was killed.\n",
			l.HolderString(), l.Created.UTC().Format(time.RFC3339))
	}
	if out := strings.TrimRight(r.Output, "\n"); out != "" {
		b.WriteString("\nRun output\n----------\n")
		b.WriteString(out)
		b.WriteString("\n")
	}
	return Message{Subject: subject, Body: capLines(b.String(), MaxBodyLines)}
}

func reasonText(r Reason, before []string) string {
	switch r {
	case ReasonFailure:
		return "a failure appeared (failures are always reported)"
	case ReasonFailureChanged:
		return "failure changed: the run still fails, but differently from the failure last mailed"
	case ReasonReminder:
		return "reminder: the failure persists (reminders at most every notify.remind_every)"
	case ReasonStaleLock:
		return "a stale run lock was broken"
	case ReasonRecovered:
		return "the run succeeded after a failure"
	case ReasonChanges:
		return "the run applied changes"
	case ReasonWarnings:
		return "the list of warnings (ignored entries, conflicts, ...) changed"
	case ReasonAlways:
		return "notify.on is always"
	}
	return string(r)
}

// planCounts summarizes a plan's changes in a few words, or "".
func planCounts(p *planner.Plan) string {
	c := p.Counts
	var parts []string
	add := func(n int, what string) {
		if n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, what))
		}
	}
	add(c.UsersAdded+c.UsersModified+c.UsersRenamed+c.UsersDeleted, "users")
	add(c.GroupsAdded+c.GroupsModified+c.GroupsRenamed, "groups")
	add(c.MembersAdded+c.MembersRemoved, "memberships")
	return strings.Join(parts, ", ")
}

func short(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "..."
	}
	return s
}

// capLines keeps the first max lines of s and replaces the rest with a
// note.
func capLines(s string, max int) string {
	lines := strings.SplitAfter(s, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) <= max {
		return s
	}
	cut := len(lines) - max
	return strings.Join(lines[:max], "") +
		fmt.Sprintf("\n[... %d more lines not shown; the full output is in the journal: journalctl --user -u dolly]\n", cut)
}
