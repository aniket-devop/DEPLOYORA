//go:build unix

package proc

import (
	"os"
	"os/exec"
	"syscall"
)

// setProcessGroup puts the child in its own process group, so a terminal Ctrl-C
// reaches only the parent.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// interrupt sends SIGINT to the child process only, as the spike did.
func interrupt(cmd *exec.Cmd) error {
	return cmd.Process.Signal(syscall.SIGINT)
}

// kill sends SIGKILL to the child's whole process group, so grandchildren go too.
func kill(cmd *exec.Cmd) error {
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

func exitedBySignal(state *os.ProcessState) bool {
	ws, ok := state.Sys().(syscall.WaitStatus)
	return ok && ws.Signaled()
}
