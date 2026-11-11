//go:build windows

package mirror

import (
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// replaceFile puts src in place of dst. The atomic rename fails with "access
// denied" when dst is open in another handle (a download, zip or verify pass),
// even if that handle allows sharing. Then the old file is moved aside into
// trashDir (allowed while it is open with FILE_SHARE_DELETE), the new file is
// renamed in, and the old one is deleted. dst is absent only between the two
// renames; if the second fails the old file is put back.
func replaceFile(src, dst, trashDir string) error {
	err := os.Rename(src, dst)
	if err == nil {
		return nil
	}
	if _, serr := os.Lstat(dst); serr != nil {
		return err // dst does not exist: the failure has another cause
	}
	trash := filepath.Join(trashDir, "old-"+strconv.FormatInt(time.Now().UnixNano(), 36))
	if merr := os.Rename(dst, trash); merr != nil {
		return err
	}
	if rerr := os.Rename(src, dst); rerr != nil {
		os.Rename(trash, dst)
		return rerr
	}
	os.Remove(trash) // may stay until the last reader closes it; CleanTemp collects leftovers
	return nil
}
