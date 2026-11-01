package helm

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aniket-devop/deployora/internal/domain"
	"github.com/aniket-devop/deployora/internal/proc"
)

type call struct {
	name string
	args []string
	env  []string
}

type fakeRunner struct {
	calls   []call
	handler func(c call) (proc.Result, error)
}

func (f *fakeRunner) run(_ context.Context, spec proc.Spec) (proc.Result, error) {
	c := call{name: spec.Name, args: spec.Args, env: spec.Env}
	f.calls = append(f.calls, c)
	return f.handler(c)
}

type fakeProber struct {
	err   error
	calls int
}

func (p *fakeProber) Ready(context.Context) error {
	p.calls++
	return p.err
}

func newHelm(h func(c call) (proc.Result, error)) (*Helm, *fakeRunner) {
	f := &fakeRunner{handler: h}
	return &Helm{Runner: f.run}, f
}

func reply(res proc.Result) func(call) (proc.Result, error) {
	return func(call) (proc.Result, error) { return res, nil }
}

func testSpec(t *testing.T) ChartSpec {
	t.Helper()
	dir := t.TempDir()
	return ChartSpec{
		Path:      filepath.Join(dir, "chart"),
		Release:   "app",
		Namespace: "app-ns",
		Values:    []string{filepath.Join(dir, "a.yaml"), filepath.Join(dir, "b.yaml")},
	}
}

var forbidden = []string{"--set", "--set-string", "--set-file", "--set-json", "--wait", "--atomic", "--rollback-on-failure"}

func assertSafe(t *testing.T, c call) {
	t.Helper()
	if c.name != "helm" {
		t.Errorf("ran %q, want helm", c.name)
	}
	for _, a := range c.args {
		if slices.Contains(forbidden, a) {
			t.Errorf("forbidden flag %s in %v", a, c.args)
		}
	}
	for _, e := range redirectEnv {
		if !slices.Contains(c.env, e) {
			t.Errorf("environment %q is not emptied", e)
		}
	}
}

func TestReleaseName(t *testing.T) {
	tests := []struct {
		in   string
		want string
		err  bool
	}{
		{"app", "app", false},
		{"My_Chart", "my-chart", false},
		{strings.Repeat("a", 60), strings.Repeat("a", 53), false},
		{"-abc-", "abc", false},
		{strings.Repeat("a", 52) + "-b", strings.Repeat("a", 52), false},
		{"", "", true},
		{"---", "", true},
	}
	for _, tt := range tests {
		got, err := ReleaseName(tt.in)
		if tt.err {
			if !errors.Is(err, ErrInvalid) {
				t.Errorf("ReleaseName(%q): got %v, want ErrInvalid", tt.in, err)
			}
			continue
		}
		if err != nil || got != tt.want {
			t.Errorf("ReleaseName(%q) = %q, %v, want %q", tt.in, got, err, tt.want)
		}
	}
}

func TestTemplateArgsAndOutput(t *testing.T) {
	h, f := newHelm(reply(proc.Result{Stdout: []byte("kind: Service\n")}))
	spec := testSpec(t)
	out, sr := h.Template(context.Background(), spec)
	if sr.Outcome != domain.OutcomePassed || string(out) != "kind: Service\n" {
		t.Fatalf("got out=%q sr=%+v", out, sr)
	}
	want := []string{"template", "app", spec.Path, "--namespace", "app-ns", "--include-crds",
		"--values", spec.Values[0], "--values", spec.Values[1]}
	if len(f.calls) != 1 || !slices.Equal(f.calls[0].args, want) {
		t.Fatalf("args\n got  %v\n want %v", f.calls, want)
	}
	assertSafe(t, f.calls[0])
}

func TestTemplateFailures(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	tests := []struct {
		name        string
		ctx         context.Context
		values      bool
		handler     func(call) (proc.Result, error)
		wantOutcome domain.Outcome
		wantReason  domain.Reason
		wantInErr   string
		notInErr    string
	}{
		{"render fails without values", context.Background(), false,
			reply(proc.Result{ExitCode: 1, Stderr: []byte("Error: required value missing")}),
			domain.OutcomeFailed, domain.ReasonRenderFailed, "--values", ""},
		{"render fails with values", context.Background(), true,
			reply(proc.Result{ExitCode: 1, Stderr: []byte("Error: bad template")}),
			domain.OutcomeFailed, domain.ReasonRenderFailed, "bad template", "pass them with --values"},
		{"output too large", context.Background(), true,
			reply(proc.Result{Stdout: []byte("x"), Truncated: true}),
			domain.OutcomeErrored, domain.ReasonEnvironmentError, "larger than", ""},
		{"helm cannot start", context.Background(), true,
			func(call) (proc.Result, error) {
				return proc.Result{ExitCode: -1}, errors.New("executable file not found")
			},
			domain.OutcomeErrored, domain.ReasonPrerequisiteMissing, "not found", ""},
		{"context cancelled", cancelled, true,
			reply(proc.Result{ExitCode: -1, Canceled: true}),
			domain.OutcomeErrored, domain.ReasonInternalError, "canceled", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, _ := newHelm(tt.handler)
			spec := testSpec(t)
			if !tt.values {
				spec.Values = nil
			}
			out, sr := h.Template(tt.ctx, spec)
			if out != nil || sr.Outcome != tt.wantOutcome || sr.Reason != tt.wantReason || sr.Err == nil {
				t.Fatalf("got out=%q sr=%+v", out, sr)
			}
			if !strings.Contains(sr.Err.Error(), tt.wantInErr) {
				t.Errorf("error %q does not contain %q", sr.Err, tt.wantInErr)
			}
			if tt.notInErr != "" && strings.Contains(sr.Err.Error(), tt.notInErr) {
				t.Errorf("error %q must not contain %q", sr.Err, tt.notInErr)
			}
		})
	}
	h, _ := newHelm(reply(proc.Result{ExitCode: -1, Canceled: true}))
	if _, sr := h.Template(cancelled, testSpec(t)); !errors.Is(sr.Err, context.Canceled) {
		t.Errorf("cancelled run: error %v does not wrap context.Canceled", sr.Err)
	}
}

func TestRejectsInvalidRequestsBeforeRunning(t *testing.T) {
	mutations := map[string]func(*ChartSpec){
		"relative chart path":  func(s *ChartSpec) { s.Path = "chart" },
		"uppercase release":    func(s *ChartSpec) { s.Release = "App" },
		"empty release":        func(s *ChartSpec) { s.Release = "" },
		"long release":         func(s *ChartSpec) { s.Release = strings.Repeat("a", 54) },
		"flag-like namespace":  func(s *ChartSpec) { s.Namespace = "--set" },
		"empty namespace":      func(s *ChartSpec) { s.Namespace = "" },
		"relative values file": func(s *ChartSpec) { s.Values = []string{"values.yaml"} },
		"url as values file":   func(s *ChartSpec) { s.Values = []string{"https://example.invalid/values.yaml"} },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			h, f := newHelm(reply(proc.Result{}))
			spec := testSpec(t)
			mutate(&spec)
			if _, sr := h.Template(context.Background(), spec); sr.Outcome != domain.OutcomeInvalidConfig || !errors.Is(sr.Err, ErrInvalid) {
				t.Errorf("Template: got %+v", sr)
			}
			kc := filepath.Join(t.TempDir(), "kubeconfig")
			if sr := h.Install(context.Background(), kc, spec); sr.Outcome != domain.OutcomeInvalidConfig {
				t.Errorf("Install: got %+v", sr)
			}
			if len(f.calls) != 0 {
				t.Errorf("ran %d commands for an invalid request", len(f.calls))
			}
		})
	}

	h, f := newHelm(reply(proc.Result{}))
	if sr := h.Install(context.Background(), "kubeconfig", testSpec(t)); sr.Outcome != domain.OutcomeInvalidConfig || len(f.calls) != 0 {
		t.Errorf("relative kubeconfig: got %+v after %d commands", sr, len(f.calls))
	}
}

func TestInstallArgs(t *testing.T) {
	spec := testSpec(t)
	kc := filepath.Join(t.TempDir(), "kubeconfig")
	base := []string{"install", "app", spec.Path, "--namespace", "app-ns", "--create-namespace",
		"--kubeconfig", kc, "--values", spec.Values[0], "--values", spec.Values[1]}

	h, f := newHelm(reply(proc.Result{}))
	if sr := h.Install(context.Background(), kc, spec); sr.Outcome != domain.OutcomePassed {
		t.Fatalf("got %+v", sr)
	}
	if !slices.Equal(f.calls[0].args, base) {
		t.Errorf("args\n got  %v\n want %v", f.calls[0].args, base)
	}
	assertSafe(t, f.calls[0])

	h, f = newHelm(reply(proc.Result{}))
	h.Timeout = 90 * time.Second
	h.Install(context.Background(), kc, spec)
	if want := append(slices.Clone(base), "--timeout", "1m30s"); !slices.Equal(f.calls[0].args, want) {
		t.Errorf("args with timeout\n got  %v\n want %v", f.calls[0].args, want)
	}
}

func TestInstallClassification(t *testing.T) {
	tests := []struct {
		name        string
		res         proc.Result
		probeErr    error
		wantOutcome domain.Outcome
		wantReason  domain.Reason
		wantProbe   bool
	}{
		{"success", proc.Result{}, nil, domain.OutcomePassed, "", false},
		{"chart problem", proc.Result{ExitCode: 1, Stderr: []byte("Error: INSTALLATION FAILED: Deployment.apps is invalid")},
			nil, domain.OutcomeFailed, domain.ReasonInvalidWorkloadConfig, true},
		{"API server unhealthy", proc.Result{ExitCode: 1, Stderr: []byte("Error: connection refused")},
			errors.New("connection refused"), domain.OutcomeErrored, domain.ReasonAPIUnavailable, true},
		{"webhook call failure in stderr", proc.Result{ExitCode: 1, Stderr: []byte(`Internal error occurred: failed calling webhook "x.example.com": dial tcp: connection refused`)},
			nil, domain.OutcomeErrored, domain.ReasonWebhookUnavailable, true},
		{"webhook call failure in stdout", proc.Result{ExitCode: 1, Stdout: []byte(`failed calling webhook "x"`)},
			nil, domain.OutcomeErrored, domain.ReasonWebhookUnavailable, true},
		{"unhealthy API server wins over a webhook message", proc.Result{ExitCode: 1, Stderr: []byte(`failed calling webhook "x"`)},
			errors.New("timeout"), domain.OutcomeErrored, domain.ReasonAPIUnavailable, true},
		{"helm killed by a signal", proc.Result{ExitCode: -1, Signaled: true},
			nil, domain.OutcomeErrored, domain.ReasonEnvironmentError, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, _ := newHelm(reply(tt.res))
			p := &fakeProber{err: tt.probeErr}
			h.Prober = p
			sr := h.Install(context.Background(), filepath.Join(t.TempDir(), "kubeconfig"), testSpec(t))
			if sr.Outcome != tt.wantOutcome || sr.Reason != tt.wantReason {
				t.Fatalf("got %+v", sr)
			}
			if (p.calls > 0) != tt.wantProbe {
				t.Errorf("probe called %d times, want called=%v", p.calls, tt.wantProbe)
			}
			if tt.wantOutcome != domain.OutcomePassed && sr.Err == nil {
				t.Error("a non-passing result must carry an error")
			}
		})
	}
}

func TestInstallFailureWithoutProber(t *testing.T) {
	h, _ := newHelm(reply(proc.Result{ExitCode: 1, Stderr: []byte("boom")}))
	sr := h.Install(context.Background(), filepath.Join(t.TempDir(), "kubeconfig"), testSpec(t))
	if sr.Outcome != domain.OutcomeErrored || sr.Reason != domain.ReasonInternalError {
		t.Errorf("got %+v", sr)
	}
}

func TestInstallCanceledAndMissingBinary(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	h, _ := newHelm(reply(proc.Result{ExitCode: -1, Canceled: true}))
	h.Prober = &fakeProber{}
	sr := h.Install(cancelled, filepath.Join(t.TempDir(), "kubeconfig"), testSpec(t))
	if sr.Outcome == domain.OutcomePassed || !errors.Is(sr.Err, context.Canceled) {
		t.Errorf("cancelled run: got %+v", sr)
	}

	h, _ = newHelm(func(call) (proc.Result, error) { return proc.Result{ExitCode: -1}, errors.New("not found") })
	sr = h.Install(context.Background(), filepath.Join(t.TempDir(), "kubeconfig"), testSpec(t))
	if sr.Outcome != domain.OutcomeErrored || sr.Reason != domain.ReasonPrerequisiteMissing {
		t.Errorf("missing binary: got %+v", sr)
	}
}
