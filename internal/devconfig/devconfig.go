// Package devconfig saves the device's own configuration, read from its REST
// API, as one JSON file in the root of the local mirror. Snapshots pick the
// file up like any other, so the configuration is versioned with the files.
package devconfig

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/schorsch3000/u64-sync/internal/config"
	"github.com/schorsch3000/u64-sync/internal/device"
	"github.com/schorsch3000/u64-sync/internal/mirror"
	"github.com/schorsch3000/u64-sync/internal/throttle"
)

type Syncer struct {
	cfg config.DeviceConfig
	dev *device.Monitor
	mir *mirror.Mirror
	th  *throttle.Throttle

	mu     sync.Mutex
	status Status
}

type Status struct {
	Enabled    bool      `json:"enabled"`
	File       string    `json:"file,omitempty"`
	LastOK     time.Time `json:"last_ok"`
	LastChange time.Time `json:"last_change"` // when the file content last changed
	LastError  string    `json:"last_error,omitempty"`
	Categories int       `json:"categories"`
}

func New(cfg config.DeviceConfig, dev *device.Monitor, mir *mirror.Mirror, th *throttle.Throttle) *Syncer {
	return &Syncer{cfg: cfg, dev: dev, mir: mir, th: th, status: Status{Enabled: cfg.Enabled, File: cfg.File}}
}

func (s *Syncer) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

func (s *Syncer) update(f func(*Status)) {
	s.mu.Lock()
	f(&s.status)
	s.mu.Unlock()
}

func (s *Syncer) Run(ctx context.Context) {
	if !s.cfg.Enabled {
		return
	}
	for ctx.Err() == nil {
		if s.dev.WaitUp(ctx) != nil {
			return
		}
		wait := s.cfg.Interval
		changed, n, err := s.Sync(ctx)
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			slog.Warn("device config", "err", err)
			s.update(func(st *Status) { st.LastError = err.Error() })
			wait = 5 * time.Minute
		default:
			s.update(func(st *Status) {
				st.LastOK, st.LastError, st.Categories = time.Now(), "", n
				if changed {
					st.LastChange = time.Now()
				}
			})
			if changed {
				slog.Info("device configuration saved", "file", s.cfg.File, "categories", n)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// Sync reads the configuration and writes the file if its content changed.
func (s *Syncer) Sync(ctx context.Context) (changed bool, categories int, err error) {
	data, n, err := s.Fetch(ctx)
	if err != nil {
		return false, 0, err
	}
	changed, err = s.write(data)
	return changed, n, err
}

type listing struct {
	Categories []string `json:"categories"`
	Errors     []string `json:"errors"`
}

// Fetch builds the JSON document: device info plus every configuration category,
// in the device's own order. It contains no timestamp, so an unchanged
// configuration yields byte-identical output.
func (s *Syncer) Fetch(ctx context.Context) ([]byte, int, error) {
	get := func(path string) ([]byte, error) {
		if err := s.th.Command(ctx); err != nil {
			return nil, err
		}
		return s.dev.GetJSON(ctx, path)
	}
	b, err := get("/v1/configs")
	if err != nil {
		return nil, 0, err
	}
	var l listing
	if err := json.Unmarshal(b, &l); err != nil {
		return nil, 0, fmt.Errorf("config list: %w", err)
	}
	if len(l.Errors) > 0 {
		return nil, 0, fmt.Errorf("config list: device reports %v", l.Errors)
	}
	if len(l.Categories) == 0 {
		return nil, 0, errors.New("config list: device returned no categories")
	}

	var out bytes.Buffer
	out.WriteString(`{"device":`)
	if info, err := get("/v1/info"); err == nil && json.Valid(info) {
		out.Write(stripErrors(info))
	} else {
		out.WriteString("null")
	}
	out.WriteString(`,"categories":{`)
	for i, cat := range l.Categories {
		raw, err := get("/v1/configs/" + escapeSegment(cat))
		if err != nil {
			return nil, 0, fmt.Errorf("category %q: %w", cat, err)
		}
		inner, err := category(raw, cat)
		if err != nil {
			return nil, 0, fmt.Errorf("category %q: %w", cat, err)
		}
		if i > 0 {
			out.WriteByte(',')
		}
		name, _ := json.Marshal(cat)
		out.Write(name)
		out.WriteByte(':')
		out.Write(inner)
	}
	out.WriteString("}}")

	var pretty bytes.Buffer
	if err := json.Indent(&pretty, out.Bytes(), "", "  "); err != nil {
		return nil, 0, fmt.Errorf("assembling config document: %w", err)
	}
	pretty.WriteByte('\n')
	return pretty.Bytes(), len(l.Categories), nil
}

// escapeSegment percent-encodes everything but unreserved characters. The device
// answers 404 for a category like "SID Socket 1: PDsid" when the colon stays raw
// (which url.PathEscape allows), so be strict.
func escapeSegment(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '.' || c == '_' || c == '~' {
			b.WriteByte(c)
		} else {
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&15])
		}
	}
	return b.String()
}

// category extracts the settings object of one category from the device's answer
// {"<category>": {...}, "errors": []}, keeping the device's item order.
func category(raw []byte, name string) (json.RawMessage, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	var errs []string
	json.Unmarshal(m["errors"], &errs)
	if len(errs) > 0 {
		return nil, fmt.Errorf("device reports %v", errs)
	}
	inner, ok := m[name]
	if !ok {
		return nil, errors.New("not in the answer")
	}
	return inner, nil
}

// stripErrors drops the trailing "errors" member the REST API adds to every answer.
func stripErrors(raw []byte) []byte {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return raw
	}
	delete(m, "errors")
	// re-marshalling sorts keys, which is fine: stable output is what matters here
	b, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return b
}

// write replaces the file atomically, only if the content differs. An unchanged
// file keeps its inode, so snapshots keep sharing it.
func (s *Syncer) write(data []byte) (bool, error) {
	target := filepath.Join(s.mir.Current, s.cfg.File)
	if old, err := os.ReadFile(target); err == nil && bytes.Equal(old, data) {
		return false, nil
	}
	if err := os.MkdirAll(s.mir.Tmp, 0o755); err != nil {
		return false, err
	}
	tmp, err := s.mir.NewTemp()
	if err != nil {
		return false, err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return false, err
	}
	// owner-only from the start: the document can hold network and WiFi passwords
	if err := s.mir.InstallMode(tmp, target, time.Now(), 0o600); err != nil {
		return false, err
	}
	return true, nil
}
