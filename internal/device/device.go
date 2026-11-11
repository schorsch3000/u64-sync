// Package device watches whether the Ultimate64 is reachable, via its REST API.
package device

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"
)

type Monitor struct {
	host, password string
	client         *http.Client

	mu      sync.Mutex
	up      bool
	since   time.Time
	checked time.Time
	info    map[string]any
}

func New(host, restPassword string, timeout time.Duration) *Monitor {
	return &Monitor{host: host, password: restPassword, client: &http.Client{Timeout: timeout}}
}

type State struct {
	Up      bool           `json:"up"`
	Since   time.Time      `json:"since"`
	Checked time.Time      `json:"checked"`
	Info    map[string]any `json:"info,omitempty"`
}

func (m *Monitor) State() State {
	m.mu.Lock()
	defer m.mu.Unlock()
	return State{m.up, m.since, m.checked, m.info}
}

func (m *Monitor) Up() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.up
}

// Run polls until ctx ends.
func (m *Monitor) Run(ctx context.Context, interval time.Duration) {
	for {
		m.check(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

// WaitUp blocks until the device is reachable or ctx ends.
func (m *Monitor) WaitUp(ctx context.Context) error {
	for !m.Up() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return ctx.Err()
}

func (m *Monitor) get(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", "http://"+m.host+path, nil)
	if err != nil {
		return nil, err
	}
	if m.password != "" {
		req.Header.Set("X-Password", m.password)
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

func (m *Monitor) check(ctx context.Context) {
	_, err := m.get(ctx, "/v1/version")
	up := err == nil
	var info map[string]any
	m.mu.Lock()
	wasUp := m.up
	m.mu.Unlock()
	if up && !wasUp {
		if b, err := m.get(ctx, "/v1/info"); err == nil {
			json.Unmarshal(b, &info)
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.checked = time.Now()
	if up != m.up {
		m.up, m.since = up, time.Now()
	}
	if info != nil {
		m.info = info
	}
}
