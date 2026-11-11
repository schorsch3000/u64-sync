package snapshot

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/schorsch3000/u64-sync/internal/config"
	"github.com/schorsch3000/u64-sync/internal/mirror"
	"github.com/schorsch3000/u64-sync/internal/store"
)

func day(d int, h int) time.Time { return time.Date(2026, 10, d, h, 0, 0, 0, time.UTC) }

func TestRetain(t *testing.T) {
	var ts []time.Time
	for d := 1; d <= 20; d++ {
		ts = append(ts, day(d, 3), day(d, 15))
	}
	k := Retain(ts, 1, 0, 0, 0)
	if len(k) != 1 || !k[day(20, 15)] {
		t.Fatalf("last=1: %v", k)
	}
	k = Retain(ts, 1, 3, 0, 0) // newest of days 20,19,18
	if len(k) != 3 || !k[day(20, 15)] || !k[day(19, 15)] || !k[day(18, 15)] {
		t.Fatalf("daily=3: %v", k)
	}
	k = Retain(ts, 0, 0, 2, 1) // weeks + month
	if !k[day(20, 15)] || len(k) < 2 {
		t.Fatalf("weekly/monthly: %v", k)
	}
	if k := Retain(nil, 1, 1, 1, 1); len(k) != 0 {
		t.Fatal("empty input")
	}
	// newest is kept even with an all-zero policy
	if k := Retain(ts, 0, 0, 0, 0); len(k) != 1 || !k[day(20, 15)] {
		t.Fatalf("zero policy: %v", k)
	}
}

func TestCreateHardlinks(t *testing.T) {
	d := t.TempDir()
	cfg := config.Default()
	cfg.Storage.SnapshotDir = filepath.Join(d, "snap")
	mir := &mirror.Mirror{Current: filepath.Join(d, "cur")}
	os.MkdirAll(cfg.Storage.SnapshotDir, 0o755)
	os.MkdirAll(filepath.Join(mir.Current, "SD", "sub"), 0o755)
	os.MkdirAll(filepath.Join(mir.Current, "SD", "empty"), 0o755)
	f := filepath.Join(mir.Current, "SD", "sub", "a.prg")
	os.WriteFile(f, []byte("x"), 0o644)

	m := New(cfg, nil, mir)
	name, err := m.Create(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	a, _ := os.Stat(f)
	b, err := os.Stat(filepath.Join(cfg.Storage.SnapshotDir, name, "SD", "sub", "a.prg"))
	if err != nil || !os.SameFile(a, b) {
		t.Fatalf("not hardlinked: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.Storage.SnapshotDir, name, "SD", "empty")); err != nil {
		t.Fatal("empty dir missing")
	}
}

func TestTickSnapshotsAfterCompleteSyncOnceAndPrunes(t *testing.T) {
	d := t.TempDir()
	st, err := store.Open(filepath.Join(d, "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := config.Default()
	cfg.Storage.SnapshotDir = filepath.Join(d, "snap")
	cfg.Snapshots.Interval = time.Hour
	mir := &mirror.Mirror{Current: filepath.Join(d, "cur")}
	os.MkdirAll(cfg.Storage.SnapshotDir, 0o755)
	os.MkdirAll(filepath.Join(mir.Current, "SD"), 0o755)
	os.WriteFile(filepath.Join(mir.Current, "SD", "a"), []byte("x"), 0o644)
	m := New(cfg, st, mir)
	count := func() int { n, _ := m.list(); return len(n) }

	m.tick() // no complete sync yet
	if count() != 0 {
		t.Fatal("snapshot without a complete sync")
	}
	st.SetKV("complete_gen", "1")
	m.tick()
	if count() != 1 {
		t.Fatal("no snapshot after a complete sync")
	}
	m.tick() // same generation: nothing new
	st.SetKV("complete_gen", "2")
	m.tick() // new generation but interval not over
	if count() != 1 {
		t.Fatalf("interval ignored: %d snapshots", count())
	}
	// age the snapshot beyond the interval: a new one is taken and keep.last=1 prunes the old one
	old, _ := m.list()
	oldDir := filepath.Join(cfg.Storage.SnapshotDir, old[0].Format(nameFormat))
	aged := old[0].Add(-2 * time.Hour).Format(nameFormat)
	os.Rename(oldDir, filepath.Join(cfg.Storage.SnapshotDir, aged))
	m.tick()
	names, _ := m.list()
	if len(names) != 1 || names[0].Format(nameFormat) == aged {
		t.Fatalf("expected only the new snapshot, got %v", names)
	}
}
