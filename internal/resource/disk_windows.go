//go:build windows

package resource

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"time"
)

func disk(ctx context.Context, path string) (uint64, uint64, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", `$ErrorActionPreference='Stop'; $d=(Get-Item -LiteralPath $env:MESHCOMPUTE_DISK_PATH).PSDrive; if ($null -eq $d.Free -or $null -eq $d.Used) {throw 'Disk capacity unavailable'}; @{Total=[uint64]($d.Free+$d.Used);Free=[uint64]$d.Free} | ConvertTo-Json -Compress`)
	cmd.Env = append(os.Environ(), "MESHCOMPUTE_DISK_PATH="+path)
	b, e := cmd.Output()
	if e != nil {
		return 0, 0, e
	}
	var d struct{ Total, Free uint64 }
	e = json.Unmarshal(b, &d)
	return d.Total, d.Free, e
}
