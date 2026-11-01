// Package helm runs the Helm command line tool on behalf of the engine: it
// renders a chart in memory and installs it into the run's cluster.
//
// Rules (docs/architecture/overview.md section 6.3 and the locked v0.1.0
// decisions): values are passed by file only, there is no --set, and Helm is
// never asked to wait or to roll back, so a failed install leaves its evidence
// in the cluster. Helm reports errors as text, so a failed install is classified
// from the exit code, a probe of the API server and one message shape.
package helm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/aniket-devop/deployora/internal/domain"
	"github.com/aniket-devop/deployora/internal/proc"
)

// ErrInvalid is wrapped by every error that rejects a request before Helm runs.
var ErrInvalid = errors.New("invalid helm request")

// Runner runs one external command. proc.Run is the real one; tests supply a fake.
type Runner func(ctx context.Context, spec proc.Spec) (proc.Result, error)

// Prober reports whether the cluster's API server is healthy (the /readyz probe).
// It returns nil when the API server is healthy. The real implementation comes
// with the Kubernetes client; tests supply a fake.
type Prober interface {
	Ready(ctx context.Context) error
}

const (
	defaultMaxRender = 64 << 20
	probeTimeout     = 15 * time.Second
	stderrTail       = 2048
	maxReleaseLen    = 53
	maxNamespaceLen  = 63

	// webhookShape is the one message shape that is looked for in Helm's text:
	// the API server relays it when an admission webhook cannot be called.
	webhookShape = "failed calling webhook"
)

// redirectEnv empties Helm environment variables that could point it at another
// cluster or namespace. They are appended after the inherited environment, where
// the last value wins.
var redirectEnv = []string{
	"HELM_KUBEAPISERVER=", "HELM_KUBEASGROUPS=", "HELM_KUBEASUSER=", "HELM_KUBECAFILE=",
	"HELM_KUBECONTEXT=", "HELM_KUBEINSECURE_SKIP_TLS_VERIFY=", "HELM_KUBETLS_SERVER_NAME=", "HELM_KUBETOKEN=", "HELM_NAMESPACE=",
}

var labelRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// Helm runs the helm executable. The zero value is usable except for Prober,
// which Install needs.
type Helm struct {
	// Runner runs commands. Nil means proc.Run.
	Runner Runner
	// Bin names the executable. Empty means "helm".
	Bin string
	// Prober is used after a failed install. Nil makes a failed install errored.
	Prober Prober
	// Grace is passed to proc.Spec. Zero means proc.DefaultGrace.
	Grace time.Duration
	// MaxRender caps the rendered chart, in bytes. Zero means 64 MiB.
	MaxRender int
	// Timeout is passed to helm install as --timeout when positive. Zero leaves
	// Helm's own default.
	Timeout time.Duration
}

// ChartSpec describes one chart to render or install.
type ChartSpec struct {
	// Path is the absolute path of the chart directory.
	Path string
	// Release is the release name (see ReleaseName).
	Release string
	// Namespace is the namespace to render or install into.
	Namespace string
	// Values are absolute paths of values files, in priority order. URLs are not accepted.
	Values []string
}

func (s ChartSpec) validate() error {
	if !filepath.IsAbs(s.Path) {
		return fmt.Errorf("%w: chart path %q must be absolute", ErrInvalid, s.Path)
	}
	if len(s.Release) > maxReleaseLen || !labelRE.MatchString(s.Release) {
		return fmt.Errorf("%w: release name %q must be lowercase letters, digits and hyphens, at most %d characters", ErrInvalid, s.Release, maxReleaseLen)
	}
	if len(s.Namespace) > maxNamespaceLen || !labelRE.MatchString(s.Namespace) {
		return fmt.Errorf("%w: namespace %q must be lowercase letters, digits and hyphens, at most %d characters", ErrInvalid, s.Namespace, maxNamespaceLen)
	}
	for _, v := range s.Values {
		if !filepath.IsAbs(v) {
			return fmt.Errorf("%w: values file %q must be an absolute path to a file (URLs are not accepted)", ErrInvalid, v)
		}
	}
	return nil
}

// CommandError describes a Helm command that ended with a non-zero exit code.
type CommandError struct {
	Op       string
	ExitCode int
	// Stderr is the tail of the command's standard error.
	Stderr string
	// Hint is a suggestion for the user, if there is one.
	Hint string
}

func (e *CommandError) Error() string {
	msg := fmt.Sprintf("%s failed (exit code %d)", e.Op, e.ExitCode)
	if e.Stderr != "" {
		msg += ": " + e.Stderr
	}
	if e.Hint != "" {
		msg += " (" + e.Hint + ")"
	}
	return msg
}

// Template renders the chart with helm template and returns the manifests. The
// output stays in memory. A chart that does not render is failed/render_failed.
func (h *Helm) Template(ctx context.Context, spec ChartSpec) ([]byte, domain.StageResult) {
	if err := spec.validate(); err != nil {
		return nil, domain.StageResult{Outcome: domain.OutcomeInvalidConfig, Reason: domain.ReasonConfigInvalid, Err: err}
	}
	args := []string{"template", spec.Release, spec.Path, "--namespace", spec.Namespace, "--include-crds"}
	args = append(args, valueArgs(spec.Values)...)

	limit := h.MaxRender
	if limit <= 0 {
		limit = defaultMaxRender
	}
	res, err := h.run(ctx, limit, args...)
	if sr, ran := started(ctx, "helm template", res, err); !ran {
		return nil, sr
	}
	if res.ExitCode != 0 {
		cerr := &CommandError{Op: "helm template", ExitCode: res.ExitCode, Stderr: tail(res.Stderr)}
		if len(spec.Values) == 0 {
			cerr.Hint = "if the chart needs values, pass them with --values"
		}
		return nil, domain.StageResult{Outcome: domain.OutcomeFailed, Reason: domain.ReasonRenderFailed, Err: cerr}
	}
	if res.Truncated {
		return nil, domain.StageResult{
			Outcome: domain.OutcomeErrored,
			Reason:  domain.ReasonEnvironmentError,
			Err:     fmt.Errorf("helm template output is larger than %d MiB", limit>>20),
		}
	}
	return res.Stdout, domain.StageResult{Outcome: domain.OutcomePassed}
}

// Install runs helm install against the cluster in kubeconfig. It never passes
// --set, --wait, --atomic or --rollback-on-failure.
func (h *Helm) Install(ctx context.Context, kubeconfig string, spec ChartSpec) domain.StageResult {
	if err := spec.validate(); err != nil {
		return domain.StageResult{Outcome: domain.OutcomeInvalidConfig, Reason: domain.ReasonConfigInvalid, Err: err}
	}
	if !filepath.IsAbs(kubeconfig) {
		err := fmt.Errorf("%w: kubeconfig path %q must be absolute", ErrInvalid, kubeconfig)
		return domain.StageResult{Outcome: domain.OutcomeInvalidConfig, Reason: domain.ReasonConfigInvalid, Err: err}
	}
	args := []string{"install", spec.Release, spec.Path,
		"--namespace", spec.Namespace, "--create-namespace", "--kubeconfig", kubeconfig}
	args = append(args, valueArgs(spec.Values)...)
	if h.Timeout > 0 {
		args = append(args, "--timeout", h.Timeout.String())
	}

	res, err := h.run(ctx, 0, args...)
	if sr, ran := started(ctx, "helm install", res, err); !ran {
		return sr
	}
	if res.ExitCode == 0 {
		return domain.StageResult{Outcome: domain.OutcomePassed}
	}
	cerr := &CommandError{Op: "helm install", ExitCode: res.ExitCode, Stderr: tail(res.Stderr)}

	switch {
	case res.Signaled:
		return domain.StageResult{Outcome: domain.OutcomeErrored, Reason: domain.ReasonEnvironmentError,
			Err: fmt.Errorf("helm was ended by a signal: %w", cerr)}
	case h.Prober == nil:
		return domain.StageResult{Outcome: domain.OutcomeErrored, Reason: domain.ReasonInternalError,
			Err: fmt.Errorf("no API server probe is configured to classify the failure: %w", cerr)}
	}

	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	perr := h.Prober.Ready(pctx)
	cancel()
	if ctx.Err() != nil {
		return domain.StageResult{Outcome: domain.OutcomeErrored, Reason: domain.ReasonInternalError,
			Err: fmt.Errorf("helm install: %w", ctxCause(ctx))}
	}
	switch {
	case perr != nil:
		return domain.StageResult{Outcome: domain.OutcomeErrored, Reason: domain.ReasonAPIUnavailable,
			Err: fmt.Errorf("%w; API server probe: %v", cerr, perr)}
	case bytes.Contains(res.Stderr, []byte(webhookShape)) || bytes.Contains(res.Stdout, []byte(webhookShape)):
		return domain.StageResult{Outcome: domain.OutcomeErrored, Reason: domain.ReasonWebhookUnavailable, Err: cerr}
	default:
		return domain.StageResult{Outcome: domain.OutcomeFailed, Reason: domain.ReasonInvalidWorkloadConfig, Err: cerr}
	}
}

func (h *Helm) run(ctx context.Context, maxOutput int, args ...string) (proc.Result, error) {
	r := h.Runner
	if r == nil {
		r = proc.Run
	}
	bin := h.Bin
	if bin == "" {
		bin = "helm"
	}
	return r(ctx, proc.Spec{Name: bin, Args: args, Env: redirectEnv, Grace: h.Grace, MaxOutput: maxOutput})
}

// started handles the two ways a command can end before it produced an exit
// code: the context ended, or the executable could not be started. It reports
// whether the command ran to completion. For a cancelled context the outcome is
// a placeholder: the engine decides canceled or timed_out from the context.
func started(ctx context.Context, op string, res proc.Result, err error) (domain.StageResult, bool) {
	switch {
	case res.Canceled || (err != nil && ctx.Err() != nil):
		return domain.StageResult{Outcome: domain.OutcomeErrored, Reason: domain.ReasonInternalError,
			Err: fmt.Errorf("%s: %w", op, ctxCause(ctx))}, false
	case err != nil:
		return domain.StageResult{Outcome: domain.OutcomeErrored, Reason: domain.ReasonPrerequisiteMissing,
			Err: fmt.Errorf("%s: %w", op, err)}, false
	}
	return domain.StageResult{}, true
}

func valueArgs(files []string) []string {
	var args []string
	for _, f := range files {
		args = append(args, "--values", f)
	}
	return args
}

func ctxCause(ctx context.Context) error {
	if c := context.Cause(ctx); c != nil {
		return c
	}
	return context.Canceled
}

// tail returns the end of a captured stream as a short, valid UTF-8 string.
func tail(b []byte) string {
	if len(b) > stderrTail {
		b = b[len(b)-stderrTail:]
	}
	return strings.TrimSpace(strings.ToValidUTF8(string(b), ""))
}
