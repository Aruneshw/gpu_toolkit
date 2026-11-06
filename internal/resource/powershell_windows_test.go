//go:build windows

package resource

import (
	"encoding/json"
	"testing"
	"unicode/utf16"
)

func TestDecodePowerShell(t *testing.T) {
	want := `{"ok":true}`
	cases := map[string][]byte{
		"utf8":            []byte(want),
		"utf8 bom":        append([]byte{0xEF, 0xBB, 0xBF}, []byte(want)...),
		"utf16 le bom":    utf16Bytes(want, false, true),
		"utf16 be bom":    utf16Bytes(want, true, true),
		"utf16 le no bom": utf16Bytes(want, false, false),
		"whitespace":      []byte("\r\n " + want + " \r\n"),
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			var v struct {
				OK bool `json:"ok"`
			}
			if err := json.Unmarshal(decodePowerShell(input), &v); err != nil || !v.OK {
				t.Fatalf("got %q err=%v", decodePowerShell(input), err)
			}
		})
	}
}

func utf16Bytes(s string, bigEndian, bom bool) []byte {
	units := utf16.Encode([]rune(s))
	out := make([]byte, 0, len(units)*2+2)
	if bom {
		if bigEndian {
			out = append(out, 0xFE, 0xFF)
		} else {
			out = append(out, 0xFF, 0xFE)
		}
	}
	for _, u := range units {
		if bigEndian {
			out = append(out, byte(u>>8), byte(u))
		} else {
			out = append(out, byte(u), byte(u>>8))
		}
	}
	return out
}
