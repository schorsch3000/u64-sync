package snapshot

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/schorsch3000/u64-sync/internal/config"
	"github.com/schorsch3000/u64-sync/internal/mirror"
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
