package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/go-ldap/ldap/v3"

	"github.com/dirkpetersen/dolly/internal/ldapfake"
	"github.com/dirkpetersen/dolly/internal/planner"
)

// Progress lines during apply: the first after progressFirst names the
// interval, then one every progressEvery; a fast apply prints none. The
// lines go to the progress writer only, not to the run log.
func TestApplyProgress(t *testing.T) {
	oldFirst, oldEvery := progressFirst, progressEvery
	t.Cleanup(func() { progressFirst, progressEvery = oldFirst, oldEvery })
	plan := func(n int) *planner.Plan {
		p := &planner.Plan{}
		for i := 0; i < n; i++ {
			p.Ops = append(p.Ops, planner.Op{Kind: planner.DeleteEntry, DN: "cn=x" + strings.Repeat("y", i) + ",dc=local"})
		}
		return p
	}
	slow := ldapfake.New()
	slow.Fail = func(string, string, any) error {
		time.Sleep(5 * time.Millisecond)
		return ldapfake.Error(ldap.LDAPResultUnwillingToPerform, "injected")
	}

	progressFirst, progressEvery = 20*time.Millisecond, 30*time.Millisecond
	var log, prog bytes.Buffer
	res := applyWithProgress(context.Background(), slow, plan(40), &log, &prog, false)
	lines := strings.Split(strings.TrimSpace(prog.String()), "\n")
	if res.Failed != 40 || len(lines) < 2 || !strings.HasPrefix(lines[0], "dolly: done ") ||
		!strings.Contains(lines[0], " of 40 operations (") || !strings.HasSuffix(lines[0], "elapsed; next update every 0.03s") ||
		strings.Contains(lines[1], "next update") || strings.Contains(log.String(), "dolly: done ") {
		t.Errorf("progress:\n%s\nlog:\n%s", prog.String(), log.String())
	}

	progressFirst = time.Hour
	prog.Reset()
	applyWithProgress(context.Background(), ldapfake.New(), plan(3), &log, &prog, false)
	if prog.Len() != 0 {
		t.Errorf("fast apply printed %q", prog.String())
	}
}
