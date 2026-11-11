//go:build windows

package mirror

import (
	"os"
	"syscall"
)

// OpenShared opens a mirrored file for reading with FILE_SHARE_DELETE, so the
// copier can still replace the file (rename over it) while a download, zip or
// verification pass has it open. os.Open does not set that flag on Windows.
func OpenShared(path string) (*os.File, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := syscall.CreateFile(p, syscall.GENERIC_READ,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}
