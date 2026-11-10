//go:build linux

package terminal

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"unsafe"
)

func ioctl(fd, req, arg uintptr) error {
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, req, arg); e != 0 {
		return e
	}
	return nil
}

// start launches cmd as the session leader of a new pseudo-terminal using
// only the standard library (/dev/ptmx, TIOCSPTLCK, TIOCGPTN).
func start(cmd *exec.Cmd, cols, rows int) (*os.File, error) {
	ptmx, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open pseudo-terminal: %w", err)
	}
	var unlock int32
	if err := ioctl(ptmx.Fd(), syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock))); err != nil {
		ptmx.Close()
		return nil, fmt.Errorf("unlock pseudo-terminal: %w", err)
	}
	var n uint32
	if err := ioctl(ptmx.Fd(), syscall.TIOCGPTN, uintptr(unsafe.Pointer(&n))); err != nil {
		ptmx.Close()
		return nil, fmt.Errorf("pseudo-terminal number: %w", err)
	}
	tty, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		ptmx.Close()
		return nil, fmt.Errorf("open terminal: %w", err)
	}
	defer tty.Close()
	if err := resize(ptmx, cols, rows); err != nil {
		ptmx.Close()
		return nil, err
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, tty
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		ptmx.Close()
		return nil, fmt.Errorf("start shell: %w", err)
	}
	return ptmx, nil
}

func resize(pty *os.File, cols, rows int) error {
	ws := struct{ Row, Col, X, Y uint16 }{uint16(rows), uint16(cols), 0, 0}
	return ioctl(pty.Fd(), syscall.TIOCSWINSZ, uintptr(unsafe.Pointer(&ws)))
}

func killGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	// The shell is a session leader, so its PID is also its process group.
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGHUP)
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
