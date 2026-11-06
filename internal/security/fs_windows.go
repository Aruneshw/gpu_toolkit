//go:build windows

package security

import "os"

// Windows does not expose Unix mode bits; privacy comes from the inherited
// user-profile ACLs, so there is no portable mode check here.
func privatePerm(os.FileMode) bool { return true }

// Directory handles are not flushed portably on Windows; NTFS journals the
// rename itself. File contents are still flushed before the rename.
func syncDir(string) error { return nil }
