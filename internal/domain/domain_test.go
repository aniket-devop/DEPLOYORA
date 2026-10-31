package domain

import (
	"regexp"
	"testing"
)

func TestOutcomeExitCodes(t *testing.T) {
	tests := []struct {
		outcome Outcome
		want    int
	}{
		{OutcomePassed, 0},
		{OutcomeFailed, 1},
		{OutcomeErrored, 2},
		{OutcomeInvalidConfig, 3},
		{OutcomeTimedOut, 4},
		{OutcomeCanceled, 130},
		{Outcome("nonsense"), 2},
	}
	for _, tt := range tests {
		if got := tt.outcome.ExitCode(); got != tt.want {
			t.Errorf("%q: exit code %d, want %d", tt.outcome, got, tt.want)
		}
	}
}

func TestStageOrderAndKinds(t *testing.T) {
	want := []Stage{"preflight", "validate", "provision", "load", "deploy", "verify", "collect", "report", "teardown"}
	got := AllStages()
	if len(got) != len(want) {
		t.Fatalf("got %d stages, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("stage %d is %q, want %q", i, got[i], want[i])
		}
	}
	verdict, windDown := 0, 0
	for _, s := range got {
		switch {
		case s.IsVerdict() && s.IsWindDown():
			t.Errorf("%q is both a verdict and a wind-down stage", s)
		case s.IsVerdict():
			verdict++
		case s.IsWindDown():
			windDown++
		default:
			t.Errorf("%q is neither a verdict nor a wind-down stage", s)
		}
	}
	if verdict != 6 || windDown != 3 {
		t.Errorf("got %d verdict and %d wind-down stages, want 6 and 3", verdict, windDown)
	}
}

var wireValue = regexp.MustCompile(`^[a-z]+(_[a-z]+)*$`)

func TestReasonAndWarningValues(t *testing.T) {
	seen := map[string]bool{}
	check := func(kind, v string) {
		if !wireValue.MatchString(v) {
			t.Errorf("%s %q is not lower snake_case", kind, v)
		}
		if seen[v] {
			t.Errorf("%s %q is used twice", kind, v)
		}
		seen[v] = true
	}
	for _, r := range AllReasons() {
		check("reason", string(r))
	}
	for _, w := range AllWarnings() {
		check("warning", string(w))
	}
}
