//go:build windows

package terminal

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"unicode/utf16"
	"unsafe"
)

// Windows pseudo-console (ConPTY, Windows 10 1809+) using only the standard
// library.
var (
	kernel32                    = syscall.NewLazyDLL("kernel32.dll")
	procCreatePseudoConsole     = kernel32.NewProc("CreatePseudoConsole")
	procResizePseudoConsole     = kernel32.NewProc("ResizePseudoConsole")
	procClosePseudoConsole      = kernel32.NewProc("ClosePseudoConsole")
	procInitializeAttributeList = kernel32.NewProc("InitializeProcThreadAttributeList")
	procUpdateAttribute         = kernel32.NewProc("UpdateProcThreadAttribute")
	procDeleteAttributeList     = kernel32.NewProc("DeleteProcThreadAttributeList")
	procCreateProcess           = kernel32.NewProc("CreateProcessW")
)

const (
	attributePseudoConsole  = 0x00020016
	extendedStartupInfo     = 0x00080000
	createUnicodeEnv        = 0x00000400
	startfUseStdHandles     = 0x00000100
	infinite                = 0xFFFFFFFF
	errInsufficientBufferNo = 122
)

type startupInfoEx struct {
	syscall.StartupInfo
	attributeList uintptr
}

func coord(cols, rows int) uintptr { return uintptr(uint32(uint16(cols)) | uint32(uint16(rows))<<16) }

type conpty struct {
	in, out *os.File // we write keyboard input to in and read output from out
	handle  uintptr
	once    sync.Once
}

func (c *conpty) Read(b []byte) (int, error)  { return c.out.Read(b) }
func (c *conpty) Write(b []byte) (int, error) { return c.in.Write(b) }
func (c *conpty) Resize(cols, rows int) error {
	if hr, _, _ := procResizePseudoConsole.Call(c.handle, coord(cols, rows)); hr != 0 {
		return fmt.Errorf("resize pseudo-console: HRESULT 0x%x", uint32(hr))
	}
	return nil
}

// release closes the pseudo-console, which ends the output pipe.
func (c *conpty) release() {
	c.once.Do(func() { procClosePseudoConsole.Call(c.handle) })
}

func (c *conpty) Close() error {
	c.release()
	c.in.Close()
	return c.out.Close()
}

type winProcess struct {
	handle syscall.Handle
	pty    *conpty
	exited chan struct{}
	code   uint32
	mu     sync.Mutex // guards handle against use after it is closed
	closed bool
}

func (p *winProcess) Kill() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.closed {
		_ = syscall.TerminateProcess(p.handle, 1)
	}
}
func (p *winProcess) Wait() int {
	<-p.exited
	return int(p.code)
}

// environment builds a sorted-insensitive UTF-16 block; later entries win.
func environment(env []string) *uint16 {
	seen := map[string]int{}
	var list []string
	for _, kv := range env {
		key := strings.ToUpper(strings.SplitN(kv, "=", 2)[0])
		if i, ok := seen[key]; ok {
			list[i] = kv
			continue
		}
		seen[key] = len(list)
		list = append(list, kv)
	}
	var block []uint16
	for _, kv := range list {
		if strings.ContainsRune(kv, 0) {
			continue
		}
		block = append(block, utf16.Encode([]rune(kv))...)
		block = append(block, 0)
	}
	block = append(block, 0, 0)
	return &block[0]
}

func start(cmd *exec.Cmd, cols, rows int) (console, process, error) {
	if procCreatePseudoConsole.Find() != nil {
		return nil, nil, ErrUnsupported
	}
	var inRead, inWrite, outRead, outWrite syscall.Handle
	if err := syscall.CreatePipe(&inRead, &inWrite, nil, 0); err != nil {
		return nil, nil, fmt.Errorf("terminal input pipe: %w", err)
	}
	if err := syscall.CreatePipe(&outRead, &outWrite, nil, 0); err != nil {
		syscall.CloseHandle(inRead)
		syscall.CloseHandle(inWrite)
		return nil, nil, fmt.Errorf("terminal output pipe: %w", err)
	}
	var handle uintptr
	hr, _, _ := procCreatePseudoConsole.Call(coord(cols, rows), uintptr(inRead), uintptr(outWrite), 0, uintptr(unsafe.Pointer(&handle)))
	// The pseudo-console keeps its own copies of these two ends.
	syscall.CloseHandle(inRead)
	syscall.CloseHandle(outWrite)
	if hr != 0 {
		syscall.CloseHandle(inWrite)
		syscall.CloseHandle(outRead)
		return nil, nil, fmt.Errorf("create pseudo-console: HRESULT 0x%x", uint32(hr))
	}
	pty := &conpty{in: os.NewFile(uintptr(inWrite), "conin"), out: os.NewFile(uintptr(outRead), "conout"), handle: handle}

	var size uintptr
	procInitializeAttributeList.Call(0, 1, 0, uintptr(unsafe.Pointer(&size)))
	if size == 0 {
		pty.Close()
		return nil, nil, errors.New("process attribute list unavailable")
	}
	list := make([]byte, size)
	if ok, _, err := procInitializeAttributeList.Call(uintptr(unsafe.Pointer(&list[0])), 1, 0, uintptr(unsafe.Pointer(&size))); ok == 0 {
		pty.Close()
		return nil, nil, fmt.Errorf("process attribute list: %w", err)
	}
	defer procDeleteAttributeList.Call(uintptr(unsafe.Pointer(&list[0])))
	if ok, _, err := procUpdateAttribute.Call(uintptr(unsafe.Pointer(&list[0])), 0, attributePseudoConsole, handle, unsafe.Sizeof(handle), 0, 0); ok == 0 {
		pty.Close()
		return nil, nil, fmt.Errorf("attach pseudo-console: %w", err)
	}

	args := cmd.Args
	if len(args) == 0 {
		args = []string{cmd.Path}
	}
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = syscall.EscapeArg(a)
	}
	line, err := syscall.UTF16PtrFromString(strings.Join(quoted, " "))
	if err != nil {
		pty.Close()
		return nil, nil, err
	}
	var dir *uint16
	if cmd.Dir != "" {
		if dir, err = syscall.UTF16PtrFromString(cmd.Dir); err != nil {
			pty.Close()
			return nil, nil, err
		}
	}
	env := cmd.Env
	if env == nil {
		env = os.Environ()
	}
	si := startupInfoEx{attributeList: uintptr(unsafe.Pointer(&list[0]))}
	si.Cb = uint32(unsafe.Sizeof(si))
	// Without this a redirected parent would hand its own handles to the shell.
	si.Flags = startfUseStdHandles
	var pi syscall.ProcessInformation
	ok, _, callErr := procCreateProcess.Call(0, uintptr(unsafe.Pointer(line)), 0, 0, 0, extendedStartupInfo|createUnicodeEnv,
		uintptr(unsafe.Pointer(environment(env))), uintptr(unsafe.Pointer(dir)), uintptr(unsafe.Pointer(&si)), uintptr(unsafe.Pointer(&pi)))
	if ok == 0 {
		pty.Close()
		return nil, nil, fmt.Errorf("start shell: %w", callErr)
	}
	syscall.CloseHandle(pi.Thread)
	proc := &winProcess{handle: pi.Process, pty: pty, exited: make(chan struct{})}
	go func() {
		// The output pipe only ends once the pseudo-console is closed.
		syscall.WaitForSingleObject(pi.Process, infinite)
		_ = syscall.GetExitCodeProcess(pi.Process, &proc.code)
		proc.mu.Lock()
		proc.closed = true
		syscall.CloseHandle(pi.Process)
		proc.mu.Unlock()
		pty.release()
		close(proc.exited)
	}()
	return pty, proc, nil
}

func killGroup(cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
