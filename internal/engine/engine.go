// Package engine runs the stages of a run in order and decides the outcome.
//
// The rules come from docs/architecture/overview.md section 6.4 and from the
// Kind interrupt spike (docs/spikes/kind-interrupt.md):
//
//   - The first terminal cause among the verdict stages fixes the outcome.
//   - Cancellation or a deadline during a verdict stage wins over any error the
//     stage returns, because that error is usually a consequence of it.
//   - Whether the run was interrupted is read from the context cause, which is
//     set at the moment of cancellation, never inferred from what a child
//     process did.
//   - Wind-down stages run on a context detached from the run's cancellation,
//     and nothing that happens in them changes the outcome.
package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/aniket-devop/deployora/internal/domain"
)

var (
	// ErrInterrupted is the cause a caller sets when the user or CI interrupts the
	// run: ctx, cancel := context.WithCancelCause(parent), then cancel(ErrInterrupted).
	ErrInterrupted = errors.New("run interrupted")

	errRunDeadline   = errors.New("run deadline exceeded")
	errStageDeadline = errors.New("stage deadline exceeded")
)

// DefaultWindDownBudget is the total time allowed for collect, report and
// teardown together. The spike saw kind delete take 65 to 80 seconds on a slow
// laptop, so this is deliberately generous. It is provisional.
const DefaultWindDownBudget = 5 * time.Minute

// StageFunc runs one stage. The stages themselves live in other packages and
// are injected, so the engine can be tested with fakes.
type StageFunc func(ctx context.Context) domain.StageResult

// Spec describes one run.
type Spec struct {
	// Stages maps a stage to its implementation. A stage with no entry is skipped.
	Stages map[domain.Stage]StageFunc
	// RunDeadline bounds the verdict stages. Zero means no deadline.
	RunDeadline time.Duration
	// StageTimeouts bounds individual verdict stages (for example provision).
	StageTimeouts map[domain.Stage]time.Duration
	// WindDownBudget bounds collect, report and teardown together. Zero means
	// DefaultWindDownBudget.
	WindDownBudget time.Duration
}

// Run executes the stages and returns the verdict. It never returns an error:
// every problem is an outcome or a warning.
func Run(ctx context.Context, spec Spec) domain.Result {
	runCtx := ctx
	cancel := func() {}
	if spec.RunDeadline > 0 {
		runCtx, cancel = context.WithTimeoutCause(ctx, spec.RunDeadline, errRunDeadline)
	}
	defer cancel()

	status := make(map[domain.Stage]domain.StageStatus)
	for _, st := range domain.AllStages() {
		status[st] = domain.StatusSkipped
	}
	var warnings []domain.Warning
	res := domain.Result{Outcome: domain.OutcomePassed}
	fixed := false
	fix := func(st domain.Stage, o domain.Outcome, r domain.Reason, err error) {
		fixed = true
		res.Outcome, res.Reason, res.Stage, res.Err = o, r, st, err
		status[st] = statusFor(o)
	}

	for _, st := range domain.AllStages() {
		if fixed || !st.IsVerdict() {
			continue
		}
		fn := spec.Stages[st]
		if fn == nil {
			continue
		}
		if runCtx.Err() != nil {
			o, r, err := contextVerdict(runCtx)
			fix(st, o, r, err)
			continue
		}

		stageCtx, stageCancel := runCtx, func() {}
		if d := spec.StageTimeouts[st]; d > 0 {
			stageCtx, stageCancel = context.WithTimeoutCause(runCtx, d, errStageDeadline)
		}
		sr, _ := runStage(stageCtx, fn)
		stageDeadline := errors.Is(context.Cause(stageCtx), errStageDeadline)
		stageCancel()

		warnings = addWarnings(warnings, sr.Warnings...)
		if sr.Outcome == "" {
			sr = domain.StageResult{
				Outcome: domain.OutcomeErrored,
				Reason:  domain.ReasonInternalError,
				Err:     errors.New("stage returned no outcome"),
			}
		}
		switch {
		case sr.Outcome == domain.OutcomePassed:
			status[st] = domain.StatusPassed
		case runCtx.Err() != nil:
			o, r, err := contextVerdict(runCtx)
			fix(st, o, r, err)
		case stageDeadline:
			fix(st, domain.OutcomeTimedOut, domain.ReasonStageDeadline, errStageDeadline)
		default:
			fix(st, sr.Outcome, sr.Reason, sr.Err)
		}
	}

	// Wind-down: detached from the run's cancellation, with its own budget.
	budget := spec.WindDownBudget
	if budget <= 0 {
		budget = DefaultWindDownBudget
	}
	wctx, wcancel := context.WithTimeout(context.WithoutCancel(ctx), budget)
	defer wcancel()
	for _, st := range domain.AllStages() {
		if !st.IsWindDown() {
			continue
		}
		fn := spec.Stages[st]
		if fn == nil {
			continue
		}
		if wctx.Err() != nil {
			warnings = addWarnings(warnings, domain.WarningWindDownInterrupted, windDownWarning(st))
			continue
		}
		sr, panicked := runStage(wctx, fn)
		warnings = addWarnings(warnings, sr.Warnings...)
		switch {
		case panicked:
			status[st] = domain.StatusErrored
			warnings = addWarnings(warnings, domain.WarningInternalErrorInWindDown)
		case sr.Outcome == domain.OutcomePassed:
			status[st] = domain.StatusPassed
		default:
			o := sr.Outcome
			if o == "" {
				o = domain.OutcomeErrored
			}
			status[st] = statusFor(o)
			warnings = addWarnings(warnings, windDownWarning(st))
			if wctx.Err() != nil {
				warnings = addWarnings(warnings, domain.WarningWindDownInterrupted)
			}
		}
	}

	for _, st := range domain.AllStages() {
		res.Stages = append(res.Stages, domain.StageRecord{Stage: st, Status: status[st]})
	}
	res.Warnings = warnings
	return res
}

// runStage runs fn and turns a panic into an errored result.
func runStage(ctx context.Context, fn StageFunc) (res domain.StageResult, panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			res = domain.StageResult{
				Outcome: domain.OutcomeErrored,
				Reason:  domain.ReasonInternalError,
				Err:     fmt.Errorf("panic in stage: %v", r),
			}
			panicked = true
		}
	}()
	return fn(ctx), false
}

// contextVerdict returns the outcome for a run context that is done: a deadline
// means timed_out, anything else (the caller's cancel, with ErrInterrupted as
// the usual cause) means canceled.
func contextVerdict(ctx context.Context) (domain.Outcome, domain.Reason, error) {
	cause := context.Cause(ctx)
	if errors.Is(cause, errRunDeadline) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return domain.OutcomeTimedOut, domain.ReasonRunDeadline, cause
	}
	return domain.OutcomeCanceled, domain.ReasonInterrupted, cause
}

func statusFor(o domain.Outcome) domain.StageStatus {
	switch o {
	case domain.OutcomePassed:
		return domain.StatusPassed
	case domain.OutcomeFailed, domain.OutcomeInvalidConfig:
		return domain.StatusFailed
	case domain.OutcomeTimedOut:
		return domain.StatusTimedOut
	case domain.OutcomeCanceled:
		return domain.StatusCanceled
	default:
		return domain.StatusErrored
	}
}

func windDownWarning(st domain.Stage) domain.Warning {
	switch st {
	case domain.StageCollect:
		return domain.WarningEvidenceIncomplete
	case domain.StageReport:
		return domain.WarningReportFailed
	default:
		return domain.WarningCleanupFailed
	}
}

func addWarnings(list []domain.Warning, add ...domain.Warning) []domain.Warning {
	for _, w := range add {
		if !slices.Contains(list, w) {
			list = append(list, w)
		}
	}
	return list
}
