// Package proc runs external tools such as kind, helm and kubeconform on behalf
// of the engine. It reports facts about how a command ended and makes no
// decision about outcomes: classification belongs to the engine.
//
// Behaviour comes from the Kind interrupt spike (docs/spikes/kind-interrupt.md):
// the child runs in its own process group so a terminal Ctrl-C does not reach it
// directly, a cancelled context sends it SIGINT, and a child that does not exit
// within the grace period is killed together with its process group.
package proc

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultGrace is how long a child may take to exit after SIGINT before it is killed.
	DefaultGrace = 15 * time.Second
	// DefaultMaxOutput is the default cap, in bytes, for each of stdout and stderr.
	DefaultMaxOutput = 1 << 20
	// waitDelay bounds how long Run waits for the output pipes after the process
	// has exited, so a grandchild that keeps a pipe open cannot hang the run.
	waitDelay = 2 * time.Second
)

// Spec describes one external command.
type Spec struct {
	Name string
	Args []string
	Dir  string
	// Env entries (KEY=VALUE) are added after the inherited environment has been
	// scrubbed of KUBECONFIG, so a caller can set variables deliberately.
	Env []string
	// Grace is the time allowed to exit after SIGINT. Zero means DefaultGrace.
	Grace time.Duration
	// MaxOutput caps each of stdout and stderr, in bytes. Zero means DefaultMaxOutput.
	MaxOutput int
}

// Result states facts about a finished command. It makes no outcome decision.
type Result struct {
	// ExitCode is the process exit code, or -1 if it was ended by a signal.
	ExitCode int
	// Signaled reports that the process was ended by a signal.
	Signaled bool
	// Canceled reports that the context was done before the process exited by itself.
	Canceled bool
	// Killed reports that the process had to be killed after the grace period.
	Killed bool
	// Stdout and Stderr hold the captured output, each cut at the cap.
	Stdout []byte
	Stderr []byte
	// Truncated reports that stdout or stderr exceeded the cap and was cut.
	Truncated bool
}

// Run starts the command and waits for it. A non-zero exit code is not an error:
// it is reported in Result. The error is non-nil only when the command could not
// be started or waited for, or when the context was already done before starting.
//
// Run does not use exec.CommandContext on purpose: that would kill the child
// the moment the context is cancelled, while the child needs a chance to clean up.
func Run(ctx context.Context, spec Spec) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{ExitCode: -1, Canceled: true}, err
	}
	grace := spec.Grace
	if grace <= 0 {
		grace = DefaultGrace
	}
	limit := spec.MaxOutput
	if limit <= 0 {
		limit = DefaultMaxOutput
	}

	cmd := exec.Command(spec.Name, spec.Args...)
	cmd.Dir = spec.Dir
	cmd.Env = append(scrubbedEnv(os.Environ()), spec.Env...)
	stdout := &capped{max: limit}
	stderr := &capped{max: limit}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.WaitDelay = waitDelay
	setProcessGroup(cmd)

	if err := cmd.Start(); err != nil {
		return Result{ExitCode: -1}, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	var res Result
	var waitErr error
	select {
	case waitErr = <-done:
	case <-ctx.Done():
		res.Canceled = true
		_ = interrupt(cmd)
		timer := time.NewTimer(grace)
		select {
		case waitErr = <-done:
			timer.Stop()
		case <-timer.C:
			res.Killed = true
			_ = kill(cmd)
			waitErr = <-done
		}
	}

	res.Stdout = stdout.bytes()
	res.Stderr = stderr.bytes()
	res.Truncated = stdout.wasTruncated() || stderr.wasTruncated()
	res.ExitCode = -1
	if state := cmd.ProcessState; state != nil {
		res.ExitCode = state.ExitCode()
		res.Signaled = exitedBySignal(state)
	}

	var exitErr *exec.ExitError
	if waitErr != nil && !errors.As(waitErr, &exitErr) && !errors.Is(waitErr, exec.ErrWaitDelay) {
		return res, waitErr
	}
	return res, nil
}

// scrubbedEnv returns env without KUBECONFIG, so a child can never pick up the
// user's default cluster configuration.
func scrubbedEnv(env []string) []string {
	const prefix = "KUBECONFIG="
	out := make([]string, 0, len(env))
	for _, e := range env {
		if len(e) >= len(prefix) && strings.EqualFold(e[:len(prefix)], prefix) {
			continue
		}
		out = append(out, e)
	}
	return out
}

// capped is an io.Writer that keeps at most max bytes. It always reports a full
// write, so the child is never blocked or sent SIGPIPE because of the cap.
type capped struct {
	mu        sync.Mutex
	buf       []byte
	max       int
	truncated bool
}

func (c *capped) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	room := c.max - len(c.buf)
	if room > 0 {
		n := len(p)
		if n > room {
			n = room
		}
		c.buf = append(c.buf, p[:n]...)
	}
	if len(p) > room {
		c.truncated = true
	}
	return len(p), nil
}

func (c *capped) bytes() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf
}

func (c *capped) wasTruncated() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.truncated
}
