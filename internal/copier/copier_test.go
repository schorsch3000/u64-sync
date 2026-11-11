package copier

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/schorsch3000/u64-sync/internal/config"
	"github.com/schorsch3000/u64-sync/internal/fakeftp"
	"github.com/schorsch3000/u64-sync/internal/ftpx"
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
	cp  *Copier
	cl  *ftpx.Client
	gen int64
}

func newEnv(t *testing.T) *env {
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
	mir := &mirror.Mirror{Current: filepath.Join(d, "cur"), Tmp: filepath.Join(d, "tmp")}
	os.MkdirAll(mir.Tmp, 0o755)
	e := &env{t: t, fs: fs, st: st, mir: mir, cp: New(cfg, st, nil, throttle.New(0, 0), mir)}
	e.dial()
	return e
}

func (e *env) dial() {
	e.t.Helper()
	cl, err := ftpx.Dial(context.Background(), e.fs.Host(), e.fs.Port(), "anonymous", "")
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(cl.Close)
	e.cl = cl
}

// put adds a file on the "device" and crawls it into the inventory.
func (e *env) put(p, data string, mtime time.Time) store.File {
	e.t.Helper()
	e.fs.Put("/SD/"+p, data, mtime)
	e.gen++
	f := store.File{Root: "/SD", Path: p, Size: int64(len(data)), MTime: mtime.Unix()}
	if err := e.st.Upsert([]store.File{f}, e.gen); err != nil {
		e.t.Fatal(err)
	}
	return f
}

func (e *env) local(p string) (string, bool) {
	lp, _ := e.mir.Path("/SD", p)
	b, err := os.ReadFile(lp)
	return string(b), err == nil
}

func (e *env) tmpFiles() int {
	ents, _ := os.ReadDir(e.mir.Tmp)
	return len(ents)
}

func (e *env) copy(f store.File) error { return e.cp.copyOne(context.Background(), e.cl, f) }

func TestCopyOneInstallsFileWithMetadata(t *testing.T) {
	e := newEnv(t)
	f := e.put("Games/deep/a.prg", "hello c64", t0)
	if err := e.copy(f); err != nil {
		t.Fatal(err)
	}
	if got, ok := e.local("Games/deep/a.prg"); !ok || got != "hello c64" {
		t.Fatalf("local content %q ok=%v", got, ok)
	}
	lp, _ := e.mir.Path("/SD", "Games/deep/a.prg")
	if st, _ := os.Stat(lp); !st.ModTime().Equal(t0) {
		t.Fatalf("mtime %v, want %v", st.ModTime(), t0)
	}
	if e.tmpFiles() != 0 {
		t.Fatal("temp file left behind")
	}
	if c, _ := e.st.Counts(); c.Pending != 0 {
		t.Fatalf("still pending: %+v", c)
	}
	sum := sha256.Sum256([]byte("hello c64"))
	if got := e.st.BackupSHA(f); got != hex.EncodeToString(sum[:]) {
		t.Fatalf("stored sha %q", got)
	}
}

func TestCopyEmptyFile(t *testing.T) {
	e := newEnv(t)
	if err := e.copy(e.put("empty.prg", "", t0)); err != nil {
		t.Fatal(err)
	}
	if got, ok := e.local("empty.prg"); !ok || got != "" {
		t.Fatalf("%q %v", got, ok)
	}
}

// The file changes on the device while we download it: the half-old, half-new
// bytes must not be installed; the new metadata is recorded and the next
// attempt gets the new version.
func TestFileChangedDuringDownload(t *testing.T) {
	e := newEnv(t)
	f := e.put("a.prg", "version-1", t0)
	e.fs.OnRetr(func(p string) {
		e.fs.Put(p, "version-2-longer", t0.Add(time.Minute))
	})
	err := e.copy(f)
	if !errors.Is(err, errChanged) {
		t.Fatalf("got %v, want errChanged", err)
	}
	if _, ok := e.local("a.prg"); ok {
		t.Fatal("inconsistent download was installed")
	}
	if e.tmpFiles() != 0 {
		t.Fatal("temp file left behind")
	}
	next, ok, _ := e.st.NextPending()
	if !ok || next.Size != int64(len("version-2-longer")) || next.MTime != t0.Add(time.Minute).Unix() {
		t.Fatalf("new metadata not recorded: %+v ok=%v", next, ok)
	}
	e.fs.OnRetr(nil)
	if err := e.copy(next); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.local("a.prg"); got != "version-2-longer" {
		t.Fatalf("content %q", got)
	}
}

// Same size, only the mtime moved: still a change.
func TestMtimeChangeDuringDownload(t *testing.T) {
	e := newEnv(t)
	f := e.put("a.prg", "same-size", t0)
	e.fs.OnRetr(func(p string) { e.fs.Put(p, "diff-size!", t0.Add(time.Hour)) })
	if err := e.copy(f); !errors.Is(err, errChanged) {
		t.Fatalf("got %v", err)
	}
	e.fs.OnRetr(func(p string) { e.fs.Put(p, "SAME-SIZE", t0.Add(time.Hour)) })
	f2 := e.put("b.prg", "same-size", t0)
	if err := e.copy(f2); !errors.Is(err, errChanged) {
		t.Fatalf("mtime-only change not detected: %v", err)
	}
	if _, ok := e.local("b.prg"); ok {
		t.Fatal("installed")
	}
}

// The listing said 5 bytes but the device delivers something else.
func TestStaleSizeInInventory(t *testing.T) {
	e := newEnv(t)
	f := e.put("a.prg", "12345", t0)
	e.fs.Put("/SD/a.prg", "123456789", t0) // changed since the crawl
	if err := e.copy(f); !errors.Is(err, errChanged) {
		t.Fatalf("got %v", err)
	}
	if _, ok := e.local("a.prg"); ok {
		t.Fatal("installed")
	}
}

// Device drops dead mid-transfer: the old local copy must survive untouched, no temp litter.
func TestConnectionDropKeepsOldCopy(t *testing.T) {
	e := newEnv(t)
	old := e.put("a.prg", "OLD-CONTENT", t0)
	if err := e.copy(old); err != nil {
		t.Fatal(err)
	}
	newer := e.put("a.prg", "NEW-CONTENT-IS-LONGER", t0.Add(time.Hour))
	e.fs.DropRetr("/SD/a.prg", true)
	if err := e.copy(newer); err == nil || errors.Is(err, errChanged) {
		t.Fatalf("expected a connection error, got %v", err)
	}
	if got, _ := e.local("a.prg"); got != "OLD-CONTENT" {
		t.Fatalf("old copy damaged: %q", got)
	}
	if e.tmpFiles() != 0 {
		t.Fatal("partial download left in tmp")
	}
	if c, _ := e.st.Counts(); c.Pending != 1 {
		t.Fatalf("file must stay queued: %+v", c)
	}
	// the dead connection is unusable; a fresh one works once the device is back
	e.fs.DropRetr("/SD/a.prg", false)
	e.dial()
	if err := e.copy(newer); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.local("a.prg"); got != "NEW-CONTENT-IS-LONGER" {
		t.Fatalf("content %q", got)
	}
}

func TestMissingRemoteFileIsReplyError(t *testing.T) {
	e := newEnv(t)
	f := e.put("a.prg", "x", t0)
	e.fs.Remove("/SD/a.prg") // vanished after the crawl
	err := e.copy(f)
	var re *ftpx.ReplyError
	if !errors.As(err, &re) || re.Code != 550 {
		t.Fatalf("got %v", err)
	}
	// a clean negative reply keeps the connection usable
	g := e.put("b.prg", "y", t0)
	if err := e.copy(g); err != nil {
		t.Fatalf("connection should survive a 550: %v", err)
	}
}

func TestUnsafePathIsRejected(t *testing.T) {
	e := newEnv(t)
	err := e.copy(store.File{Root: "/SD", Path: "../../etc/passwd", Size: 1, MTime: 1})
	if err == nil || !strings.Contains(err.Error(), "unsafe") {
		t.Fatalf("got %v", err)
	}
}

func TestMarkCompleteOnlyWhenQueueEmpty(t *testing.T) {
	e := newEnv(t)
	f := e.put("a.prg", "x", t0)
	e.st.SetKV("crawl_ok_gen", "1")
	e.cp.markComplete()
	if e.st.GetKV("complete_gen") != "" {
		t.Fatal("marked complete with a pending file")
	}
	e.copy(f)
	e.cp.markComplete()
	if e.st.GetKV("complete_gen") != "1" || e.st.GetKV("complete_at") == "" {
		t.Fatal("not marked complete")
	}
	// no completed crawl yet: nothing to mark
	e2 := newEnv(t)
	e2.cp.markComplete()
	if e2.st.GetKV("complete_gen") != "" {
		t.Fatal("marked complete without a crawl")
	}
}
