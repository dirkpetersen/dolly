package target

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/go-ldap/ldap/v3"

	"github.com/dirkpetersen/dolly/internal/model"
)

// Status notes, stored as key=value description values of
// cn=status,<state_base> (an organizationalRole, core schema only).
const (
	StatusLastRun      = "last-run"      // when the last real run ended (RFC 3339, UTC)
	StatusLastSuccess  = "last-success"  // when the last successful run ended
	StatusFailureSince = "failure-since" // when the current failure first appeared; absent while healthy
	StatusFailure      = "failure"       // the current failure, one line; absent while healthy
	StatusLastResult   = "last-result"   // one-line summary of the last run
	StatusLastNotified = "last-notified" // when mail was last sent successfully; set by the Notes hook
	StatusNotifyError  = "notify-error"  // the last mail failure (time and error); cleared by a successful send
	// StatusWarningsHash is the prefix of the notes that hold a short hash
	// of the last mailed warnings list (ignored entries, conflicts, ...),
	// one note per run scope: warnings-hash (users and groups),
	// warnings-hash-users, warnings-hash-groups, warnings-hash-adopt.
	StatusWarningsHash = "warnings-hash"
)

// maxFailureLen caps the failure note, so a huge error can't bloat the entry.
const maxFailureLen = 500

// RunStatus is the outcome of a real run, for cn=status.
type RunStatus struct {
	End     time.Time
	Failure string // "" for a successful run
	Result  string // one-line summary
	// Notes, if set, is called with the notes before the run, after the
	// new notes are computed and before they are written. It returns
	// notes to set; an empty value deletes the note. Notifications use it
	// to decide on and send the run's single mail, and to record
	// last-notified, notify-error, and the warnings hash.
	Notes func(before []string) map[string]string
}

// StatusDN returns cn=status under stateBase.
func StatusDN(stateBase string) string { return model.StatusRDN + "," + stateBase }

// WriteStatus records a run's outcome in cn=status: the last run and last
// success, or the current failure and when it first appeared. Notes it
// doesn't manage (last-notified) are kept. It returns the notes before and
// after, for the notification decision.
func WriteStatus(ctx context.Context, c Conn, stateBase string, st RunStatus) (before, after []string, err error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	dn := StatusDN(stateBase)
	e, err := readEntry(c, dn, "description")
	if err != nil {
		return nil, nil, fmt.Errorf("reading %s: %w", dn, err)
	}
	if e != nil {
		before = e.GetEqualFoldAttributeValues("description")
	}
	after = nextStatus(before, st)
	if st.Notes != nil {
		after = setNotes(after, st.Notes(before))
	}

	if e == nil {
		req := ldap.NewAddRequest(dn, nil)
		req.Attribute("objectClass", []string{"organizationalRole"})
		req.Attribute("cn", []string{model.RDNValue(dn)})
		req.Attribute("description", after)
		err = c.Add(req)
	} else {
		req := ldap.NewModifyRequest(dn, nil)
		req.Replace("description", after)
		err = c.Modify(req)
	}
	if err != nil {
		return before, after, fmt.Errorf("writing %s: %w", dn, err)
	}
	return before, after, nil
}

// nextStatus computes the new status notes from the old ones.
func nextStatus(before []string, st RunStatus) []string {
	notes := map[string]string{}
	var order []string
	var plain []string // notes without "=", kept verbatim
	for _, n := range before {
		k, v, ok := strings.Cut(n, "=")
		if !ok {
			plain = append(plain, n)
			continue
		}
		if _, dup := notes[k]; !dup {
			order = append(order, k)
		}
		notes[k] = v
	}
	set := func(k, v string) {
		if _, ok := notes[k]; !ok {
			order = append(order, k)
		}
		notes[k] = v
	}
	end := st.End.UTC().Format(time.RFC3339)
	set(StatusLastRun, end)
	if st.Result != "" {
		set(StatusLastResult, oneLine(st.Result))
	}
	if st.Failure == "" {
		set(StatusLastSuccess, end)
		delete(notes, StatusFailure)
		delete(notes, StatusFailureSince)
	} else {
		if _, ok := notes[StatusFailureSince]; !ok {
			set(StatusFailureSince, end)
		}
		set(StatusFailure, oneLine(st.Failure))
	}
	var out []string
	for _, k := range order {
		if v, ok := notes[k]; ok {
			out = append(out, k+"="+v)
		}
	}
	return append(out, plain...)
}

// setNotes sets key=value notes in notes, in place where a key exists and
// appended otherwise; an empty value removes the key.
func setNotes(notes []string, set map[string]string) []string {
	if len(set) == 0 {
		return notes
	}
	done := map[string]bool{}
	var out []string
	for _, n := range notes {
		k, _, ok := strings.Cut(n, "=")
		v, change := set[k]
		switch {
		case !ok || !change:
			out = append(out, n)
		case done[k] || v == "":
			// removed, or a duplicate of a key already set
		default:
			out = append(out, k+"="+oneLine(v))
		}
		if ok && change {
			done[k] = true
		}
	}
	for _, k := range model.SortedKeys(set) {
		if v := set[k]; !done[k] && v != "" {
			out = append(out, k+"="+oneLine(v))
		}
	}
	return out
}

// StatusNote returns the value of key in status notes, or "".
func StatusNote(notes []string, key string) string {
	for _, n := range notes {
		if k, v, ok := strings.Cut(n, "="); ok && k == key {
			return v
		}
	}
	return ""
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > maxFailureLen {
		s = string(r[:maxFailureLen]) + "..."
	}
	return s
}
