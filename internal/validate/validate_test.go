package validate

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/aniket-devop/deployora/internal/domain"
	"github.com/aniket-devop/deployora/internal/proc"
)

// Captured from kubeconform v0.8.0 (see docs/spikes), then shortened to one line.
const jsonInvalid = `{"resources":[{"filename":"stdin","kind":"Deployment","name":"bad","version":"apps/v1","status":"statusInvalid","msg":"problem validating schema. Check JSON formatting: jsonschema validation failed with 'https://raw.githubusercontent.com/yannh/kubernetes-json-schema/master/v1.31.0-standalone/deployment-apps-v1.json#' - at '/spec': missing properties 'selector', 'template' - at '/spec/replicas': got string, want null or integer","validationErrors":[{"path":"/spec","msg":"missing properties 'selector', 'template'"},{"path":"/spec/replicas","msg":"got string, want null or integer"}]}],"summary":{"valid":0,"invalid":1,"errors":0,"skipped":0}}`

const jsonMixed = `{"resources":[{"filename":"crd.yaml","kind":"Widget","name":"w","version":"example.com/v1","status":"statusError","msg":"could not find schema for Widget"},{"filename":"bad.yaml","kind":"Deployment","name":"bad","version":"apps/v1","status":"statusInvalid","msg":"problem validating schema","validationErrors":[{"path":"/spec","msg":"missing properties 'selector', 'template'"},{"path":"/spec/replicas","msg":"got string, want null or integer"}]}],"summary":{"valid":1,"invalid":1,"errors":1,"skipped":0}}`

const jsonVerboseSkipped = `{"resources":[{"filename":"crd.yaml","kind":"Widget","name":"w","version":"example.com/v1","status":"statusSkipped","msg":""},{"filename":"ok.yaml","kind":"ConfigMap","name":"ok","version":"v1","status":"statusValid","msg":""}],"summary":{"valid":1,"invalid":0,"errors":0,"skipped":1}}`

// The next four are constructed from the shapes above, not captured.
const jsonErrorOnly = `{"resources":[{"filename":"crd.yaml","kind":"Widget","name":"w","version":"example.com/v1","status":"statusError","msg":"could not find schema for Widget"}],"summary":{"valid":0,"invalid":0,"errors":1,"skipped":0}}`
const jsonAllSkipped = `{"resources":[{"filename":"stdin","kind":"Widget","name":"w","version":"example.com/v1","status":"statusSkipped","msg":""}],"summary":{"valid":0,"invalid":0,"errors":0,"skipped":1}}`
const jsonEmpty = `{"resources":[],"summary":{"valid":0,"invalid":0,"errors":0,"skipped":0}}`
const jsonUnknownStatus = `{"resources":[{"filename":"stdin","kind":"Pod","name":"p","version":"v1","status":"statusFuture","msg":"new"}],"summary":{"valid":0,"invalid":0,"errors":0,"skipped":0}}`

type fake struct {
	calls     int
	spec      proc.Spec
	stdinSeen string
	res       proc.Result
	err       error
}

func (f *fake) run(_ context.Context, s proc.Spec) (proc.Result, error) {
	f.calls++
	f.spec = s
	if s.Stdin != nil {
		b, _ := io.ReadAll(s.Stdin)
		f.stdinSeen = string(b)
	}
	return f.res, f.err
}

func newValidator(res proc.Result, err error) (*Validator, *fake) {
	f := &fake{res: res, err: err}
	return &Validator{Runner: f.run}, f
}

func stdinSpec(t *testing.T) Spec {
	t.Helper()
	return Spec{
		KubernetesVersion: "1.31.0",
		CacheDir:          filepath.Join(t.TempDir(), "cache"),
		Stdin:             strings.NewReader("kind: X"),
	}
}

func TestArgsInStdinMode(t *testing.T) {
	v, f := newValidator(proc.Result{Stdout: []byte(jsonEmpty)}, nil)
	spec := stdinSpec(t)
	if _, sr := v.Validate(context.Background(), spec); sr.Outcome != domain.OutcomePassed {
		t.Fatalf("got %+v", sr)
	}
	want := []string{"-output", "json", "-summary", "-verbose", "-ignore-missing-schemas",
		"-kubernetes-version", "1.31.0", "-cache", spec.CacheDir, "-"}
	if f.spec.Name != "kubeconform" || !slices.Equal(f.spec.Args, want) {
		t.Errorf("command %s %v\n want %v", f.spec.Name, f.spec.Args, want)
	}
	if f.stdinSeen != "kind: X" {
		t.Errorf("stdin was %q", f.stdinSeen)
	}
}

func TestArgsInFilesMode(t *testing.T) {
	v, f := newValidator(proc.Result{Stdout: []byte(jsonEmpty)}, nil)
	dir := t.TempDir()
	spec := Spec{
		KubernetesVersion: "1.36.1",
		CacheDir:          filepath.Join(dir, "cache"),
		Files:             []string{filepath.Join(dir, "a.yaml"), filepath.Join(dir, "b.yaml")},
	}
	if _, sr := v.Validate(context.Background(), spec); sr.Outcome != domain.OutcomePassed {
		t.Fatalf("got %+v", sr)
	}
	want := []string{"-output", "json", "-summary", "-verbose", "-ignore-missing-schemas",
		"-kubernetes-version", "1.36.1", "-cache", spec.CacheDir, spec.Files[0], spec.Files[1]}
	if !slices.Equal(f.spec.Args, want) || f.spec.Stdin != nil {
		t.Errorf("args %v stdin %v\n want %v", f.spec.Args, f.spec.Stdin, want)
	}
}

func TestCacheDirIsCreated(t *testing.T) {
	v, _ := newValidator(proc.Result{Stdout: []byte(jsonEmpty)}, nil)
	spec := stdinSpec(t)
	v.Validate(context.Background(), spec)
	if info, err := os.Stat(spec.CacheDir); err != nil || !info.IsDir() {
		t.Errorf("cache directory was not created: %v", err)
	}
}

func TestOutcomes(t *testing.T) {
	tests := []struct {
		name        string
		res         proc.Result
		strict      bool
		wantOutcome domain.Outcome
		wantReason  domain.Reason
		wantInErr   string
		wantCount   int
	}{
		{"nothing to validate", proc.Result{Stdout: []byte(jsonEmpty)}, false,
			domain.OutcomePassed, "", "", 0},
		{"valid and skipped", proc.Result{Stdout: []byte(jsonVerboseSkipped)}, false,
			domain.OutcomePassed, "", "", 2},
		{"skipped fails when strict", proc.Result{Stdout: []byte(jsonVerboseSkipped)}, true,
			domain.OutcomeFailed, domain.ReasonSchemaMissing, "Widget/w", 2},
		{"invalid", proc.Result{ExitCode: 1, Stdout: []byte(jsonInvalid)}, false,
			domain.OutcomeFailed, domain.ReasonValidationFailed, "/spec missing properties", 1},
		{"invalid wins over error", proc.Result{ExitCode: 1, Stdout: []byte(jsonMixed)}, false,
			domain.OutcomeFailed, domain.ReasonValidationFailed, "Deployment/bad", 3},
		{"error only", proc.Result{ExitCode: 1, Stdout: []byte(jsonErrorOnly)}, false,
			domain.OutcomeErrored, domain.ReasonEnvironmentError, "could not find schema", 1},
		{"everything skipped", proc.Result{Stdout: []byte(jsonAllSkipped)}, false,
			domain.OutcomeErrored, domain.ReasonEnvironmentError, "no schema was found", 1},
		{"unknown status is an error", proc.Result{ExitCode: 1, Stdout: []byte(jsonUnknownStatus)}, false,
			domain.OutcomeErrored, domain.ReasonEnvironmentError, "statusFuture", 1},
		{"exit 1 without findings", proc.Result{ExitCode: 1, Stdout: []byte(jsonEmpty)}, false,
			domain.OutcomeErrored, domain.ReasonEnvironmentError, "no invalid or failed", 0},
		{"unreadable output", proc.Result{ExitCode: 1, Stdout: []byte("not json"), Stderr: []byte("boom")}, false,
			domain.OutcomeErrored, domain.ReasonEnvironmentError, "boom", 0},
		{"unexpected exit code", proc.Result{ExitCode: 2, Stderr: []byte("usage")}, false,
			domain.OutcomeErrored, domain.ReasonEnvironmentError, "exit code 2", 0},
		{"killed by a signal", proc.Result{ExitCode: -1, Signaled: true}, false,
			domain.OutcomeErrored, domain.ReasonEnvironmentError, "signal", 0},
		{"output too large", proc.Result{Stdout: []byte("x"), Truncated: true}, false,
			domain.OutcomeErrored, domain.ReasonEnvironmentError, "larger than", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, _ := newValidator(tt.res, nil)
			spec := stdinSpec(t)
			spec.StrictSchemas = tt.strict
			rep, sr := v.Validate(context.Background(), spec)
			if sr.Outcome != tt.wantOutcome || sr.Reason != tt.wantReason {
				t.Fatalf("got %+v", sr)
			}
			if tt.wantOutcome != domain.OutcomePassed {
				if sr.Err == nil || !strings.Contains(sr.Err.Error(), tt.wantInErr) {
					t.Errorf("error %v does not contain %q", sr.Err, tt.wantInErr)
				}
			}
			if len(rep.Resources) > 0 && rep.Count() != tt.wantCount && tt.name != "unknown status is an error" {
				t.Errorf("Count() = %d, want %d", rep.Count(), tt.wantCount)
			}
		})
	}
}

func TestReportDetails(t *testing.T) {
	v, _ := newValidator(proc.Result{ExitCode: 1, Stdout: []byte(jsonInvalid)}, nil)
	rep, _ := v.Validate(context.Background(), stdinSpec(t))
	if len(rep.Resources) != 1 || rep.Invalid != 1 {
		t.Fatalf("got %+v", rep)
	}
	r := rep.Resources[0]
	if r.File != "stdin" || r.Kind != "Deployment" || r.Name != "bad" || r.APIVersion != "apps/v1" || r.Status != StatusInvalid {
		t.Errorf("resource %+v", r)
	}
	if len(r.Errors) != 2 || r.Errors[0].Path != "/spec" || r.Errors[1].Path != "/spec/replicas" {
		t.Errorf("errors %+v", r.Errors)
	}

	v, _ = newValidator(proc.Result{Stdout: []byte(jsonVerboseSkipped)}, nil)
	rep, _ = v.Validate(context.Background(), stdinSpec(t))
	if rep.Skipped != 1 || rep.Valid != 1 || rep.Resources[0].Kind != "Widget" || rep.Resources[0].Status != StatusSkipped {
		t.Errorf("got %+v", rep)
	}
}

func TestCanceledAndMissingBinary(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	v, _ := newValidator(proc.Result{ExitCode: -1, Canceled: true}, nil)
	if _, sr := v.Validate(cancelled, stdinSpec(t)); sr.Outcome == domain.OutcomePassed || !errors.Is(sr.Err, context.Canceled) {
		t.Errorf("cancelled run: got %+v", sr)
	}

	v, _ = newValidator(proc.Result{ExitCode: -1}, errors.New("executable file not found"))
	_, sr := v.Validate(context.Background(), stdinSpec(t))
	if sr.Outcome != domain.OutcomeErrored || sr.Reason != domain.ReasonPrerequisiteMissing {
		t.Errorf("missing binary: got %+v", sr)
	}
}

func TestRejectsInvalidRequestsBeforeRunning(t *testing.T) {
	abs := filepath.Join(t.TempDir(), "a.yaml")
	mutations := map[string]func(*Spec){
		"short version":   func(s *Spec) { s.KubernetesVersion = "1.31" },
		"v prefix":        func(s *Spec) { s.KubernetesVersion = "v1.31.0" },
		"pre-release":     func(s *Spec) { s.KubernetesVersion = "1.31.0-rc1" },
		"relative cache":  func(s *Spec) { s.CacheDir = "cache" },
		"stdin and files": func(s *Spec) { s.Files = []string{abs} },
		"neither":         func(s *Spec) { s.Stdin = nil },
		"relative file":   func(s *Spec) { s.Stdin = nil; s.Files = []string{"a.yaml"} },
		"flag-like file":  func(s *Spec) { s.Stdin = nil; s.Files = []string{"-output"} },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			v, f := newValidator(proc.Result{Stdout: []byte(jsonEmpty)}, nil)
			spec := stdinSpec(t)
			mutate(&spec)
			_, sr := v.Validate(context.Background(), spec)
			if sr.Outcome != domain.OutcomeInvalidConfig || !errors.Is(sr.Err, ErrInvalid) {
				t.Errorf("got %+v", sr)
			}
			if f.calls != 0 {
				t.Errorf("ran %d commands for an invalid request", f.calls)
			}
		})
	}
}
