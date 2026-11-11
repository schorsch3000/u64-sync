package devconfig

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/schorsch3000/u64-sync/internal/config"
	"github.com/schorsch3000/u64-sync/internal/device"
	"github.com/schorsch3000/u64-sync/internal/mirror"
	"github.com/schorsch3000/u64-sync/internal/throttle"
)

// answers imitates the Ultimate64's REST API (including its odd spacing and trailing "errors").
type answers struct {
	cats      []string
	items     map[string]string // category -> settings object, in device order
	broken    atomic.Value      // category name that answers with HTTP 500
	listError bool
}

func (a *answers) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/v1/info":
		w.Write([]byte(`{ "product" : "Ultimate 64-II", "firmware_version" : "3.15a", "errors" : [  ] }`))
	case r.URL.Path == "/v1/configs":
		if a.listError {
			w.Write([]byte(`{ "errors" : [ "no access" ] }`))
			return
		}
		cats, _ := json.Marshal(a.cats)
		w.Write([]byte(`{ "categories" : ` + string(cats) + `, "errors" : [  ] }`))
	case strings.HasPrefix(r.URL.Path, "/v1/configs/"):
		// like the real device: a raw ':' in the path is a 404, it must be sent as %3A
		if strings.Contains(r.URL.EscapedPath(), ":") {
			http.NotFound(w, r)
			return
		}
		cat := strings.TrimPrefix(r.URL.Path, "/v1/configs/")
		if b, _ := a.broken.Load().(string); b == cat {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		in, ok := a.items[cat]
		if !ok {
			w.Write([]byte(`{ "errors" : [ "unknown category" ] }`))
			return
		}
		name, _ := json.Marshal(cat)
		w.Write([]byte(`{ ` + string(name) + ` : ` + in + `, "errors" : [  ] }`))
	default:
		http.NotFound(w, r)
	}
}

func setup(t *testing.T, a *answers) (*Syncer, *mirror.Mirror) {
	t.Helper()
	srv := httptest.NewServer(a)
	t.Cleanup(srv.Close)
	d := t.TempDir()
	mir := &mirror.Mirror{Current: filepath.Join(d, "cur"), Tmp: filepath.Join(d, "tmp")}
	os.MkdirAll(mir.Current, 0o755)
	os.MkdirAll(mir.Tmp, 0o755)
	dev := device.New("127.0.0.1", srv.Listener.Addr().(*net.TCPAddr).Port, "", 2*time.Second)
	cfg := config.Default().DeviceConfig
	cfg.Enabled = true
	return New(cfg, dev, mir, throttle.New(0, 0)), mir
}

func newAnswers() *answers {
	return &answers{
		cats: []string{"Network Settings", "SID Socket 1: PDsid", "Audio Mixer"},
		items: map[string]string{
			"Network Settings":    `{ "Host Name" : "u64", "Network Password" : "s3cret", "Time Server 1" : "time.windows.com" }`,
			"SID Socket 1: PDsid": `{ "Volume" : "+1 dB" }`,
			"Audio Mixer":         `{ "Vol UltiSid 1" : "0 dB", "Pan UltiSid 1" : "Center" }`,
		},
	}
}

func TestFetchBuildsCfg(t *testing.T) {
	a := newAnswers()
	a.items["Audio Mixer"] = `{ "Vol UltiSid 1" : " 0 dB", "Mouse Sensitivity" : 8, "Pan UltiSid 1" : "Center" }`
	s, _ := setup(t, a)
	data, n, err := s.Fetch(context.Background())
	if err != nil || n != 3 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	want := "[Network Settings]\nHost Name=u64\nNetwork Password=s3cret\nTime Server 1=time.windows.com\n\n" +
		"[SID Socket 1: PDsid]\nVolume=+1 dB\n\n" +
		"[Audio Mixer]\nVol UltiSid 1= 0 dB\nMouse Sensitivity=8\nPan UltiSid 1=Center\n\n"
	if string(data) != want {
		t.Fatalf("got:\n%q\nwant:\n%q", data, want)
	}
	again, _, _ := s.Fetch(context.Background())
	if string(again) != string(data) {
		t.Fatal("output is not deterministic")
	}
}

func TestWriteOnlyWhenChangedAndOwnerOnly(t *testing.T) {
	a := newAnswers()
	s, mir := setup(t, a)
	target := filepath.Join(mir.Current, "u64-config.cfg")

	changed, n, err := s.Sync(context.Background())
	if err != nil || !changed || n != 3 {
		t.Fatalf("first sync: %v %v %v", changed, n, err)
	}
	st1, _ := os.Stat(target)
	if runtime.GOOS != "windows" && st1.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v: the file can hold passwords", st1.Mode().Perm())
	}
	if b, _ := os.ReadFile(target); !strings.Contains(string(b), "Host Name=u64") {
		t.Fatalf("content:\n%s", b)
	}
	if ents, _ := os.ReadDir(mir.Tmp); len(ents) != 0 {
		t.Fatal("temp file left behind")
	}

	// unchanged: not rewritten, same inode, so snapshots keep sharing it
	changed, _, err = s.Sync(context.Background())
	if err != nil || changed {
		t.Fatalf("unchanged config rewritten: %v %v", changed, err)
	}
	st2, _ := os.Stat(target)
	if !os.SameFile(st1, st2) {
		t.Fatal("inode replaced although nothing changed")
	}

	// a snapshot (hardlink) must keep the old version when the config changes
	link := filepath.Join(t.TempDir(), "snap.cfg")
	if err := os.Link(target, link); err != nil {
		t.Skipf("no hardlinks here: %v", err)
	}
	a.items["Network Settings"] = `{ "Host Name" : "renamed" }`
	changed, _, err = s.Sync(context.Background())
	if err != nil || !changed {
		t.Fatalf("changed config not written: %v %v", changed, err)
	}
	if b, _ := os.ReadFile(link); !strings.Contains(string(b), "Host Name=u64") {
		t.Fatal("snapshot copy was modified in place")
	}
	if b, _ := os.ReadFile(target); !strings.Contains(string(b), "Host Name=renamed") {
		t.Fatal("new config not in place")
	}
}

// A failing category must not leave a half-written or truncated file behind.
func TestFailureKeepsOldFile(t *testing.T) {
	a := newAnswers()
	s, mir := setup(t, a)
	if _, _, err := s.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(mir.Current, "u64-config.cfg")
	before, _ := os.ReadFile(target)

	a.broken.Store("Audio Mixer")
	if _, _, err := s.Sync(context.Background()); err == nil || !strings.Contains(err.Error(), "Audio Mixer") {
		t.Fatalf("expected an error naming the category, got %v", err)
	}
	a.broken.Store("")
	delete(a.items, "Audio Mixer") // device now answers "unknown category" for it
	if _, _, err := s.Sync(context.Background()); err == nil {
		t.Fatal("device error in a category must fail the sync")
	}
	a.listError = true
	if _, _, err := s.Sync(context.Background()); err == nil {
		t.Fatal("device error in the list must fail the sync")
	}
	if after, _ := os.ReadFile(target); string(after) != string(before) {
		t.Fatal("old file damaged by failed syncs")
	}
}

func TestDeviceDownIsAnError(t *testing.T) {
	s, _ := setup(t, newAnswers())
	s.dev = device.New("127.0.0.1", 1, "", 500*time.Millisecond)
	if _, _, err := s.Sync(context.Background()); err == nil {
		t.Fatal("expected an error")
	}
}

func TestConfigValidation(t *testing.T) {
	check := func(mod func(*config.Config)) error {
		c := config.Default()
		c.Device.Host = "x"
		c.DeviceConfig.Enabled = true
		mod(&c)
		_, err := config.Validate(c)
		return err
	}
	if err := check(func(c *config.Config) {}); err != nil {
		t.Fatalf("defaults must be valid: %v", err)
	}
	for name, mod := range map[string]func(*config.Config){
		"path in name":   func(c *config.Config) { c.DeviceConfig.File = "sub/x.json" },
		"parent":         func(c *config.Config) { c.DeviceConfig.File = ".." },
		"empty":          func(c *config.Config) { c.DeviceConfig.File = "" },
		"windows stream": func(c *config.Config) { c.DeviceConfig.File = "x:y" },
		"root collision": func(c *config.Config) { c.DeviceConfig.File = "sd" },
		"too frequent":   func(c *config.Config) { c.DeviceConfig.Interval = time.Second },
	} {
		if err := check(mod); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	// disabled: nothing about it is checked
	c := config.Default()
	c.Device.Host = "x"
	c.DeviceConfig.File = "../bad"
	if _, err := config.Validate(c); err != nil {
		t.Fatalf("disabled option validated: %v", err)
	}
}

func TestEscapeSegment(t *testing.T) {
	for in, want := range map[string]string{
		"Audio Mixer":         "Audio%20Mixer",
		"SID Socket 1: PDsid": "SID%20Socket%201%3A%20PDsid",
		"WiFi settings":       "WiFi%20settings",
		"a/b?c#d%e":           "a%2Fb%3Fc%23d%25e",
		"plain-._~":           "plain-._~",
		"Ünï":                 "%C3%9Cn%C3%AF",
	} {
		if got := escapeSegment(in); got != want {
			t.Errorf("escapeSegment(%q) = %q, want %q", in, got, want)
		}
	}
}
