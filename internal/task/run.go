package task

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"runtime"
	"time"
)

// command builds the process for spec. The task ID is in the environment so
// scripts can tag their own files.
func command(ctx context.Context, spec Spec) (*exec.Cmd, error) {
	shell := spec.Shell
	if shell == "" || shell == "auto" {
		shell = "bash"
		if runtime.GOOS == "windows" {
			shell = "powershell"
		} else if _, err := exec.LookPath("bash"); err != nil {
			shell = "sh"
		}
	}
	var name string
	var args []string
	switch shell {
	case "bash", "sh":
		if runtime.GOOS == "windows" {
			return nil, errors.New(shell + " is not available on Windows; use auto, powershell or cmd")
		}
		name, args = shell, []string{"-c", spec.Command}
	case "powershell", "pwsh":
		name = shell
		if runtime.GOOS != "windows" && shell == "powershell" {
			name = "pwsh"
		}
		// UTF-8 output, so the host shows non-ASCII text correctly.
		args = []string{"-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command",
			"[Console]::OutputEncoding=[Text.Encoding]::UTF8; $ProgressPreference='SilentlyContinue'; " + spec.Command}
	case "cmd":
		if runtime.GOOS != "windows" {
			return nil, errors.New("cmd is only available on Windows")
		}
		name, args = "cmd", []string{"/D", "/S", "/C", spec.Command}
	default:
		return nil, errors.New("unknown shell")
	}
	path, err := exec.LookPath(name)
	if err != nil {
		return nil, errors.New(name + " was not found on this computer")
	}
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Dir = spec.Dir
	if cmd.Dir == "" {
		cmd.Dir, _ = os.UserHomeDir()
	}
	if st, err := os.Stat(cmd.Dir); err != nil || !st.IsDir() {
		return nil, errors.New("working directory " + cmd.Dir + " does not exist on this computer")
	}
	cmd.Env = append(os.Environ(), "MESHCOMPUTE_TASK_ID="+spec.ID, "PYTHONUNBUFFERED=1")
	isolate(cmd)
	cmd.Cancel = func() error { killTree(cmd); return nil }
	cmd.WaitDelay = 3 * time.Second
	return cmd, nil
}

// Run executes spec, writing combined output to out, until it exits or ctx
// ends (which kills the whole process tree). It returns the exit code.
func Run(ctx context.Context, spec Spec, out io.Writer) (int, error) {
	cmd, err := command(ctx, spec)
	if err != nil {
		return -1, err
	}
	cmd.Stdin = nil
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		return -1, err
	}
	err = cmd.Wait()
	if ctx.Err() != nil {
		return -1, ctx.Err()
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), nil
	}
	if err != nil && !errors.Is(err, exec.ErrWaitDelay) {
		return -1, err
	}
	return cmd.ProcessState.ExitCode(), nil
}
