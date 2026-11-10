package cli

import (
	"os"
	"os/exec"
	"runtime"
)

// openBrowser shows url in the desktop's default browser. It is best effort:
// the URL is always printed as well, so a headless machine still works.
func openBrowser(url string) bool {
	if os.Getenv("MESHCOMPUTE_NO_BROWSER") != "" {
		return false
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
			return false
		}
		path, err := exec.LookPath("xdg-open")
		if err != nil {
			return false
		}
		cmd = exec.Command(path, url)
	}
	cmd.Stdout, cmd.Stderr = nil, nil
	if err := cmd.Start(); err != nil {
		return false
	}
	go func() { _ = cmd.Wait() }()
	return true
}
