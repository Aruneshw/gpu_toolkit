//go:build windows

package resource

import (
	"context"
	"syscall"
	"unsafe"
)

var (
	kernel32               = syscall.NewLazyDLL("kernel32.dll")
	procGetDiskFreeSpaceEx = kernel32.NewProc("GetDiskFreeSpaceExW")
	procGlobalMemoryStatus = kernel32.NewProc("GlobalMemoryStatusEx")
)

// disk asks Windows directly (GetDiskFreeSpaceExW): instant, unlike a
// PowerShell query, so a status request never waits for it.
func disk(ctx context.Context, path string) (uint64, uint64, error) {
	if e := ctx.Err(); e != nil {
		return 0, 0, e
	}
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, err
	}
	var freeToCaller, total, free uint64
	r, _, e := procGetDiskFreeSpaceEx.Call(uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&freeToCaller)), uintptr(unsafe.Pointer(&total)), uintptr(unsafe.Pointer(&free)))
	if r == 0 {
		return 0, 0, e
	}
	return total, freeToCaller, nil
}

// memoryStatusEx is MEMORYSTATUSEX.
type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

// memory returns total and available physical memory from Windows.
func memory() (*uint64, *uint64) {
	m := memoryStatusEx{Length: uint32(unsafe.Sizeof(memoryStatusEx{}))}
	if r, _, _ := procGlobalMemoryStatus.Call(uintptr(unsafe.Pointer(&m))); r == 0 || m.TotalPhys == 0 || m.AvailPhys > m.TotalPhys {
		return nil, nil
	}
	return &m.TotalPhys, &m.AvailPhys
}
