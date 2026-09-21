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
