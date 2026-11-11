//go:build !windows

package task

import (
	"os/exec"
	"syscall"
)

// isolate starts the command in its own process group, so cancelling kills
// everything it started.
func isolate(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

func killTree(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
