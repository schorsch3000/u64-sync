// Package mirror manages the local copy: safe path mapping and atomic installs.
package mirror

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

type Mirror struct {
	Current string // mirror root
	Tmp     string // temp downloads; must be on the same fs as Current

	// Mu serializes tree changes (Install, Remove) against snapshot creation,
	// so a snapshot never sees a half-updated mirror.
	Mu sync.Mutex
}

// CheckSameFS fails unless all dirs exist on one filesystem (needed for atomic rename and hardlinks).
func CheckSameFS(dirs ...string) error {
	var dev uint64
	for i, d := range dirs {
		var st syscall.Stat_t
		if err := syscall.Stat(d, &st); err != nil {
			return err
		}
		if i == 0 {
			dev = uint64(st.Dev)
		} else if uint64(st.Dev) != dev {
			return fmt.Errorf("%s and %s are on different filesystems", dirs[0], d)
		}
	}
	return nil
}

// Path maps a remote root and relative path to a local path inside Current.
func (m *Mirror) Path(root, rel string) (string, error) {
	full := strings.TrimPrefix(root, "/") + "/" + rel
	for _, part := range strings.Split(full, "/") {
		if part == "" || part == "." || part == ".." || strings.ContainsRune(part, 0) {
			return "", fmt.Errorf("unsafe path %q", full)
		}
	}
	return filepath.Join(m.Current, filepath.FromSlash(full)), nil
}

// NewTemp creates a temp file in the tmp dir.
func (m *Mirror) NewTemp() (*os.File, error) {
	return os.CreateTemp(m.Tmp, "dl-*")
}

// CleanTemp removes leftovers of interrupted downloads.
func (m *Mirror) CleanTemp() {
	ents, _ := os.ReadDir(m.Tmp)
	for _, e := range ents {
		os.Remove(filepath.Join(m.Tmp, e.Name()))
	}
}

// Install finalizes a downloaded temp file: fsync, set mtime, atomic rename
// over target, fsync directory. The target is replaced by a new inode, never
// modified in place, so hardlinked snapshots keep the old content.
func (m *Mirror) Install(tmp *os.File, target string, mtime time.Time) error {
	name := tmp.Name()
	fail := func(err error) error {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		return fail(err)
	}
	if err := os.Chmod(name, 0o644); err != nil {
		return fail(err)
	}
	if err := os.Chtimes(name, mtime, mtime); err != nil {
		return fail(err)
	}
	dir := filepath.Dir(target)
	m.Mu.Lock()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		m.Mu.Unlock()
		return fail(err)
	}
	err := os.Rename(name, target)
	m.Mu.Unlock()
	if err != nil {
		return fail(err)
	}
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
}

// Remove deletes a mirrored file and prunes empty parent dirs up to Current.
func (m *Mirror) Remove(root, rel string) error {
	p, err := m.Path(root, rel)
	if err != nil {
		return err
	}
	m.Mu.Lock()
	defer m.Mu.Unlock()
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	for d := filepath.Dir(p); d != m.Current && strings.HasPrefix(d, m.Current+string(filepath.Separator)); d = filepath.Dir(d) {
		if os.Remove(d) != nil { // fails if not empty
			break
		}
	}
	return nil
}
