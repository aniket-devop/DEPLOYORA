//go:build unix

package proc

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The tests run the test binary itself as the child process, so they need
// neither kind nor docker.
func TestMain(m *testing.M) {
	if mode := os.Getenv("PROC_TEST_HELPER"); mode != "" {
		runHelper(mode)
	}
	os.Exit(m.Run())
}

func markReady() {
	if p := os.Getenv("PROC_TEST_READY"); p != "" {
		_ = os.WriteFile(p, []byte("ready"), 0o600)
	}
}

func runHelper(mode string) {
	switch mode {
	case "exit":
		code, _ := strconv.Atoi(os.Getenv("PROC_TEST_CODE"))
		fmt.Fprintln(os.Stdout, "out")
		fmt.Fprintln(os.Stderr, "err")
		os.Exit(code)
	case "graceful":
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGINT)
		markReady()
		<-ch
		os.Exit(0)
	case "stubborn":
		signal.Ignore(syscall.SIGINT)
		markReady()
		time.Sleep(time.Minute)
		os.Exit(0)
	case "stdin":
		data, _ := io.ReadAll(os.Stdin)
		fmt.Fprintf(os.Stdout, "got:%s", data)
		os.Exit(0)
	case "flood":
		fmt.Fprint(os.Stdout, strings.Repeat("x", 1000))
		os.Exit(0)
	case "env":
		fmt.Fprintf(os.Stdout, "KUBECONFIG=[%s]\n", os.Getenv("KUBECONFIG"))
		os.Exit(0)
	}
	os.Exit(64)
}

func helperSpec(t *testing.T, mode string, extra ...string) Spec {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return Spec{Name: exe, Env: append([]string{"PROC_TEST_HELPER=" + mode}, extra...)}
}

// cancelWhenReady cancels the context once the helper has installed its signal
// handler, so the tests do not depend on timing guesses.
func cancelWhenReady(ready string, cancel context.CancelFunc) {
	go func() {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(ready); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
	}()
}

func TestExitCodesAndOutput(t *testing.T) {
	for _, code := range []int{0, 3} {
		spec := helperSpec(t, "exit", "PROC_TEST_CODE="+strconv.Itoa(code))
		res, err := Run(context.Background(), spec)
		if err != nil {
			t.Fatalf("code %d: unexpected error: %v", code, err)
		}
		if res.ExitCode != code || res.Signaled || res.Canceled || res.Killed {
			t.Errorf("code %d: got %+v", code, res)
		}
		if string(res.Stdout) != "out\n" || string(res.Stderr) != "err\n" {
			t.Errorf("code %d: stdout %q stderr %q", code, res.Stdout, res.Stderr)
		}
	}
}

func TestStartFailure(t *testing.T) {
	_, err := Run(context.Background(), Spec{Name: "deployora-no-such-binary-xyz"})
	if err == nil {
		t.Fatal("expected an error for a command that cannot start")
	}
}

func TestAlreadyCanceledContextDoesNotStart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err := Run(ctx, helperSpec(t, "exit"))
	if err == nil || !res.Canceled {
		t.Fatalf("got res=%+v err=%v, want Canceled and an error", res, err)
	}
}

func TestCancelLetsChildExitGracefully(t *testing.T) {
	ready := filepath.Join(t.TempDir(), "ready")
	spec := helperSpec(t, "graceful", "PROC_TEST_READY="+ready)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancelWhenReady(ready, cancel)

	res, err := Run(ctx, spec)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Canceled || res.Killed || res.Signaled || res.ExitCode != 0 {
		t.Errorf("got %+v, want Canceled only, exit code 0", res)
	}
}

func TestChildIgnoringSigintIsKilledAfterGrace(t *testing.T) {
	ready := filepath.Join(t.TempDir(), "ready")
	spec := helperSpec(t, "stubborn", "PROC_TEST_READY="+ready)
	spec.Grace = 200 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancelWhenReady(ready, cancel)

	start := time.Now()
	res, err := Run(ctx, spec)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Canceled || !res.Killed || !res.Signaled || res.ExitCode != -1 {
		t.Errorf("got %+v, want Canceled, Killed, Signaled, exit code -1", res)
	}
	if time.Since(start) > 10*time.Second {
		t.Errorf("took %s, the kill after grace did not work", time.Since(start))
	}
}

func TestOutputIsCapped(t *testing.T) {
	spec := helperSpec(t, "flood")
	spec.MaxOutput = 100
	res, err := Run(context.Background(), spec)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Truncated || len(res.Stdout) != 100 {
		t.Errorf("Truncated=%v len(Stdout)=%d, want true and 100", res.Truncated, len(res.Stdout))
	}
}

func TestKubeconfigIsScrubbed(t *testing.T) {
	t.Setenv("KUBECONFIG", "/should/not/leak")
	res, err := Run(context.Background(), helperSpec(t, "env"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(res.Stdout) != "KUBECONFIG=[]\n" {
		t.Errorf("child saw %q", res.Stdout)
	}
}
