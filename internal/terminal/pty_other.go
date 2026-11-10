//go:build !linux

package terminal

import (
	"os"
	"os/exec"
)

func start(*exec.Cmd, int, int) (*os.File, error) { return nil, ErrUnsupported }

func resize(*os.File, int, int) error { return ErrUnsupported }

func killGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
