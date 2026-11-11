//go:build !windows

package mirror

import "os"

// replaceFile atomically puts src in place of dst. rename(2) replaces a
// destination that is currently open without trouble.
func replaceFile(src, dst, _ string) error { return os.Rename(src, dst) }
