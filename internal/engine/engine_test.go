package engine

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/aniket-devop/deployora/internal/domain"
)

func pass(context.Context) domain.StageResult {
	return domain.StageResult{Outcome: domain.OutcomePassed}
}

func result(o domain.Outcome, r domain.Reason) StageFunc {
	return func(context.Context) domain.StageResult {
		return domain.StageResult{Outcome: o, Reason: r, Err: errors.New("stage error")}
	}
}

func stagesWith(over map[domain.Stage]StageFunc) map[domain.Stage]StageFunc {
	m := map[domain.Stage]StageFunc{}
	for _, st := range domain.AllStages() {
		m[st] = pass
	}
	for st, fn := range over {
		m[st] = fn
	}
	return m
}

func statusOf(t *testing.T, r domain.Result, st domain.Stage) domain.StageStatus {
	t.Helper()
	for _, rec := range r.Stages {
		if rec.Stage == st {
			return rec.Status
		}
	}
	t.Fatalf("stage %q missing from result", st)
	return ""
}

func hasWarning(r domain.Result, w domain.Warning) bool { return slices.Contains(r.Warnings, w) }

func TestAllStagesPass(t *testing.T) {
	r := Run(context.Background(), Spec{Stages: stagesWith(nil)})
	if r.Outcome != domain.OutcomePassed || r.Reason != "" || r.Stage != "" || r.ExitCode() != 0 {
		t.Fatalf("got %+v", r)
	}
	if len(r.Stages) != 9 {
		t.Fatalf("got %d stage records, want 9", len(r.Stages))
	}
	for i, st := range domain.AllStages() {
		if r.Stages[i].Stage != st || r.Stages[i].Status != domain.StatusPassed {
			t.Errorf("record %d is %+v, want %s passed", i, r.Stages[i], st)
		}
	}
	if len(r.Warnings) != 0 {
		t.Errorf("unexpected warnings: %v", r.Warnings)
	}
}

func TestMissingStageIsSkipped(t *testing.T) {
	stages := stagesWith(nil)
	delete(stages, domain.StageLoad)
	r := Run(context.Background(), Spec{Stages: stages})
	if r.Outcome != domain.OutcomePassed || statusOf(t, r, domain.StageLoad) != domain.StatusSkipped {
		t.Errorf("got outcome %s, load status %s", r.Outcome, statusOf(t, r, domain.StageLoad))
	}
}

func TestVerdictFailureSkipsLaterVerdictStagesButRunsWindDown(t *testing.T) {
	var ran []domain.Stage
	over := map[domain.Stage]StageFunc{}
	for _, st := range domain.AllStages() {
		over[st] = func(context.Context) domain.StageResult {
			ran = append(ran, st)
			return domain.StageResult{Outcome: domain.OutcomePassed}
		}
	}
	over[domain.StageValidate] = func(context.Context) domain.StageResult {
		ran = append(ran, domain.StageValidate)
		return domain.StageResult{Outcome: domain.OutcomeFailed, Reason: domain.ReasonValidationFailed}
	}
	r := Run(context.Background(), Spec{Stages: over})

	want := []domain.Stage{domain.StagePreflight, domain.StageValidate, domain.StageCollect, domain.StageReport, domain.StageTeardown}
	if !slices.Equal(ran, want) {
		t.Errorf("ran %v, want %v", ran, want)
	}
	if r.Outcome != domain.OutcomeFailed || r.Reason != domain.ReasonValidationFailed || r.Stage != domain.StageValidate || r.ExitCode() != 1 {
		t.Errorf("got %+v", r)
	}
	wantStatus := map[domain.Stage]domain.StageStatus{
		domain.StagePreflight: domain.StatusPassed,
		domain.StageValidate:  domain.StatusFailed,
		domain.StageProvision: domain.StatusSkipped,
		domain.StageLoad:      domain.StatusSkipped,
		domain.StageDeploy:    domain.StatusSkipped,
		domain.StageVerify:    domain.StatusSkipped,
		domain.StageCollect:   domain.StatusPassed,
		domain.StageReport:    domain.StatusPassed,
		domain.StageTeardown:  domain.StatusPassed,
	}
	for st, ws := range wantStatus {
		if got := statusOf(t, r, st); got != ws {
			t.Errorf("stage %s: status %s, want %s", st, got, ws)
		}
	}
}

func TestInterruptDuringStageWinsOverTheStageError(t *testing.T) {
	// The spike's race: the child died first and the stage reported an error, but
	// the interrupt was already recorded as the context cause.
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	r := Run(ctx, Spec{Stages: stagesWith(map[domain.Stage]StageFunc{
		domain.StageDeploy: func(context.Context) domain.StageResult {
			cancel(ErrInterrupted)
			return domain.StageResult{Outcome: domain.OutcomeErrored, Reason: domain.ReasonAPIUnavailable}
		},
	})})
	if r.Outcome != domain.OutcomeCanceled || r.Reason != domain.ReasonInterrupted || r.Stage != domain.StageDeploy || r.ExitCode() != 130 {
		t.Fatalf("got %+v", r)
	}
	if !errors.Is(r.Err, ErrInterrupted) {
		t.Errorf("Err = %v, want ErrInterrupted", r.Err)
	}
	if statusOf(t, r, domain.StageDeploy) != domain.StatusCanceled || statusOf(t, r, domain.StageVerify) != domain.StatusSkipped {
		t.Errorf("unexpected statuses: %+v", r.Stages)
	}
}

func TestWindDownRunsOnADetachedContextAfterInterrupt(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(ErrInterrupted)

	var preflightRan, teardownCtxLive, teardownHasDeadline bool
	r := Run(ctx, Spec{Stages: stagesWith(map[domain.Stage]StageFunc{
		domain.StagePreflight: func(context.Context) domain.StageResult {
			preflightRan = true
			return domain.StageResult{Outcome: domain.OutcomePassed}
		},
		domain.StageTeardown: func(c context.Context) domain.StageResult {
			teardownCtxLive = c.Err() == nil
			_, teardownHasDeadline = c.Deadline()
			return domain.StageResult{Outcome: domain.OutcomePassed}
		},
	})})
	if preflightRan {
		t.Error("a verdict stage started although the run was already cancelled")
	}
	if r.Outcome != domain.OutcomeCanceled || r.Stage != domain.StagePreflight || r.ExitCode() != 130 {
		t.Errorf("got %+v", r)
	}
	if !teardownCtxLive || !teardownHasDeadline {
		t.Errorf("teardown context live=%v hasDeadline=%v, want a live context with its own deadline", teardownCtxLive, teardownHasDeadline)
	}
	if statusOf(t, r, domain.StageTeardown) != domain.StatusPassed {
		t.Errorf("teardown status %s, want passed", statusOf(t, r, domain.StageTeardown))
	}
}

func TestRunDeadline(t *testing.T) {
	r := Run(context.Background(), Spec{
		RunDeadline: 50 * time.Millisecond,
		Stages: stagesWith(map[domain.Stage]StageFunc{
			domain.StageVerify: func(c context.Context) domain.StageResult {
				<-c.Done()
				return domain.StageResult{Outcome: domain.OutcomeErrored, Reason: domain.ReasonAPIUnavailable}
			},
		}),
	})
	if r.Outcome != domain.OutcomeTimedOut || r.Reason != domain.ReasonRunDeadline || r.Stage != domain.StageVerify || r.ExitCode() != 4 {
		t.Fatalf("got %+v", r)
	}
	if statusOf(t, r, domain.StageTeardown) != domain.StatusPassed {
		t.Errorf("teardown must still run after a deadline")
	}
}

func TestStageDeadline(t *testing.T) {
	r := Run(context.Background(), Spec{
		StageTimeouts: map[domain.Stage]time.Duration{domain.StageProvision: 50 * time.Millisecond},
		Stages: stagesWith(map[domain.Stage]StageFunc{
			domain.StageProvision: func(c context.Context) domain.StageResult {
				<-c.Done()
				return domain.StageResult{Outcome: domain.OutcomeErrored, Reason: domain.ReasonProvisionFailed}
			},
		}),
	})
	if r.Outcome != domain.OutcomeTimedOut || r.Reason != domain.ReasonStageDeadline || r.Stage != domain.StageProvision {
		t.Fatalf("got %+v", r)
	}
	if statusOf(t, r, domain.StageProvision) != domain.StatusTimedOut {
		t.Errorf("provision status %s, want timed_out", statusOf(t, r, domain.StageProvision))
	}
}

func TestWindDownProblemsAreOnlyWarnings(t *testing.T) {
	over := map[domain.Stage]StageFunc{
		domain.StageCollect:  result(domain.OutcomeErrored, domain.ReasonInternalError),
		domain.StageReport:   result(domain.OutcomeFailed, domain.ReasonInternalError),
		domain.StageTeardown: result(domain.OutcomeErrored, domain.ReasonInternalError),
	}
	r := Run(context.Background(), Spec{Stages: stagesWith(over)})
	if r.Outcome != domain.OutcomePassed || r.ExitCode() != 0 {
		t.Fatalf("wind-down problems changed the outcome: %+v", r)
	}
	for _, w := range []domain.Warning{domain.WarningEvidenceIncomplete, domain.WarningReportFailed, domain.WarningCleanupFailed} {
		if !hasWarning(r, w) {
			t.Errorf("missing warning %s in %v", w, r.Warnings)
		}
	}

	over[domain.StageDeploy] = result(domain.OutcomeFailed, domain.ReasonInvalidWorkloadConfig)
	r = Run(context.Background(), Spec{Stages: stagesWith(over)})
	if r.Outcome != domain.OutcomeFailed || r.Reason != domain.ReasonInvalidWorkloadConfig {
		t.Errorf("verdict changed by wind-down problems: %+v", r)
	}
}

func TestPanicInVerdictStage(t *testing.T) {
	var teardownRan bool
	r := Run(context.Background(), Spec{Stages: stagesWith(map[domain.Stage]StageFunc{
		domain.StageDeploy: func(context.Context) domain.StageResult { panic("boom") },
		domain.StageTeardown: func(context.Context) domain.StageResult {
			teardownRan = true
			return domain.StageResult{Outcome: domain.OutcomePassed}
		},
	})})
	if r.Outcome != domain.OutcomeErrored || r.Reason != domain.ReasonInternalError || r.Stage != domain.StageDeploy || r.ExitCode() != 2 {
		t.Fatalf("got %+v", r)
	}
	if !teardownRan {
		t.Error("teardown did not run after a panic")
	}
}

func TestPanicInWindDownStage(t *testing.T) {
	var teardownRan bool
	r := Run(context.Background(), Spec{Stages: stagesWith(map[domain.Stage]StageFunc{
		domain.StageReport: func(context.Context) domain.StageResult { panic("boom") },
		domain.StageTeardown: func(context.Context) domain.StageResult {
			teardownRan = true
			return domain.StageResult{Outcome: domain.OutcomePassed}
		},
	})})
	if r.Outcome != domain.OutcomePassed {
		t.Fatalf("a wind-down panic changed the outcome: %+v", r)
	}
	if !hasWarning(r, domain.WarningInternalErrorInWindDown) || !teardownRan {
		t.Errorf("warnings %v, teardownRan %v", r.Warnings, teardownRan)
	}
}

func TestWindDownBudgetExhausted(t *testing.T) {
	r := Run(context.Background(), Spec{
		WindDownBudget: 30 * time.Millisecond,
		Stages: stagesWith(map[domain.Stage]StageFunc{
			domain.StageCollect: func(c context.Context) domain.StageResult {
				<-c.Done()
				return domain.StageResult{Outcome: domain.OutcomeErrored, Reason: domain.ReasonInternalError}
			},
		}),
	})
	if r.Outcome != domain.OutcomePassed {
		t.Fatalf("outcome changed: %+v", r)
	}
	if statusOf(t, r, domain.StageReport) != domain.StatusSkipped || statusOf(t, r, domain.StageTeardown) != domain.StatusSkipped {
		t.Errorf("later wind-down stages should be skipped: %+v", r.Stages)
	}
	for _, w := range []domain.Warning{
		domain.WarningEvidenceIncomplete, domain.WarningWindDownInterrupted,
		domain.WarningReportFailed, domain.WarningCleanupFailed,
	} {
		if !hasWarning(r, w) {
			t.Errorf("missing warning %s in %v", w, r.Warnings)
		}
	}
}

func TestStageWithoutOutcomeIsInternalError(t *testing.T) {
	r := Run(context.Background(), Spec{Stages: stagesWith(map[domain.Stage]StageFunc{
		domain.StageValidate: func(context.Context) domain.StageResult { return domain.StageResult{} },
	})})
	if r.Outcome != domain.OutcomeErrored || r.Reason != domain.ReasonInternalError || r.Stage != domain.StageValidate {
		t.Errorf("got %+v", r)
	}
}

func TestStageWarningsAreKept(t *testing.T) {
	r := Run(context.Background(), Spec{Stages: stagesWith(map[domain.Stage]StageFunc{
		domain.StageVerify: func(context.Context) domain.StageResult {
			return domain.StageResult{Outcome: domain.OutcomePassed, Warnings: []domain.Warning{domain.WarningEvidenceIncomplete}}
		},
	})})
	if r.Outcome != domain.OutcomePassed || !hasWarning(r, domain.WarningEvidenceIncomplete) {
		t.Errorf("got %+v", r)
	}
}
