// Package e2e runs the real crawler, copier, verifier and device monitor
// against a fake Ultimate64 (FTP + REST) and checks the mirror end to end.
package e2e

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/schorsch3000/u64-sync/internal/config"
	"github.com/schorsch3000/u64-sync/internal/copier"
	"github.com/schorsch3000/u64-sync/internal/crawler"
	"github.com/schorsch3000/u64-sync/internal/device"
	"github.com/schorsch3000/u64-sync/internal/fakeftp"
	"github.com/schorsch3000/u64-sync/internal/mirror"
	"github.com/schorsch3000/u64-sync/internal/store"
	"github.com/schorsch3000/u64-sync/internal/throttle"
	"github.com/schorsch3000/u64-sync/internal/verify"
)

var t0 = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

type system struct {
	t    *testing.T
	fs   *fakeftp.Server
	st   *store.Store
	mir  *mirror.Mirror
	cr   *crawler.Crawler
	dev  *device.Monitor
	down atomic.Bool // REST API hangs up while set
}

func start(t *testing.T) *system {
	t.Helper()
	d := t.TempDir()
	s := &system{t: t, fs: fakeftp.New(t)}

	rest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.down.Load() {
			c, _, _ := http.NewResponseController(w).Hijack()
			c.Close()
			return
		}
		w.Write([]byte(`{"version":"0.1","errors":[]}`))
	}))
	t.Cleanup(rest.Close)
	restPort := rest.Listener.Addr().(*net.TCPAddr).Port

	var err error
	if s.st, err = store.Open(filepath.Join(d, "db")); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Device.Host, cfg.Device.FTPPort, cfg.Device.RESTPort = s.fs.Host(), s.fs.Port(), restPort
	cfg.Device.Roots = []string{"/SD", "/Flash"}
	cfg.Sync.DeleteGrace = 0
	cfg.Verify.Interval = time.Second
	cfg.Verify.BytesPerSecond = 0

	s.mir = &mirror.Mirror{Current: filepath.Join(d, "cur"), Tmp: filepath.Join(d, "tmp")}
	os.MkdirAll(s.mir.Tmp, 0o755)
	th := throttle.New(0, 0)
	s.dev = device.New(cfg.Device.Host, cfg.Device.RESTPort, "", time.Second)
	cp := copier.New(cfg, s.st, s.dev, th, s.mir)
	s.cr = crawler.New(cfg, s.st, s.dev, th, s.mir, cp.Wake)
	vf := verify.New(cfg.Verify, s.st, s.mir, cp.Wake)
	vf.Poll = 100 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for _, run := range []func(){
		func() { s.dev.Run(ctx, 50*time.Millisecond) },
		func() { s.cr.Run(ctx) },
		func() { cp.Run(ctx) },
		func() { vf.Run(ctx) },
	} {
		wg.Add(1)
		go func() { defer wg.Done(); run() }()
	}
	t.Cleanup(func() { cancel(); wg.Wait(); s.st.Close() })
	return s
}

func (s *system) crawlNow() {
	select {
	case s.cr.Trigger <- struct{}{}:
	default:
	}
}

func (s *system) local(root, p string) (string, bool) {
	lp, _ := s.mir.Path(root, p)
	b, err := os.ReadFile(lp)
	return string(b), err == nil
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

func (s *system) hasContent(root, p, want string) func() bool {
	return func() bool { got, ok := s.local(root, p); return ok && got == want }
}

func TestMirrorLifecycle(t *testing.T) {
	s := start(t)
	s.fs.Put("/SD/a.prg", "AAA", t0)
	s.fs.Put("/SD/Games/b.d64", "BBBBBBBB", t0)
	s.fs.Put("/Flash/f.cfg", "cfg", t0)

	t.Log("initial sync")
	eventually(t, "initial mirror", func() bool {
		return s.hasContent("/SD", "a.prg", "AAA")() && s.hasContent("/SD", "Games/b.d64", "BBBBBBBB")() && s.hasContent("/Flash", "f.cfg", "cfg")()
	})
	eventually(t, "complete sync recorded", func() bool { return s.st.GetKV("complete_gen") != "" })
	lp, _ := s.mir.Path("/SD", "a.prg")
	if st, _ := os.Stat(lp); !st.ModTime().Equal(t0) {
		t.Fatalf("mtime %v", st.ModTime())
	}

	t.Log("changed + new file on the device")
	s.fs.Put("/SD/a.prg", "AAA-edited", t0.Add(time.Hour))
	s.fs.Put("/SD/new.prg", "N", t0)
	s.crawlNow()
	eventually(t, "changes mirrored", func() bool {
		return s.hasContent("/SD", "a.prg", "AAA-edited")() && s.hasContent("/SD", "new.prg", "N")()
	})

	t.Log("file deleted on the device (grace period 0)")
	s.fs.Remove("/SD/Games/b.d64")
	s.crawlNow()
	eventually(t, "deleted file removed locally", func() bool {
		_, ok := s.local("/SD", "Games/b.d64")
		return !ok
	})
	// The file disappears a moment before its empty folder (and on Windows a deleted
	// file that is still open delays that), so wait; the next crawl's cleanup catches stragglers.
	eventually(t, "empty directory pruned", func() bool {
		s.crawlNow()
		_, err := os.Stat(filepath.Join(s.mir.Current, "SD", "Games"))
		return os.IsNotExist(err)
	})

	t.Log("bit rot on the backup disk is detected and repaired")
	rotten, _ := s.mir.Path("/SD", "new.prg")
	if err := os.WriteFile(rotten, []byte("X"), 0o644); err != nil { // same size, different content
		t.Fatal(err)
	}
	eventually(t, "verifier repaired the file", s.hasContent("/SD", "new.prg", "N"))

	t.Log("local file deleted by accident")
	os.Remove(rotten)
	eventually(t, "missing file restored", s.hasContent("/SD", "new.prg", "N"))
}

func TestNothingHappensWhileDeviceIsDown(t *testing.T) {
	s := start(t)
	s.fs.Put("/SD/a.prg", "A", t0)
	eventually(t, "first sync", s.hasContent("/SD", "a.prg", "A"))

	s.down.Store(true)
	eventually(t, "device seen as down", func() bool { return !s.dev.Up() })
	s.fs.Put("/SD/late.prg", "L", t0)
	s.crawlNow()
	time.Sleep(500 * time.Millisecond)
	if _, ok := s.local("/SD", "late.prg"); ok {
		t.Fatal("synced from a device that is down")
	}

	s.down.Store(false)
	eventually(t, "sync resumes when the device is back", s.hasContent("/SD", "late.prg", "L"))
}
