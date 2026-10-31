package cluster

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/aniket-devop/deployora/internal/proc"
)

// Runner runs one external command. proc.Run is the real one; tests supply a fake.
type Runner func(ctx context.Context, spec proc.Spec) (proc.Result, error)

const (
	defaultWatchWindow = 60 * time.Second
	defaultWatchEvery  = 5 * time.Second
	stderrTail         = 2048
)

// Kind is a ClusterProvider that drives the kind and docker command line tools.
// The zero value is ready to use.
type Kind struct {
	// Runner runs commands. Nil means proc.Run.
	Runner Runner
	// KindBin and DockerBin name the executables. Empty means "kind" and "docker".
	KindBin, DockerBin string
	// Grace is passed to proc.Spec. Zero means proc.DefaultGrace.
	Grace time.Duration
	// WatchWindow and WatchEvery control how long Delete keeps looking for a late
	// node container after a Create that did not finish normally (the spike saw
	// Docker create the container after kind was interrupted). Zero means 60s and 5s.
	WatchWindow, WatchEvery time.Duration

	mu        sync.Mutex
	unsettled map[string]bool
}

var _ ClusterProvider = (*Kind)(nil)

// CreateError reports that kind create cluster ended with a non-zero exit code.
type CreateError struct {
	Name     string
	ExitCode int
	Signaled bool
	// Stderr is the tail of the command's standard error.
	Stderr string
}

func (e *CreateError) Error() string {
	msg := fmt.Sprintf("kind create cluster %s failed (exit code %d)", e.Name, e.ExitCode)
	if e.Stderr != "" {
		msg += ": " + e.Stderr
	}
	return msg
}

// LeakError reports that something of a cluster was still there after Delete.
type LeakError struct {
	Name string
	// Left names the containers or clusters still present.
	Left []string
	// DeleteErr is the last error from kind delete, if any.
	DeleteErr error
}

func (e *LeakError) Error() string {
	return fmt.Sprintf("cluster %s was not fully removed; still present: %s", e.Name, strings.Join(e.Left, ", "))
}

func (e *LeakError) Unwrap() error { return e.DeleteErr }

// Create runs kind create cluster. It never deletes anything itself: after a
// failed or interrupted Create the engine still calls Delete, which is idempotent.
func (k *Kind) Create(ctx context.Context, spec ClusterSpec) (Cluster, error) {
	if err := spec.Validate(); err != nil {
		return Cluster{}, err
	}
	k.setUnsettled(spec.Name, true)
	res, err := k.run(ctx, k.kindBin(), "create", "cluster",
		"--name", spec.Name, "--image", spec.NodeImage, "--kubeconfig", spec.KubeconfigPath)
	switch {
	case err != nil:
		return Cluster{}, fmt.Errorf("kind create cluster %s: %w", spec.Name, err)
	case res.Canceled:
		return Cluster{}, fmt.Errorf("kind create cluster %s: %w", spec.Name, ctxCause(ctx))
	case res.ExitCode != 0:
		return Cluster{}, &CreateError{
			Name: spec.Name, ExitCode: res.ExitCode, Signaled: res.Signaled, Stderr: tail(res.Stderr),
		}
	}
	k.setUnsettled(spec.Name, false)
	return Cluster{Name: spec.Name, KubeconfigPath: spec.KubeconfigPath}, nil
}

// Delete removes the cluster and verifies that nothing is left. It refuses any
// name without the Deployora prefix before running a single command.
//
// After a normal Create it checks once. After a Create that did not finish
// normally it keeps watching for a late node container for the watch window,
// and deletes once more if one shows up. A failing kind delete is not fatal on
// its own: what matters is whether anything is left.
func (k *Kind) Delete(ctx context.Context, c Cluster) error {
	if err := CheckName(c.Name); err != nil {
		return err
	}
	var delErr error
	remove := func() error {
		_, err := k.runOK(ctx, k.kindBin(), "delete", "cluster", "--name", c.Name, "--kubeconfig", c.KubeconfigPath)
		if err != nil && ctx.Err() != nil {
			return err
		}
		delErr = err
		return nil
	}
	if err := remove(); err != nil {
		return err
	}

	window, every := k.watch()
	watching := k.isUnsettled(c.Name)
	var deadline time.Time
	if watching {
		deadline = time.Now().Add(window)
	}
	retried := false
	for {
		left, err := k.leftovers(ctx, c.Name)
		if err != nil {
			return fmt.Errorf("verifying removal of cluster %s: %w", c.Name, err)
		}
		over := !watching || !time.Now().Before(deadline)
		switch {
		case len(left) > 0 && !retried:
			retried = true
			if err := remove(); err != nil {
				return err
			}
			continue
		case len(left) > 0 && over:
			return &LeakError{Name: c.Name, Left: left, DeleteErr: delErr}
		case len(left) == 0 && over:
			k.forget(c.Name)
			return nil
		}
		if err := sleep(ctx, every); err != nil {
			return err
		}
	}
}

// List returns the Deployora clusters kind knows about. It reads standard output
// only, one cluster name per line, and ignores everything else.
func (k *Kind) List(ctx context.Context) ([]ClusterInfo, error) {
	res, err := k.runOK(ctx, k.kindBin(), "get", "clusters")
	if err != nil {
		return nil, err
	}
	var out []ClusterInfo
	for _, name := range lines(res.Stdout) {
		if CheckName(name) == nil {
			out = append(out, ClusterInfo{Name: name})
		}
	}
	return out, nil
}

// leftovers returns what still exists for the cluster: containers carrying the
// kind cluster label, and the cluster itself if kind still lists it.
func (k *Kind) leftovers(ctx context.Context, name string) ([]string, error) {
	ps, err := k.runOK(ctx, k.dockerBin(), "ps", "-a",
		"--filter", "label=io.x-k8s.kind.cluster="+name, "--format", "{{.Names}}")
	if err != nil {
		return nil, err
	}
	left := lines(ps.Stdout)
	listed, err := k.runOK(ctx, k.kindBin(), "get", "clusters")
	if err != nil {
		return nil, err
	}
	for _, n := range lines(listed.Stdout) {
		if n == name {
			left = append(left, "kind cluster "+name)
			break
		}
	}
	return left, nil
}

func (k *Kind) run(ctx context.Context, name string, args ...string) (proc.Result, error) {
	r := k.Runner
	if r == nil {
		r = proc.Run
	}
	return r(ctx, proc.Spec{Name: name, Args: args, Grace: k.Grace})
}

// runOK runs a command and turns a start failure, a cancellation or a non-zero
// exit code into an error.
func (k *Kind) runOK(ctx context.Context, name string, args ...string) (proc.Result, error) {
	res, err := k.run(ctx, name, args...)
	what := name + " " + args[0]
	switch {
	case err != nil:
		return res, fmt.Errorf("%s: %w", what, err)
	case res.Canceled:
		return res, fmt.Errorf("%s: %w", what, ctxCause(ctx))
	case res.ExitCode != 0:
		return res, fmt.Errorf("%s failed (exit code %d): %s", what, res.ExitCode, tail(res.Stderr))
	}
	return res, nil
}

func (k *Kind) kindBin() string {
	if k.KindBin != "" {
		return k.KindBin
	}
	return "kind"
}

func (k *Kind) dockerBin() string {
	if k.DockerBin != "" {
		return k.DockerBin
	}
	return "docker"
}

func (k *Kind) watch() (window, every time.Duration) {
	window, every = k.WatchWindow, k.WatchEvery
	if window <= 0 {
		window = defaultWatchWindow
	}
	if every <= 0 {
		every = defaultWatchEvery
	}
	return window, every
}

func (k *Kind) setUnsettled(name string, v bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.unsettled == nil {
		k.unsettled = map[string]bool{}
	}
	if v {
		k.unsettled[name] = true
	} else {
		delete(k.unsettled, name)
	}
}

func (k *Kind) isUnsettled(name string) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.unsettled[name]
}

func (k *Kind) forget(name string) { k.setUnsettled(name, false) }

func ctxCause(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return context.Canceled
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// lines splits command output into trimmed, non-empty lines.
func lines(b []byte) []string {
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// tail returns the end of a captured stream as a short, valid UTF-8 string.
func tail(b []byte) string {
	if len(b) > stderrTail {
		b = b[len(b)-stderrTail:]
	}
	return strings.TrimSpace(strings.ToValidUTF8(string(b), ""))
}
