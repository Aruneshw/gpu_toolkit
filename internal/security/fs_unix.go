//go:build !windows

package security

import "os"

// privatePerm requires that group and other have no access.
func privatePerm(m os.FileMode) bool { return m.Perm()&0077 == 0 }

// syncDir persists directory entries after an atomic rename.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fail()
	}
	defer d.Close()
	if d.Sync() != nil {
		return fail()
	}
	return nil
}
