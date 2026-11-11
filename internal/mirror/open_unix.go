//go:build !windows

package mirror

import "os"

// OpenShared opens a mirrored file for reading. On Unix any open file can be
// replaced by rename, so this is plain os.Open.
func OpenShared(path string) (*os.File, error) { return os.Open(path) }
