//go:build !windows

package resource

import "bytes"

// decodePowerShell trims redirected command output on platforms that do not
// use PowerShell. The Windows build adds BOM and UTF-16 handling.
func decodePowerShell(b []byte) []byte { return bytes.TrimSpace(b) }
