package cluster

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aniket-devop/deployora/internal/proc"
)

type call struct {
	name string
	args []string
	env  []string
}

func (c call) String() string { return c.name + " " + strings.Join(c.args, " ") }

type fakeRunner struct {
	mu      sync.Mutex
	calls   []call
	handler func(c call) (proc.Result, error)
}

func (f *fakeRunner) run(_ context.Context, spec proc.Spec) (proc.Result, error) {
	c := call{name: spec.Name, args: spec.Args, env: spec.Env}
	f.mu.Lock()
	f.calls = append(f.calls, c)
	f.mu.Unlock()
	return f.handler(c)
}

// count returns how many recorded calls start with prefix.
func (f *fakeRunner) count(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c.String(), prefix) {
			n++
		}
	}
	return n
}

func out(s string) (proc.Result, error) { return proc.Result{Stdout: []byte(s)}, nil }

func newKind(h func(c call) (proc.Result, error)) (*Kind, *fakeRunner) {
	f := &fakeRunner{handler: h}
	return &Kind{Runner: f.run, WatchWindow: 300 * time.Millisecond, WatchEvery: 5 * time.Millisecond}, f
}

func testSpec(t *testing.T) ClusterSpec {
	t.Helper()
	return ClusterSpec{
		Name:           "deployora-abc123",
		NodeImage:      "kindest/node:v1.36.1",
		KubeconfigPath: filepath.Join(t.TempDir(), "kubeconfig"),
	}
}

func TestKindCreateArgs(t *testing.T) {
	k, f := newKind(func(call) (proc.Result, error) { return out("") })
	spec := testSpec(t)
	got, err := k.Create(context.Background(), spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got.Name != spec.Name || got.KubeconfigPath != spec.KubeconfigPath {
		t.Errorf("got cluster %+v", got)
	}
	if len(f.calls) != 1 {
		t.Fatalf("got %d commands, want 1", len(f.calls))
	}
	want := "kind create cluster --name deployora-abc123 --image kindest/node:v1.36.1 --kubeconfig " + spec.KubeconfigPath
	if f.calls[0].String() != want {
		t.Errorf("command\n got  %s\n want %s", f.calls[0], want)
	}
	if len(f.calls[0].env) != 0 {
		t.Errorf("Create passed extra environment: %v", f.calls[0].env)
	}
}

func TestKindCreateRejectsInvalidSpecBeforeRunning(t *testing.T) {
	k, f := newKind(func(call) (proc.Result, error) { return out("") })
	spec := testSpec(t)
	spec.Name = "kind"
	if _, err := k.Create(context.Background(), spec); !errors.Is(err, ErrInvalid) {
		t.Errorf("got %v, want ErrInvalid", err)
	}
	if len(f.calls) != 0 {
		t.Errorf("ran %d commands for an invalid spec", len(f.calls))
	}
}

func TestKindCreateFailure(t *testing.T) {
	k, _ := newKind(func(call) (proc.Result, error) {
		return proc.Result{ExitCode: 1, Stderr: []byte("kubeadm timed out")}, nil
	})
	_, err := k.Create(context.Background(), testSpec(t))
	var ce *CreateError
	if !errors.As(err, &ce) || ce.ExitCode != 1 || !strings.Contains(ce.Stderr, "kubeadm timed out") {
		t.Fatalf("got %v, want a CreateError with exit code 1 and the stderr tail", err)
	}
}

func TestKindCreateCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	k, _ := newKind(func(call) (proc.Result, error) {
		return proc.Result{ExitCode: -1, Canceled: true}, nil
	})
	if _, err := k.Create(ctx, testSpec(t)); !errors.Is(err, context.Canceled) {
		t.Errorf("got %v, want context.Canceled", err)
	}
}

func TestKindDeleteRefusesForeignName(t *testing.T) {
	k, f := newKind(func(call) (proc.Result, error) { return out("") })
	if err := k.Delete(context.Background(), Cluster{Name: "kind"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("got %v, want ErrInvalid", err)
	}
	if len(f.calls) != 0 {
		t.Errorf("ran %d commands for a foreign cluster name", len(f.calls))
	}
}

func TestKindDeleteClean(t *testing.T) {
	k, f := newKind(func(call) (proc.Result, error) { return out("") })
	c := Cluster{Name: "deployora-abc123", KubeconfigPath: "/tmp/x/kubeconfig"}
	if err := k.Delete(context.Background(), c); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if f.count("kind delete cluster --name deployora-abc123 --kubeconfig /tmp/x/kubeconfig") != 1 {
		t.Errorf("unexpected delete calls: %v", f.calls)
	}
}

func TestKindDeleteChecksOnceAfterNormalCreate(t *testing.T) {
	k, f := newKind(func(call) (proc.Result, error) { return out("") })
	ctx := context.Background()
	spec := testSpec(t)
	c, err := k.Create(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := k.Delete(ctx, c); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if n := f.count("docker ps"); n != 1 {
		t.Errorf("checked %d times after a normal Create, want 1", n)
	}
}

func TestKindDeleteWatchesForLateContainerAfterAbnormalCreate(t *testing.T) {
	var mu sync.Mutex
	deletes, psCalls := 0, 0
	k, _ := newKind(func(c call) (proc.Result, error) {
		mu.Lock()
		defer mu.Unlock()
		switch s := c.String(); {
		case strings.HasPrefix(s, "kind create"):
			return proc.Result{ExitCode: 1}, nil
		case strings.HasPrefix(s, "kind delete"):
			deletes++
			return out("")
		case strings.HasPrefix(s, "docker ps"):
			psCalls++
			if psCalls >= 2 && deletes < 2 {
				return out("deployora-abc123-control-plane\n")
			}
			return out("")
		default:
			return out("")
		}
	})
	ctx := context.Background()
	spec := testSpec(t)
	if _, err := k.Create(ctx, spec); err == nil {
		t.Fatal("Create should have failed")
	}
	if err := k.Delete(ctx, Cluster{Name: spec.Name, KubeconfigPath: spec.KubeconfigPath}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if deletes != 2 {
		t.Errorf("kind delete ran %d times, want 2 (one retry after the late container)", deletes)
	}
}

func TestKindDeleteLeak(t *testing.T) {
	k, f := newKind(func(c call) (proc.Result, error) {
		if strings.HasPrefix(c.String(), "docker ps") {
			return out("deployora-abc123-control-plane\n")
		}
		return out("")
	})
	err := k.Delete(context.Background(), Cluster{Name: "deployora-abc123", KubeconfigPath: "/tmp/x/kubeconfig"})
	var le *LeakError
	if !errors.As(err, &le) || len(le.Left) == 0 {
		t.Fatalf("got %v, want a LeakError", err)
	}
	if n := f.count("kind delete"); n != 2 {
		t.Errorf("kind delete ran %d times, want 2", n)
	}
}

func TestKindDeleteVerifyErrorIsNotClean(t *testing.T) {
	k, _ := newKind(func(c call) (proc.Result, error) {
		if strings.HasPrefix(c.String(), "docker ps") {
			return proc.Result{ExitCode: 1, Stderr: []byte("cannot connect to the docker daemon")}, nil
		}
		return out("")
	})
	err := k.Delete(context.Background(), Cluster{Name: "deployora-abc123", KubeconfigPath: "/tmp/x/kubeconfig"})
	if err == nil || !strings.Contains(err.Error(), "cannot connect") {
		t.Fatalf("got %v, want a verification error carrying the docker message", err)
	}
	var le *LeakError
	if errors.As(err, &le) {
		t.Error("a failed check must not be reported as a leak")
	}
}

func TestKindDeleteNonZeroButNothingLeft(t *testing.T) {
	k, _ := newKind(func(c call) (proc.Result, error) {
		if strings.HasPrefix(c.String(), "kind delete") {
			return proc.Result{ExitCode: 1, Stderr: []byte("no such cluster")}, nil
		}
		return out("")
	})
	if err := k.Delete(context.Background(), Cluster{Name: "deployora-abc123", KubeconfigPath: "/tmp/x/kubeconfig"}); err != nil {
		t.Errorf("got %v, want nil when nothing is left", err)
	}
}

func TestKindList(t *testing.T) {
	k, _ := newKind(func(call) (proc.Result, error) {
		return out("deployora-a1\nkind\nother-cluster\ndeployora-b2\n")
	})
	got, err := k.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, ci := range got {
		names = append(names, ci.Name)
	}
	if strings.Join(names, ",") != "deployora-a1,deployora-b2" {
		t.Errorf("got %v, want only the Deployora clusters", names)
	}
}

func TestKindListIgnoresStderr(t *testing.T) {
	k, _ := newKind(func(call) (proc.Result, error) {
		return proc.Result{Stderr: []byte("No kind clusters found.\n")}, nil
	})
	got, err := k.List(context.Background())
	if err != nil || len(got) != 0 {
		t.Errorf("got %v, %v, want an empty list", got, err)
	}
}
