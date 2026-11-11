//go:build !linux && !windows

package terminal

import "os/exec"

func start(*exec.Cmd, int, int) (console, process, error) {
	return nil, nil, ErrUnsupported
}

func killGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
