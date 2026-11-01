package domain

// StageResult is what one stage reports back to the engine. A stage that
// succeeded returns OutcomePassed. Any other outcome is a terminal cause when it
// comes from a verdict stage, and only a warning when it comes from a wind-down
// stage.
type StageResult struct {
	Outcome Outcome
	Reason  Reason
	// Err is the underlying error, kept for the report. It is never parsed.
	Err error
	// Warnings are recorded without changing the outcome.
	Warnings []Warning
}

// StageRecord is the status of one stage in a finished run.
type StageRecord struct {
	Stage  Stage
	Status StageStatus
}

// Result is the verdict of a finished run.
type Result struct {
	Outcome Outcome
	Reason  Reason
	// Stage is the verdict stage where the outcome was fixed. Empty when passed.
	Stage Stage
	// Err is the error behind the terminal cause, if any.
	Err error
	// Stages lists every stage in run order, skipped ones included.
	Stages   []StageRecord
	Warnings []Warning
}

// ExitCode returns the process exit code for the run.
func (r Result) ExitCode() int { return r.Outcome.ExitCode() }
