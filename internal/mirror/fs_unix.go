//go:build !windows

package mirror

import "syscall"

// localName maps a device file name to a local one; identity on Unix.
func localName(name string) string { return name }

// fsID identifies the filesystem a path lives on.
func fsID(path string) (uint64, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return 0, err
	}
	return uint64(st.Dev), nil
}
