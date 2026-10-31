package domain

// Stage is one phase of a run, in run order.
type Stage string

const (
	StagePreflight Stage = "preflight"
	StageValidate  Stage = "validate"
	StageProvision Stage = "provision"
	StageLoad      Stage = "load"
	StageDeploy    Stage = "deploy"
	StageVerify    Stage = "verify"
	StageCollect   Stage = "collect"
	StageReport    Stage = "report"
	StageTeardown  Stage = "teardown"
)

// AllStages returns every stage in run order.
func AllStages() []Stage {
	return []Stage{
		StagePreflight, StageValidate, StageProvision, StageLoad, StageDeploy,
		StageVerify, StageCollect, StageReport, StageTeardown,
	}
}

// IsVerdict reports whether the stage can fix the outcome of a run. The first
// terminal cause among the verdict stages decides the outcome.
func (s Stage) IsVerdict() bool {
	switch s {
	case StagePreflight, StageValidate, StageProvision, StageLoad, StageDeploy, StageVerify:
		return true
	}
	return false
}

// IsWindDown reports whether the stage is a wind-down stage. Wind-down stages
// never change the outcome, whatever happens in them.
func (s Stage) IsWindDown() bool {
	switch s {
	case StageCollect, StageReport, StageTeardown:
		return true
	}
	return false
}

// StageStatus is the status recorded for a single stage.
type StageStatus string

const (
	StatusPassed   StageStatus = "passed"
	StatusFailed   StageStatus = "failed"
	StatusErrored  StageStatus = "errored"
	StatusTimedOut StageStatus = "timed_out"
	StatusCanceled StageStatus = "canceled"
	StatusSkipped  StageStatus = "skipped"
)
