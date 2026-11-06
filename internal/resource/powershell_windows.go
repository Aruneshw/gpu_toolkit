//go:build windows

package resource

import (
	"bytes"
	"unicode/utf16"
)

// decodePowerShell normalizes redirected PowerShell output. Windows PowerShell
// may emit a UTF-8 BOM or UTF-16 depending on host and policy; Go receives raw
// bytes, so accept all three encodings before JSON decoding.
func decodePowerShell(b []byte) []byte {
	switch {
	case bytes.HasPrefix(b, []byte{0xEF, 0xBB, 0xBF}):
		b = b[3:]
	case bytes.HasPrefix(b, []byte{0xFF, 0xFE}):
		b = decodeUTF16(b[2:], false)
	case bytes.HasPrefix(b, []byte{0xFE, 0xFF}):
		b = decodeUTF16(b[2:], true)
	case len(b) >= 2 && b[0] != 0 && b[1] == 0:
		// Unmarked UTF-16LE from some redirected PowerShell hosts.
		b = decodeUTF16(b, false)
	}
	return bytes.TrimSpace(b)
}

func decodeUTF16(b []byte, bigEndian bool) []byte {
	n := len(b) / 2
	units := make([]uint16, n)
	for i := 0; i < n; i++ {
		if bigEndian {
			units[i] = uint16(b[2*i])<<8 | uint16(b[2*i+1])
		} else {
			units[i] = uint16(b[2*i]) | uint16(b[2*i+1])<<8
		}
	}
	return []byte(string(utf16.Decode(units)))
}
