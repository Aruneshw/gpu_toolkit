//go:build !linux && !windows

package resource

import (
	"context"
	"fmt"
)

func disk(context.Context, string) (uint64, uint64, error) {
	return 0, 0, fmt.Errorf("disk discovery unsupported on this OS")
}
