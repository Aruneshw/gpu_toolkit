//go:build linux

package resource

import (
	"context"
	"syscall"
)

func disk(ctx context.Context, path string) (uint64, uint64, error) {
	if e := ctx.Err(); e != nil {
		return 0, 0, e
	}
	var s syscall.Statfs_t
	if e := syscall.Statfs(path, &s); e != nil {
		return 0, 0, e
	}
	return s.Blocks * uint64(s.Bsize), s.Bavail * uint64(s.Bsize), nil
}
