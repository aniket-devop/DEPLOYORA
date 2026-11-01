// Package validate runs kubeconform on the manifests a run is about to deploy.
//
// Rendered Helm output is passed on standard input, so it never touches disk.
// kubeconform is always run with -ignore-missing-schemas and -verbose, so that
// resources without a schema (CRDs) are reported by name as skipped instead of
// failing the run. Skipped resources are recorded in the Report; with
// StrictSchemas they fail the run.
//
// Not yet verified against a real kubeconform: what it reports when the schema
// source cannot be reached. Any status that is not valid, invalid or skipped is
// treated as an error, so such a failure can never pass silently.
package validate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/aniket-devop/deployora/internal/domain"
	"github.com/aniket-devop/deployora/internal/proc"
)

// ErrInvalid is wrapped by every error that rejects a request before kubeconform runs.
var ErrInvalid = errors.New("invalid validate request")

// Runner runs one external command. proc.Run is the real one; tests supply a fake.
type Runner func(ctx context.Context, spec proc.Spec) (proc.Result, error)

const (
	defaultMaxOutput = 16 << 20
	stderrTail       = 2048
	maxMessage       = 2048
	describeLimit    = 3
)

var versionRE = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

// Validator runs the kubeconform executable. The zero value is ready to use.
type Validator struct {
	// Runner runs commands. Nil means proc.Run.
	Runner Runner
	// Bin names the executable. Empty means "kubeconform".
	Bin string
	// Grace is passed to proc.Spec. Zero means proc.DefaultGrace.
	Grace time.Duration
	// MaxOutput caps kubeconform's output, in bytes. Zero means 16 MiB.
	MaxOutput int
}

// Spec describes one validation. Give either Stdin (rendered manifests) or Files.
type Spec struct {
	// KubernetesVersion is the version to validate against, as X.Y.Z. It should
	// be the version of the cluster the run deploys to.
	KubernetesVersion string
	// CacheDir is an absolute directory where downloaded schemas are cached. It
	// is created if it does not exist.
	CacheDir string
	// Stdin holds the manifests to validate. Non-nil means "validate standard input".
	Stdin io.Reader
	// Files are absolute paths of manifest files to validate.
	Files []string
	// StrictSchemas makes resources without a schema fail the run.
	StrictSchemas bool
}

func (s Spec) validate() error {
	if !versionRE.MatchString(s.KubernetesVersion) {
		return fmt.Errorf("%w: Kubernetes version %q must look like 1.31.0", ErrInvalid, s.KubernetesVersion)
	}
	if !filepath.IsAbs(s.CacheDir) {
		return fmt.Errorf("%w: cache directory %q must be absolute", ErrInvalid, s.CacheDir)
	}
	if (s.Stdin != nil) == (len(s.Files) > 0) {
		return fmt.Errorf("%w: give either Stdin or Files, not both and not neither", ErrInvalid)
	}
	for _, f := range s.Files {
		if !filepath.IsAbs(f) {
			return fmt.Errorf("%w: manifest file %q must be an absolute path", ErrInvalid, f)
		}
	}
	return nil
}

// Status is the validation status of one resource.
type Status string

const (
	StatusValid   Status = "valid"
	StatusInvalid Status = "invalid"
	StatusSkipped Status = "skipped"
	StatusError   Status = "error"
)

// FieldError is one schema violation inside a resource.
type FieldError struct {
	Path    string
	Message string
}

// Resource is the result for one Kubernetes object.
type Resource struct {
	// File is the file name, or "stdin" for rendered manifests.
	File       string
	Kind       string
	Name       string
	APIVersion string
	Status     Status
	Message    string
	Errors     []FieldError
}

// Report is what kubeconform found. The counts come from kubeconform's summary.
type Report struct {
	Resources []Resource
	Valid     int
	Invalid   int
	Errors    int
	Skipped   int
}

// Count returns the number of resources kubeconform looked at. Zero means the
// input held no Kubernetes objects.
func (r Report) Count() int { return r.Valid + r.Invalid + r.Errors + r.Skipped }

// Validate runs kubeconform and classifies the result.
func (v *Validator) Validate(ctx context.Context, spec Spec) (Report, domain.StageResult) {
	if err := spec.validate(); err != nil {
		return Report{}, fail(domain.OutcomeInvalidConfig, domain.ReasonConfigInvalid, err)
	}
	if err := os.MkdirAll(spec.CacheDir, 0o700); err != nil {
		return Report{}, fail(domain.OutcomeErrored, domain.ReasonEnvironmentError,
			fmt.Errorf("cannot create the schema cache directory: %w", err))
	}

	args := []string{"-output", "json", "-summary", "-verbose", "-ignore-missing-schemas",
		"-kubernetes-version", spec.KubernetesVersion, "-cache", spec.CacheDir}
	if spec.Stdin != nil {
		args = append(args, "-")
	} else {
		args = append(args, spec.Files...)
	}

	runner := v.Runner
	if runner == nil {
		runner = proc.Run
	}
	bin := v.Bin
	if bin == "" {
		bin = "kubeconform"
	}
	limit := v.MaxOutput
	if limit <= 0 {
		limit = defaultMaxOutput
	}
	res, err := runner(ctx, proc.Spec{Name: bin, Args: args, Stdin: spec.Stdin, Grace: v.Grace, MaxOutput: limit})

	switch {
	case res.Canceled || (err != nil && ctx.Err() != nil):
		return Report{}, fail(domain.OutcomeErrored, domain.ReasonInternalError,
			fmt.Errorf("kubeconform: %w", ctxCause(ctx)))
	case err != nil:
		return Report{}, fail(domain.OutcomeErrored, domain.ReasonPrerequisiteMissing,
			fmt.Errorf("kubeconform: %w", err))
	case res.Signaled:
		return Report{}, fail(domain.OutcomeErrored, domain.ReasonEnvironmentError,
			fmt.Errorf("kubeconform was ended by a signal: %s", tail(res.Stderr)))
	case res.Truncated:
		return Report{}, fail(domain.OutcomeErrored, domain.ReasonEnvironmentError,
			fmt.Errorf("kubeconform output is larger than %d MiB", limit>>20))
	case res.ExitCode != 0 && res.ExitCode != 1:
		return Report{}, fail(domain.OutcomeErrored, domain.ReasonEnvironmentError,
			fmt.Errorf("kubeconform failed (exit code %d): %s", res.ExitCode, tail(res.Stderr)))
	}

	var out kcOutput
	if err := json.Unmarshal(res.Stdout, &out); err != nil {
		return Report{}, fail(domain.OutcomeErrored, domain.ReasonEnvironmentError,
			fmt.Errorf("cannot read kubeconform output (exit code %d): %v: %s", res.ExitCode, err, tail(res.Stderr)))
	}
	rep := buildReport(out)

	switch {
	case rep.Invalid > 0:
		return rep, fail(domain.OutcomeFailed, domain.ReasonValidationFailed,
			fmt.Errorf("%d resource(s) failed validation: %s", rep.Invalid, describe(rep, StatusInvalid)))
	case rep.Errors > 0:
		return rep, fail(domain.OutcomeErrored, domain.ReasonEnvironmentError,
			fmt.Errorf("kubeconform could not validate %d resource(s): %s", rep.Errors, describe(rep, StatusError)))
	case res.ExitCode == 1:
		return rep, fail(domain.OutcomeErrored, domain.ReasonEnvironmentError,
			errors.New("kubeconform exited with code 1 but reported no invalid or failed resource"))
	case rep.Valid == 0 && rep.Skipped > 0:
		return rep, fail(domain.OutcomeErrored, domain.ReasonEnvironmentError,
			fmt.Errorf("no schema was found for any of the %d resource(s); check the Kubernetes version and the schema source", rep.Skipped))
	case rep.Skipped > 0 && spec.StrictSchemas:
		return rep, fail(domain.OutcomeFailed, domain.ReasonSchemaMissing,
			fmt.Errorf("%d resource(s) have no schema: %s", rep.Skipped, describe(rep, StatusSkipped)))
	}
	return rep, domain.StageResult{Outcome: domain.OutcomePassed}
}

// kcOutput mirrors kubeconform's JSON output.
type kcOutput struct {
	Resources []kcResource `json:"resources"`
	Summary   kcSummary    `json:"summary"`
}

type kcResource struct {
	Filename         string         `json:"filename"`
	Kind             string         `json:"kind"`
	Name             string         `json:"name"`
	Version          string         `json:"version"`
	Status           string         `json:"status"`
	Msg              string         `json:"msg"`
	ValidationErrors []kcFieldError `json:"validationErrors"`
}

type kcFieldError struct {
	Path string `json:"path"`
	Msg  string `json:"msg"`
}

type kcSummary struct {
	Valid   int `json:"valid"`
	Invalid int `json:"invalid"`
	Errors  int `json:"errors"`
	Skipped int `json:"skipped"`
}

func buildReport(out kcOutput) Report {
	r := Report{Valid: out.Summary.Valid, Invalid: out.Summary.Invalid, Errors: out.Summary.Errors, Skipped: out.Summary.Skipped}
	for _, kr := range out.Resources {
		res := Resource{File: kr.Filename, Kind: kr.Kind, Name: kr.Name, APIVersion: kr.Version, Message: clip(kr.Msg)}
		switch kr.Status {
		case "statusValid":
			res.Status = StatusValid
		case "statusInvalid":
			res.Status = StatusInvalid
		case "statusSkipped":
			res.Status = StatusSkipped
		case "statusError":
			res.Status = StatusError
		default:
			res.Status = StatusError
			res.Message = clip(fmt.Sprintf("unknown kubeconform status %q: %s", kr.Status, kr.Msg))
			r.Errors++
		}
		for _, fe := range kr.ValidationErrors {
			res.Errors = append(res.Errors, FieldError{Path: fe.Path, Message: clip(fe.Msg)})
		}
		r.Resources = append(r.Resources, res)
	}
	return r
}

// describe names the first few resources with the given status, with their
// first problem, for use in an error message.
func describe(r Report, st Status) string {
	var parts []string
	for _, res := range r.Resources {
		if res.Status != st {
			continue
		}
		if len(parts) == describeLimit {
			parts = append(parts, "...")
			break
		}
		p := res.Kind + "/" + res.Name
		switch {
		case len(res.Errors) > 0:
			p += ": " + res.Errors[0].Path + " " + res.Errors[0].Message
		case res.Message != "":
			p += ": " + res.Message
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, "; ")
}

func fail(o domain.Outcome, r domain.Reason, err error) domain.StageResult {
	return domain.StageResult{Outcome: o, Reason: r, Err: err}
}

func ctxCause(ctx context.Context) error {
	if c := context.Cause(ctx); c != nil {
		return c
	}
	return context.Canceled
}

func clip(s string) string {
	if len(s) > maxMessage {
		s = strings.ToValidUTF8(s[:maxMessage], "") + "..."
	}
	return s
}

// tail returns the end of a captured stream as a short, valid UTF-8 string.
func tail(b []byte) string {
	if len(b) > stderrTail {
		b = b[len(b)-stderrTail:]
	}
	return strings.TrimSpace(strings.ToValidUTF8(string(b), ""))
}
