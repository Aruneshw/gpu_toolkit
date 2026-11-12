//go:build !windows

package resource

import (
	"os"
	"runtime"
)

// memory returns total and available physical memory where it is instant
// to read (Linux /proc/meminfo); nil otherwise.
func memory() (*uint64, *uint64) {
	if runtime.GOOS != "linux" {
		return nil, nil
	}
	b, e := os.ReadFile("/proc/meminfo")
	if e != nil {
		return nil, nil
	}
	return parseMeminfo(string(b))
}
