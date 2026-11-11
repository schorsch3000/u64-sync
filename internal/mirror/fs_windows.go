//go:build windows

package mirror

import "syscall"

// localName escapes characters Windows cannot store in file names.
func localName(name string) string { return escapeWindows(name) }

// fsID identifies the volume a path lives on by its serial number.
func fsID(path string) (uint64, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	// FILE_FLAG_BACKUP_SEMANTICS is required to open a directory.
	h, err := syscall.CreateFile(p, 0, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil, syscall.OPEN_EXISTING, syscall.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return 0, err
	}
	defer syscall.CloseHandle(h)
	var info syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(h, &info); err != nil {
		return 0, err
	}
	return uint64(info.VolumeSerialNumber), nil
}
