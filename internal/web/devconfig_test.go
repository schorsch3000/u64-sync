package web

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/schorsch3000/u64-sync/internal/config"
	"github.com/schorsch3000/u64-sync/internal/devconfig"
	"github.com/schorsch3000/u64-sync/internal/mirror"
)

func TestDevConfigDownload(t *testing.T) {
	cur := t.TempDir()
	cfg := config.Default().DeviceConfig
	get := func(s *Server) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		s.devConfig(w, httptest.NewRequest("GET", "/devconfig", nil))
		return w
	}

	// option off, or nothing saved yet: 404
	if w := get(&Server{}); w.Code != 404 {
		t.Fatalf("no syncer: %d", w.Code)
	}
	cfg.Enabled = true
	s := &Server{DevConfig: devconfig.New(cfg, nil, &mirror.Mirror{Current: cur}, nil)}
	if w := get(s); w.Code != 404 {
		t.Fatalf("not saved yet: %d", w.Code)
	}

	os.WriteFile(filepath.Join(cur, cfg.File), []byte("[Audio Mixer]\nVol Master= 0 dB\n\n"), 0o600)
	w := get(s)
	if w.Code != 200 || !strings.Contains(w.Header().Get("Content-Disposition"), `attachment; filename="u64-config.cfg"`) {
		t.Fatalf("download: %d %v", w.Code, w.Header())
	}
	if w.Body.String() != "[Audio Mixer]\nVol Master= 0 dB\n\n" {
		t.Fatalf("body %q", w.Body.String())
	}
}
