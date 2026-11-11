package crawler

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/schorsch3000/u64-sync/internal/config"
	"github.com/schorsch3000/u64-sync/internal/device"
	"github.com/schorsch3000/u64-sync/internal/fakeftp"
	"github.com/schorsch3000/u64-sync/internal/mirror"
	"github.com/schorsch3000/u64-sync/internal/store"
	"github.com/schorsch3000/u64-sync/internal/throttle"
)

var t0 = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

type env struct {
	t   *testing.T
	fs  *fakeftp.Server
	st  *store.Store
	mir *mirror.Mirror
	cr  *Crawler
	cfg config.Config
	gen int
}

func newEnv(t *testing.T, mod func(*config.Config)) *env {
	t.Helper()
	d := t.TempDir()
	fs := fakeftp.New(t)
	st, err := store.Open(filepath.Join(d, "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := config.Default()
	cfg.Device.Host, cfg.Device.FTPPort = fs.Host(), fs.Port()
	cfg.Device.Roots = []string{"/SD"}
	cfg.Sync.DeleteGrace = time.Hour
	if mod != nil {
		mod(&cfg)
	}
	mir := &mirror.Mirror{Current: filepath.Join(d, "cur"), Tmp: filepath.Join(d, "tmp")}
	os.MkdirAll(mir.Tmp, 0o755)
	wake := make(chan struct{}, 1)
	cr := New(cfg, st, device.New(fs.Host(), 1, "", time.Second), throttle.New(0, 0), mir, wake)
	return &env{t: t, fs: fs, st: st, mir: mir, cr: cr, cfg: cfg}
}

func (e *env) pass() error { return e.cr.pass(context.Background()) }

func (e *env) mustPass() {
	e.t.Helper()
	if err := e.pass(); err != nil {
		e.t.Fatalf("crawl pass: %v", err)
	}
}

func (e *env) counts() store.Counts {
	c, err := e.st.Counts()
	if err != nil {
		e.t.Fatal(err)
	}
	return c
}

// backedUp pretends the copier already mirrored every live file.
func (e *env) backedUp() {
	for {
		f, ok, _ := e.st.NextPending()
		if !ok {
			return
		}
		p, _ := e.mir.Path(f.Root, f.Path)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte("x"), 0o644)
		e.st.MarkBackedUp(f, "sha")
	}
}

func TestCrawlInventoriesTreeWithExcludes(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.Sync.Excludes = []string{"*.tmp", "Games/skip*"} })
	e.fs.Put("/SD/a.prg", "12345", t0)
	e.fs.Put("/SD/Games/b.d64", "123", t0.Add(time.Hour))
	e.fs.Put("/SD/Games/deep/er/c.prg", "1", t0)
	e.fs.Put("/SD/z.tmp", "junk", t0)            // excluded by base name
	e.fs.Put("/SD/Games/skipme.prg", "junk", t0) // excluded by relative path
	e.fs.MkDir("/SD/empty")
	e.mustPass()

	if c := e.counts(); c.Files != 3 || c.Pending != 3 || c.Bytes != 9 {
		t.Fatalf("counts %+v", c)
	}
	ents, _ := e.st.ListDir("/SD", "Games")
	if len(ents) != 2 || ents[0].Name != "deep" || ents[1].Name != "b.d64" || ents[1].Size != 3 || ents[1].MTime != t0.Add(time.Hour).Unix() {
		t.Fatalf("Games: %+v", ents)
	}
	if e.st.GetKV("crawl_ok_gen") == "" {
		t.Fatal("complete crawl not recorded")
	}
	if s := e.cr.Status(); !s.LastOK || s.Dirs < 4 {
		t.Fatalf("status %+v", s)
	}
}

func TestRecrawlPicksUpChangesAndKeepsUnchanged(t *testing.T) {
	e := newEnv(t, nil)
	e.fs.Put("/SD/same.prg", "aaa", t0)
	e.fs.Put("/SD/edit.prg", "bbb", t0)
	e.mustPass()
	e.backedUp()
	e.fs.Put("/SD/edit.prg", "bbbb", t0.Add(time.Minute))
	e.fs.Put("/SD/new.prg", "n", t0)
	e.mustPass()
	if c := e.counts(); c.Pending != 2 || c.Files != 3 {
		t.Fatalf("only edited and new files should be pending: %+v", c)
	}
}

func TestVanishedFileIsMarkedDeletedAfterCompleteCrawl(t *testing.T) {
	e := newEnv(t, nil)
	e.fs.Put("/SD/a.prg", "a", t0)
	e.fs.Put("/SD/b.prg", "b", t0)
	e.mustPass()
	e.backedUp()
	e.fs.Remove("/SD/b.prg")
	e.mustPass()
	if c := e.counts(); c.Files != 1 || c.Deleted != 1 {
		t.Fatalf("counts %+v", c)
	}
	// inside the grace period the local copy stays
	if p, _ := e.mir.Path("/SD", "b.prg"); !exists(p) {
		t.Fatal("local copy removed inside grace period")
	}
}

func TestPurgeRemovesLocalCopyAfterGrace(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.Sync.DeleteGrace = 0 })
	e.fs.Put("/SD/dir/a.prg", "a", t0)
	e.fs.Put("/SD/keep.prg", "k", t0)
	e.mustPass()
	e.backedUp()
	e.fs.Remove("/SD/dir/a.prg")
	e.fs.MkDir("/SD") // /SD still exists
	e.mustPass()
	p, _ := e.mir.Path("/SD", "dir/a.prg")
	if exists(p) || exists(filepath.Dir(p)) {
		t.Fatal("purged file or its empty directory still on disk")
	}
	if k, _ := e.mir.Path("/SD", "keep.prg"); !exists(k) {
		t.Fatal("unrelated file removed")
	}
	if c := e.counts(); c.Files != 1 || c.Deleted != 0 {
		t.Fatalf("counts %+v", c)
	}
}

func TestDeletionBrakeAndApproval(t *testing.T) {
	e := newEnv(t, nil) // brake: >=20 files and >20%
	for i := 0; i < 30; i++ {
		e.fs.Put(fmt.Sprintf("/SD/f%02d.prg", i), "x", t0)
	}
	e.mustPass()
	e.backedUp()
	for i := 0; i < 25; i++ {
		e.fs.Remove(fmt.Sprintf("/SD/f%02d.prg", i))
	}
	e.mustPass()
	if c := e.counts(); c.Deleted != 0 || c.Files != 30 {
		t.Fatalf("brake did not hold: %+v", c)
	}
	if b := e.cr.Status().Brake; len(b) != 1 || b[0] != "/SD" {
		t.Fatalf("brake not reported: %+v", b)
	}
	// still refused on the next crawl without approval
	e.mustPass()
	if c := e.counts(); c.Deleted != 0 {
		t.Fatalf("brake released by itself: %+v", c)
	}
	e.cr.ApproveBrake("/SD")
	e.mustPass()
	if c := e.counts(); c.Deleted != 25 || c.Files != 5 {
		t.Fatalf("approved deletions not applied: %+v", c)
	}
	if b := e.cr.Status().Brake; len(b) != 0 {
		t.Fatalf("brake still reported: %+v", b)
	}
	// approval is single-use
	e.fs.Put("/SD/new.prg", "n", t0)
	e.mustPass()
}

func TestSmallDeletionsPassTheBrake(t *testing.T) {
	e := newEnv(t, nil)
	for i := 0; i < 10; i++ {
		e.fs.Put(fmt.Sprintf("/SD/f%d.prg", i), "x", t0)
	}
	e.mustPass()
	for i := 0; i < 6; i++ { // 60%, but below delete_brake_min
		e.fs.Remove(fmt.Sprintf("/SD/f%d.prg", i))
	}
	e.mustPass()
	if c := e.counts(); c.Deleted != 6 {
		t.Fatalf("%+v", c)
	}
}

// A root that suddenly lists as empty (flaky device) must trip the brake, not wipe the mirror.
func TestEmptyListingTripsBrake(t *testing.T) {
	e := newEnv(t, nil)
	for i := 0; i < 30; i++ {
		e.fs.Put(fmt.Sprintf("/SD/f%02d.prg", i), "x", t0)
	}
	e.mustPass()
	for i := 0; i < 30; i++ {
		e.fs.Remove(fmt.Sprintf("/SD/f%02d.prg", i))
	}
	e.fs.MkDir("/SD")
	e.mustPass()
	if c := e.counts(); c.Deleted != 0 || c.Files != 30 {
		t.Fatalf("empty listing wiped the inventory: %+v", c)
	}
}

// A directory that cannot be listed makes the crawl incomplete: nothing may be
// treated as deleted, neither inside that directory nor elsewhere in the root.
func TestUnlistableDirectoryBlocksDeletions(t *testing.T) {
	e := newEnv(t, nil)
	e.fs.Put("/SD/a/x.prg", "x", t0)
	e.fs.Put("/SD/a/y.prg", "y", t0)
	e.fs.Put("/SD/b/z.prg", "z", t0)
	e.mustPass()
	okGen := e.st.GetKV("crawl_ok_gen")

	e.fs.FailList("/SD/b", true)
	e.fs.Remove("/SD/a/y.prg")
	e.mustPass() // pass itself succeeds, but is incomplete
	if c := e.counts(); c.Deleted != 0 || c.Files != 3 {
		t.Fatalf("deletions applied on an incomplete crawl: %+v", c)
	}
	if got := e.st.GetKV("crawl_ok_gen"); got != okGen {
		t.Fatalf("incomplete crawl recorded as complete: %q -> %q", okGen, got)
	}
	if s := e.cr.Status(); s.LastOK || s.LastError == "" {
		t.Fatalf("status %+v", s)
	}

	e.fs.FailList("/SD/b", false)
	e.mustPass()
	if c := e.counts(); c.Deleted != 1 || c.Files != 2 {
		t.Fatalf("after recovery: %+v", c)
	}
}

func TestConnectionDropAbortsPassWithoutDeleting(t *testing.T) {
	e := newEnv(t, nil)
	e.fs.Put("/SD/a/x.prg", "x", t0)
	e.fs.Put("/SD/b/y.prg", "y", t0)
	e.mustPass()
	e.fs.Remove("/SD/b/y.prg")
	e.fs.DropList("/SD/a", true)
	if err := e.pass(); err == nil {
		t.Fatal("expected an error when the device hangs up")
	}
	if c := e.counts(); c.Deleted != 0 || c.Files != 2 {
		t.Fatalf("%+v", c)
	}
	if s := e.cr.Status(); s.Running || s.LastOK {
		t.Fatalf("status %+v", s)
	}
}

// One root the device cannot list must not stop the other roots from being crawled.
func TestMissingRootDoesNotBlockOtherRoots(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.Device.Roots = []string{"/Flash", "/SD"} })
	e.fs.Put("/SD/a.prg", "a", t0) // /Flash does not exist on the device
	e.mustPass()
	if c := e.counts(); c.Files != 1 {
		t.Fatalf("SD not crawled: %+v", c)
	}
	if e.st.GetKV("crawl_ok_gen") != "" {
		t.Fatal("crawl with a missing root recorded as complete")
	}
}

func TestNoServerIsAnError(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.Device.FTPPort = 1 })
	if err := e.pass(); err == nil {
		t.Fatal("expected dial error")
	}
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }
