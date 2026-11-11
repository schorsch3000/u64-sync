package store

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func f(path string, size, mtime int64) File {
	return File{Root: "/SD", Path: path, Size: size, MTime: mtime}
}

func mustNext(t *testing.T, s *Store) File {
	t.Helper()
	got, ok, err := s.NextPending()
	if err != nil || !ok {
		t.Fatalf("expected a pending file: ok=%v err=%v", ok, err)
	}
	return got
}

func mustNone(t *testing.T, s *Store) {
	t.Helper()
	if got, ok, err := s.NextPending(); err != nil || ok {
		t.Fatalf("expected nothing pending, got %+v ok=%v err=%v", got, ok, err)
	}
}

func TestReopenKeepsData(t *testing.T) {
	p := filepath.Join(t.TempDir(), "db")
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	s.Upsert([]File{f("a", 1, 1)}, 1)
	s.Close()
	s, err = Open(p) // migrations must be idempotent
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if c, _ := s.Counts(); c.Files != 1 {
		t.Fatalf("counts after reopen: %+v", c)
	}
}

func TestPendingLifecycle(t *testing.T) {
	s := open(t)
	s.Upsert([]File{f("a.prg", 10, 100)}, 1)
	got := mustNext(t, s)
	if got != f("a.prg", 10, 100) {
		t.Fatalf("got %+v", got)
	}
	if err := s.MarkBackedUp(got, "sha"); err != nil {
		t.Fatal(err)
	}
	mustNone(t, s)

	// same metadata on the next crawl: stays backed up
	s.Upsert([]File{f("a.prg", 10, 100)}, 2)
	mustNone(t, s)

	// size change and mtime change each re-queue
	s.Upsert([]File{f("a.prg", 11, 100)}, 3)
	mustNext(t, s)
	s.MarkBackedUp(f("a.prg", 11, 100), "sha2")
	mustNone(t, s)
	s.Upsert([]File{f("a.prg", 11, 200)}, 4)
	mustNext(t, s)
}

// A file that changed on the device while we copied the older version must stay queued.
func TestMarkBackedUpKeepsNewerRemoteQueued(t *testing.T) {
	s := open(t)
	s.Upsert([]File{f("a", 10, 100)}, 1)
	old := mustNext(t, s)
	s.Upsert([]File{f("a", 12, 200)}, 2) // crawler saw a newer version meanwhile
	s.MarkBackedUp(old, "sha-old")
	got := mustNext(t, s)
	if got.Size != 12 || got.MTime != 200 {
		t.Fatalf("queued %+v", got)
	}
	// a crawl that sees the unchanged newer file must not drop it from the queue
	s.Upsert([]File{f("a", 12, 200)}, 3)
	mustNext(t, s)
}

func TestNewFilesBeforeChangedFiles(t *testing.T) {
	s := open(t)
	s.Upsert([]File{f("a-changed", 1, 1)}, 1)
	s.MarkBackedUp(f("a-changed", 1, 1), "x")
	s.Upsert([]File{f("a-changed", 2, 2), f("z-new", 1, 1)}, 2)
	if got := mustNext(t, s); got.Path != "z-new" {
		t.Fatalf("new file must come first, got %q", got.Path)
	}
}

func TestFailBackoffAndReset(t *testing.T) {
	s := open(t)
	s.Upsert([]File{f("a", 1, 1)}, 1)
	x := mustNext(t, s)
	if err := s.Fail(x, errors.New("boom")); err != nil {
		t.Fatal(err)
	}
	mustNone(t, s) // backed off
	c, _ := s.Counts()
	if c.Failing != 1 || c.Pending != 1 {
		t.Fatalf("counts %+v", c)
	}
	s.Upsert([]File{f("a", 1, 1)}, 2) // unchanged: still backed off
	mustNone(t, s)
	s.Upsert([]File{f("a", 2, 1)}, 3) // changed on device: retry right away, failures forgotten
	mustNext(t, s)
	if c, _ := s.Counts(); c.Failing != 0 {
		t.Fatalf("failure state not reset: %+v", c)
	}
	// backoff grows
	s.Fail(f("a", 2, 1), errors.New("1"))
	s.Fail(f("a", 2, 1), errors.New("2"))
	var next int64
	s.db.QueryRow(`SELECT next_try FROM files`).Scan(&next)
	if d := time.Until(time.Unix(next, 0)); d < 3*time.Minute {
		t.Fatalf("backoff after 2 failures too short: %v", d)
	}
}

func TestDeletionLifecycle(t *testing.T) {
	s := open(t)
	s.Upsert([]File{f("keep", 1, 1), f("gone", 1, 1)}, 1)
	s.Upsert([]File{f("keep", 1, 1)}, 2) // "gone" not seen in gen 2

	missing, live, _ := s.CountMissing("/SD", 2)
	if missing != 1 || live != 2 {
		t.Fatalf("missing=%d live=%d", missing, live)
	}
	if n, _ := s.MarkMissingDeleted("/SD", 2); n != 1 {
		t.Fatalf("marked %d", n)
	}
	if c, _ := s.Counts(); c.Files != 1 || c.Deleted != 1 {
		t.Fatalf("counts %+v", c)
	}
	// a deleted file is not queued for backup
	s.MarkBackedUp(f("keep", 1, 1), "x")
	mustNone(t, s)

	if p, _ := s.Purgeable(time.Hour); len(p) != 0 {
		t.Fatal("purged inside grace period")
	}
	p, _ := s.Purgeable(0)
	if len(p) != 1 || p[0].Path != "gone" {
		t.Fatalf("purgeable %+v", p)
	}
	s.Forget(p[0])
	if c, _ := s.Counts(); c.Deleted != 0 {
		t.Fatalf("not forgotten: %+v", c)
	}
}

func TestReappearingFileIsRevived(t *testing.T) {
	s := open(t)
	s.Upsert([]File{f("a", 1, 1)}, 1)
	s.MarkBackedUp(f("a", 1, 1), "x")
	s.MarkMissingDeleted("/SD", 2)
	s.Upsert([]File{f("a", 1, 1)}, 3)
	c, _ := s.Counts()
	if c.Files != 1 || c.Deleted != 0 || c.Pending != 0 {
		t.Fatalf("revived file should be live and still backed up: %+v", c)
	}
	// Forget must never delete a live row
	s.Forget(f("a", 1, 1))
	if c, _ := s.Counts(); c.Files != 1 {
		t.Fatal("Forget removed a live file")
	}
}

func TestListDirAndBounds(t *testing.T) {
	s := open(t)
	s.Upsert([]File{
		f("a/x.prg", 5, 10), f("a/sub/y.prg", 7, 20), f("a/sub/z.prg", 1, 30),
		f("a0/other", 100, 1), f("a-b/other", 100, 1), f("top.prg", 3, 5),
	}, 1)
	s.MarkBackedUp(f("a/x.prg", 5, 10), "x")

	ents, err := s.ListDir("/SD", "a")
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 2 {
		t.Fatalf("siblings a0 / a-b leaked into the listing: %+v", ents)
	}
	if !ents[0].IsDir || ents[0].Name != "sub" || ents[0].Files != 2 || ents[0].Size != 8 || ents[0].Pending != 2 || ents[0].MTime != 30 {
		t.Fatalf("dir entry: %+v", ents[0])
	}
	if ents[1].IsDir || ents[1].Name != "x.prg" || ents[1].Pending != 0 {
		t.Fatalf("file entry: %+v", ents[1])
	}

	root, _ := s.ListDir("/SD", "")
	var names []string
	for _, e := range root {
		names = append(names, e.Name)
	}
	if len(names) != 4 || names[0] != "a" || names[3] != "top.prg" { // dirs first, case-insensitive order
		t.Fatalf("root listing: %v", names)
	}

	n, b, pend, _ := s.DirStat("/SD", "a")
	if n != 3 || b != 13 || pend != 2 {
		t.Fatalf("DirStat: %d %d %d", n, b, pend)
	}
	if n, _, _, _ := s.DirStat("/SD", ""); n != 6 {
		t.Fatalf("root DirStat: %d", n)
	}
}

func TestVerifyBatchAndFlagCorrupt(t *testing.T) {
	s := open(t)
	s.Upsert([]File{f("a", 3, 1), f("never", 1, 1)}, 1)
	s.MarkBackedUp(f("a", 3, 1), "sha")
	// only backed-up files are verifiable; fresh downloads count as just verified
	if b, _ := s.VerifyBatch(10, time.Now().Add(-time.Hour)); len(b) != 0 {
		t.Fatalf("freshly backed up file due: %+v", b)
	}
	b, _ := s.VerifyBatch(10, time.Now().Add(time.Hour))
	if len(b) != 1 || b[0].Path != "a" || b[0].SHA256 != "sha" || b[0].BSize != 3 {
		t.Fatalf("batch %+v", b)
	}
	s.FlagCorrupt(f("a", 3, 1), "bad")
	if x := mustNext(t, s); x.Path != "a" {
		t.Fatalf("flagged file not queued: %+v", x)
	}
	if s.BackupSHA(f("a", 3, 1)) != "" {
		t.Fatal("backup hash not cleared")
	}
}
