//go:build !unix

package proc

import (
	"os"
	"os/exec"
)

// Outside unix there are no process groups and no portable SIGINT, so in v0.1 a
// cancelled context ends the child at once. This is a documented limitation.

func setProcessGroup(cmd *exec.Cmd) {}

func interrupt(cmd *exec.Cmd) error { return cmd.Process.Kill() }

func kill(cmd *exec.Cmd) error { return cmd.Process.Kill() }

func exitedBySignal(state *os.ProcessState) bool { return false }
