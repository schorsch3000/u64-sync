package web

import (
	"archive/zip"
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/schorsch3000/u64-sync/internal/config"
	"github.com/schorsch3000/u64-sync/internal/copier"
	"github.com/schorsch3000/u64-sync/internal/crawler"
	"github.com/schorsch3000/u64-sync/internal/device"
	"github.com/schorsch3000/u64-sync/internal/meter"
	"github.com/schorsch3000/u64-sync/internal/mirror"
	"github.com/schorsch3000/u64-sync/internal/snapshot"
	"github.com/schorsch3000/u64-sync/internal/store"
	"github.com/schorsch3000/u64-sync/internal/throttle"
	"github.com/schorsch3000/u64-sync/internal/verify"
)

func testServer(t *testing.T) (http.Handler, *store.Store, *mirror.Mirror) {
	t.Helper()
	s, st, mir := newServer(t)
	h, err := s.Handler()
	if err != nil {
		t.Fatal(err)
	}
	return h, st, mir
}

func newServer(t *testing.T) (*Server, *store.Store, *mirror.Mirror) {
	t.Helper()
	d := t.TempDir()
	st, err := store.Open(filepath.Join(d, "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := config.Default()
	cfg.Storage.SnapshotDir = filepath.Join(d, "snap")
	mir := &mirror.Mirror{Current: filepath.Join(d, "cur"), Tmp: filepath.Join(d, "tmp")}
	dev := device.New("127.0.0.1", 1, "", time.Second)
	th := throttle.New(0, 0)
	cp := copier.New(cfg, st, dev, th, mir)
	s := &Server{
		Store: st, Device: dev, Crawler: crawler.New(cfg, st, dev, th, mir, cp.Wake), Copier: cp,
		Snapshots: snapshot.New(cfg, st, mir), Throttle: th, Mirror: mir, Verifier: verify.New(cfg.Verify, st, mir, cp.Wake),
		Roots: []string{"/SD", "/Flash"},
	}
	if err := s.load(); err != nil {
		t.Fatal(err)
	}
	return s, st, mir
}

func get(h http.Handler, method, url string) (int, string, http.Header) {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(method, url, nil))
	b, _ := io.ReadAll(w.Result().Body)
	return w.Code, string(b), w.Header()
}

// Every page must render without template errors, empty and with data.
func TestPagesRender(t *testing.T) {
	h, st, mir := testServer(t)

	empty := []string{"/", "/ui/status", "/files/", "/api/status"}
	for _, u := range empty {
		if code, body, _ := get(h, "GET", u); code != 200 || len(body) < 50 {
			t.Fatalf("empty %s: %d %q", u, code, body)
		}
	}

	files := []store.File{
		{Root: "/SD", Path: "Games/a.prg", Size: 2048, MTime: 1772366400},
		{Root: "/SD", Path: "Games/sub/b.d64", Size: 174848, MTime: 1772366400},
		{Root: "/SD", Path: "ok.txt", Size: 5, MTime: 1772366400},
		{Root: "/SD", Path: "gone.txt", Size: 5, MTime: 1772366400},
		{Root: "/Flash", Path: "f.cfg", Size: 9, MTime: 1772366400},
	}
	st.Upsert(files, 1)
	st.MarkBackedUp(files[2], "sha")
	st.Fail(files[1], errors.New("boom <b>x</b>"))
	st.Upsert(files[:3], 2)
	st.MarkMissingDeleted("/SD", 2)
	p, _ := mir.Path("/SD", "ok.txt")
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, []byte("hello"), 0o644)

	for _, c := range []struct {
		url  string
		code int
		want []string
	}{
		{"/", 200, []string{"</html>", "app.css", "hx-get=\"/ui/status\""}},
		{"/ui/status", 200, []string{"Failing files", "boom &lt;b&gt;x&lt;/b&gt;", "Controls", "Backed up"}},
		{"/files/", 200, []string{"SD", "Flash", "Download as zip"}},
		{"/files/SD", 200, []string{"Games", "ok.txt", "deleted on device", `id="filter"`}},
		{"/files/SD/Games", 200, []string{"a.prg", "failing", "sub"}},
		{"/files/SD/nope", 404, nil},
		{"/files/Nope", 404, nil},
		{"/static/app.css", 200, []string{".badge"}},
		{"/static/app.js", 200, []string{"filter"}},
		{"/favicon.ico", 200, []string{"<svg"}},
		{"/healthz", 200, []string{"ok"}},
		{"/dl/SD/ok.txt", 200, []string{"hello"}},
		{"/dl/SD/missing.txt", 404, nil},
		{"/dl/SD", 404, nil},
	} {
		code, body, _ := get(h, "GET", c.url)
		if code != c.code {
			t.Errorf("%s: status %d, want %d", c.url, code, c.code)
			continue
		}
		for _, w := range c.want {
			if !strings.Contains(body, w) {
				t.Errorf("%s: missing %q in body", c.url, w)
			}
		}
	}

	code, body, hdr := get(h, "GET", "/zip/SD")
	if code != 200 || hdr.Get("Content-Type") != "application/zip" {
		t.Fatalf("zip: %d %v", code, hdr)
	}
	zr, err := zip.NewReader(bytes.NewReader([]byte(body)), int64(len(body)))
	if err != nil || len(zr.File) != 1 || zr.File[0].Name != "SD/ok.txt" {
		t.Fatalf("zip content: %v %v", err, zr)
	}
}

func TestActions(t *testing.T) {
	h, _, _ := testServer(t)
	for _, u := range []string{"/ui/crawl", "/ui/snapshot"} {
		if code, _, _ := get(h, "POST", u); code != 200 && code != 400 {
			t.Errorf("%s: %d", u, code)
		}
	}
	if code, _, _ := get(h, "POST", "/ui/approve?root=/Nope"); code != 400 {
		t.Errorf("approve unknown root: %d", code)
	}
}

func TestSpeedBlockRenders(t *testing.T) {
	s, st, _ := newServer(t)
	st.Upsert([]store.File{{Root: "/SD", Path: "big.bin", Size: 10 << 20, MTime: 1}}, 1)
	view := s.status()
	view.Copier.Rates = meter.Rates{Last1m: 182 * 1024, Last5m: 175 * 1024, Sync: 160 * 1024,
		SyncBytes: 50 << 20, SyncDuration: 5*time.Minute + 20*time.Second, Running: true}
	w := httptest.NewRecorder()
	s.render(w, "index", "status", view)
	body := w.Body.String()
	for _, want := range []string{"Last minute", "182.0 KiB/s", "Last 5 minutes", "175.0 KiB/s", "This sync", "160.0 KiB/s",
		"50.0 MiB in 5m 20s", "Remaining", "about 56s"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	view.Copier.Rates.Running = false
	w = httptest.NewRecorder()
	s.render(w, "index", "status", view)
	if b := w.Body.String(); !strings.Contains(b, "Last sync") || strings.Contains(b, "Remaining") {
		t.Error("finished sync must say 'Last sync' and show no ETA")
	}
	view.Copier.Rates = meter.Rates{}
	w = httptest.NewRecorder()
	s.render(w, "index", "status", view)
	if !strings.Contains(w.Body.String(), "No data yet") {
		t.Error("empty state missing")
	}
}
