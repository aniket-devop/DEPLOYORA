// Package domain holds the small shared types of a run: outcomes, stages and
// reason codes. It performs no I/O and imports nothing from this repository.
//
// The values here are a public contract once v0.1 ships (see
// docs/architecture/overview.md, sections 6.2, 6.3 and 6.6).
package domain

// Outcome is the single verdict of a run.
type Outcome string

const (
	// OutcomePassed means all executed checks passed.
	OutcomePassed Outcome = "passed"
	// OutcomeFailed means the thing under test is unhealthy or invalid.
	OutcomeFailed Outcome = "failed"
	// OutcomeErrored means Deployora or its environment could not complete the run.
	OutcomeErrored Outcome = "errored"
	// OutcomeInvalidConfig means the invocation or deployora.yaml is wrong.
	OutcomeInvalidConfig Outcome = "invalid_config"
	// OutcomeTimedOut means the run deadline or the provision stage deadline was exceeded.
	OutcomeTimedOut Outcome = "timed_out"
	// OutcomeCanceled means the run was interrupted by the user or CI.
	OutcomeCanceled Outcome = "canceled"
)

// ExitCode returns the process exit code for the outcome. An unknown outcome maps
// to the errored code, because an outcome Deployora cannot name is a tooling problem.
func (o Outcome) ExitCode() int {
	switch o {
	case OutcomePassed:
		return 0
	case OutcomeFailed:
		return 1
	case OutcomeErrored:
		return 2
	case OutcomeInvalidConfig:
		return 3
	case OutcomeTimedOut:
		return 4
	case OutcomeCanceled:
		return 130
	default:
		return 2
	}
}
