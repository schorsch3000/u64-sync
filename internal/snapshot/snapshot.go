// Package snapshot creates hardlink snapshots of the mirror and prunes them
// by a last/daily/weekly/monthly retention policy.
//
// A snapshot costs one directory entry per file, no data. It stays valid
// because the copier always replaces files with a new inode (rename) and never
// modifies a file in place.
package snapshot

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/schorsch3000/u64-sync/internal/config"
	"github.com/schorsch3000/u64-sync/internal/mirror"
	"github.com/schorsch3000/u64-sync/internal/store"
)

const (
	nameFormat = "20060102T150405Z"
	partial    = ".partial"
)

type Manager struct {
	cfg config.Config
	st  *store.Store
	mir *mirror.Mirror

	mu     sync.Mutex
	status Status
}

type Status struct {
	Enabled   bool     `json:"enabled"`
	Names     []string `json:"snapshots"` // newest first
	LastError string   `json:"last_error,omitempty"`
}

func New(cfg config.Config, st *store.Store, mir *mirror.Mirror) *Manager {
	return &Manager{cfg: cfg, st: st, mir: mir}
}

func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.status
}

// Run snapshots after each complete sync (at most once per interval) and prunes.
func (m *Manager) Run(ctx context.Context) {
	m.update(func(s *Status) { s.Enabled = m.cfg.Snapshots.Enabled })
	if !m.cfg.Snapshots.Enabled {
		return
	}
	m.cleanPartial()
	for {
		m.tick()
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Minute):
		}
	}
}

func (m *Manager) update(f func(*Status)) {
	m.mu.Lock()
	f(&m.status)
	m.mu.Unlock()
}

func (m *Manager) tick() {
	times, err := m.list()
	if err != nil {
		m.update(func(s *Status) { s.LastError = err.Error() })
		return
	}
	gen := m.st.GetKV("complete_gen")
	due := gen != "" && gen != m.st.GetKV("snapshot_gen") &&
		(len(times) == 0 || time.Since(times[0]) >= m.cfg.Snapshots.Interval)
	if due {
		name, err := m.Create(time.Now())
		if err != nil {
			slog.Error("snapshot", "err", err)
			m.update(func(s *Status) { s.LastError = err.Error() })
		} else {
			m.st.SetKV("snapshot_gen", gen)
			slog.Info("snapshot created", "name", name)
			m.update(func(s *Status) { s.LastError = "" })
		}
	}
	m.prune()
}

// Create builds a hardlink tree of the mirror under a temporary name, then renames it.
func (m *Manager) Create(now time.Time) (string, error) {
	name := now.UTC().Format(nameFormat)
	final := filepath.Join(m.cfg.Storage.SnapshotDir, name)
	work := final + partial
	os.RemoveAll(work)

	m.mir.Mu.Lock() // no file replaced while linking: consistent tree
	defer m.mir.Mu.Unlock()
	cur := m.mir.Current
	err := filepath.WalkDir(cur, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(cur, p)
		dst := filepath.Join(work, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(dst, 0o755)
		case d.Type().IsRegular():
			return os.Link(p, dst)
		}
		return nil
	})
	if err == nil {
		err = os.Rename(work, final)
	}
	if err != nil {
		os.RemoveAll(work)
		return "", fmt.Errorf("snapshot %s: %w", name, err)
	}
	return name, nil
}

func (m *Manager) cleanPartial() {
	ents, _ := os.ReadDir(m.cfg.Storage.SnapshotDir)
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), partial) {
			os.RemoveAll(filepath.Join(m.cfg.Storage.SnapshotDir, e.Name()))
		}
	}
}

// list returns snapshot times, newest first, and refreshes the status.
func (m *Manager) list() ([]time.Time, error) {
	ents, err := os.ReadDir(m.cfg.Storage.SnapshotDir)
	if err != nil {
		return nil, err
	}
	var ts []time.Time
	for _, e := range ents {
		if t, err := time.Parse(nameFormat, e.Name()); err == nil && e.IsDir() {
			ts = append(ts, t)
		}
	}
	sort.Slice(ts, func(i, j int) bool { return ts[i].After(ts[j]) })
	names := make([]string, len(ts))
	for i, t := range ts {
		names[i] = t.Format(nameFormat)
	}
	m.update(func(s *Status) { s.Names = names })
	return ts, nil
}

func (m *Manager) prune() {
	ts, err := m.list()
	if err != nil {
		return
	}
	keep := Retain(ts, m.cfg.Snapshots.Keep.Last, m.cfg.Snapshots.Keep.Daily, m.cfg.Snapshots.Keep.Weekly, m.cfg.Snapshots.Keep.Monthly)
	for _, t := range ts {
		if keep[t] {
			continue
		}
		name := t.Format(nameFormat)
		if err := os.RemoveAll(filepath.Join(m.cfg.Storage.SnapshotDir, name)); err != nil {
			slog.Error("snapshot prune", "name", name, "err", err)
			continue
		}
		slog.Info("snapshot pruned", "name", name)
	}
	m.list()
}

// Retain picks the snapshots to keep: the newest `last`, plus the newest of each
// of the latest `daily` days, `weekly` ISO weeks and `monthly` months that have
// snapshots. The newest snapshot is always kept.
func Retain(ts []time.Time, last, daily, weekly, monthly int) map[time.Time]bool {
	s := append([]time.Time(nil), ts...)
	sort.Slice(s, func(i, j int) bool { return s[i].After(s[j]) })
	keep := map[time.Time]bool{}
	if len(s) > 0 {
		keep[s[0]] = true
	}
	for i := 0; i < last && i < len(s); i++ {
		keep[s[i]] = true
	}
	bucket := func(n int, key func(time.Time) string) {
		seen := map[string]bool{}
		for _, t := range s {
			k := key(t)
			if seen[k] {
				continue
			}
			if len(seen) >= n {
				return
			}
			seen[k] = true
			keep[t] = true
		}
	}
	bucket(daily, func(t time.Time) string { return t.Format("2006-01-02") })
	bucket(weekly, func(t time.Time) string { y, w := t.ISOWeek(); return fmt.Sprintf("%d-W%02d", y, w) })
	bucket(monthly, func(t time.Time) string { return t.Format("2006-01") })
	return keep
}

// Now takes a snapshot immediately, regardless of interval, then prunes.
func (m *Manager) Now() error {
	name, err := m.Create(time.Now())
	if err != nil {
		m.update(func(s *Status) { s.LastError = err.Error() })
		return err
	}
	slog.Info("snapshot created", "name", name)
	m.update(func(s *Status) { s.LastError = "" })
	m.prune()
	return nil
}
