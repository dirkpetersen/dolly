package planner

import (
	"strings"
	"testing"
)

// The guard line says "a real run would abort" only in a dry run.
func TestPrintGuardWording(t *testing.T) {
	for _, real := range []bool{false, true} {
		var b strings.Builder
		(&Plan{Mode: "sync", Users: true, Real: real, Guard: Guard{Tripped: true, Reasons: []string{"too many"}}}).Print(&b)
		if got := strings.Contains(b.String(), "a real run would"); got == real {
			t.Errorf("real=%v: output:\n%s", real, b.String())
		}
		if !strings.Contains(b.String(), "too many") {
			t.Errorf("real=%v: reasons missing", real)
		}
	}
}

// Without Verbose, only containers and non-ignored warnings are listed, and
// the hints name the hidden counts; zero hides the lines.
func TestPrintVerbose(t *testing.T) {
	p := &Plan{Mode: "sync", Users: true,
		Ops:      []Op{{Kind: CreateContainer, Section: SectionContainers, DN: "ou=dolly,dc=local"}, {Kind: DeleteEntry, Section: SectionUsers, DN: "uid=x,dc=local"}},
		Warnings: []Warning{{Kind: WarnIgnored, Subject: "CN=a", Message: "user missing"}, {Kind: WarnDuplicate, Subject: "CN=b", Message: "dup"}},
		Counts:   Counts{IgnoredEntries: 1}}
	var b strings.Builder
	p.Print(&b)
	out := b.String()
	for _, want := range []string{"ou=dolly,dc=local", "(use -v to list the other 1 operations)", "\nWarnings: 1 ignored, 1 duplicate (use -v to list them)\n", "  ignored:     1 entries missing required attributes\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "uid=x") || strings.Contains(out, "CN=a") || strings.Contains(out, "CN=b") {
		t.Errorf("hidden lines printed:\n%s", out)
	}
	b.Reset()
	p.Verbose = true
	p.Print(&b)
	if out := b.String(); !strings.Contains(out, "uid=x") || !strings.Contains(out, "CN=a: user missing") || !strings.Contains(out, "Warnings (2)\n") || strings.Contains(out, "use -v") {
		t.Errorf("verbose:\n%s", out)
	}
	// id-changed always prints (applied in this run, so a later -v can't show it);
	// a kind missing from warningOrder is still counted.
	b.Reset()
	(&Plan{Mode: "sync", Users: true, Warnings: []Warning{
		{Kind: WarnIDChanged, Subject: "uid=j", Message: "uidNumber 1 -> 2"}, {Kind: "future", Subject: "x", Message: "y"}}}).Print(&b)
	if out := b.String(); !strings.Contains(out, "ID changes (1)\n  id-changed   uid=j: uidNumber 1 -> 2\n") || !strings.Contains(out, "Warnings: 1 future (use -v") {
		t.Errorf("id-changed / unknown kind:\n%s", out)
	}
	for _, k := range []WarningKind{WarnIgnored, WarnConflict, WarnDuplicate, WarnDuplicateID, WarnIDChanged, WarnPending, WarnInvalid} {
		found := false
		for _, o := range warningOrder {
			found = found || o == k
		}
		if !found {
			t.Errorf("warningOrder lacks %s", k)
		}
	}
	b.Reset()
	(&Plan{Mode: "sync", Users: true}).Print(&b)
	if out := b.String(); strings.Contains(out, "ignored:") || strings.Contains(out, "use -v") || strings.Contains(out, "Warnings") {
		t.Errorf("zero counts:\n%s", out)
	}
}
