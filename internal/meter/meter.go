// Package meter measures download throughput: average over the last minute,
// the last five minutes, and over the current sync (a continuous busy period).
package meter

import (
	"sync"
	"time"
)

const window = 300 // seconds of history kept

type Meter struct {
	mu    sync.Mutex
	secs  [window]int64 // unix second each bucket belongs to
	bytes [window]int64

	start time.Time // current (or last) busy period
	end   time.Time // zero while running
	total int64     // bytes in that busy period
}

type Rates struct {
	Last1m       float64       `json:"last_1m_bps"`
	Last5m       float64       `json:"last_5m_bps"`
	Sync         float64       `json:"sync_bps"` // average over the busy period
	SyncBytes    int64         `json:"sync_bytes"`
	SyncDuration time.Duration `json:"sync_duration"`
	Running      bool          `json:"running"` // a busy period is in progress
}

// Add records n downloaded bytes now.
func (m *Meter) Add(n int) { m.AddAt(n, time.Now()) }

func (m *Meter) AddAt(n int, now time.Time) {
	if n <= 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.start.IsZero() || !m.end.IsZero() { // first bytes after idle: new busy period
		m.start, m.end, m.total = now, time.Time{}, 0
	}
	m.total += int64(n)
	s := now.Unix()
	i := s % window
	if m.secs[i] != s {
		m.secs[i], m.bytes[i] = s, 0
	}
	m.bytes[i] += int64(n)
}

// Idle ends the current busy period (the copier ran out of work). Its average stays visible.
func (m *Meter) Idle() { m.IdleAt(time.Now()) }

func (m *Meter) IdleAt(now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.start.IsZero() && m.end.IsZero() {
		m.end = now
	}
}

func (m *Meter) Rates() Rates { return m.RatesAt(time.Now()) }

func (m *Meter) RatesAt(now time.Time) Rates {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := Rates{SyncBytes: m.total, Running: !m.start.IsZero() && m.end.IsZero()}
	if !m.start.IsZero() {
		end := m.end
		if end.IsZero() {
			end = now
		}
		r.SyncDuration = end.Sub(m.start)
		if r.SyncDuration > 0 {
			r.Sync = float64(m.total) / r.SyncDuration.Seconds()
		}
	}
	r.Last1m = m.avg(now, 60, r.Running)
	r.Last5m = m.avg(now, 300, r.Running)
	return r
}

// avg is the byte rate over the last w seconds. While a busy period is younger
// than w, it is divided by the period's age, so a fresh sync is not understated.
func (m *Meter) avg(now time.Time, w int64, running bool) float64 {
	cur := now.Unix()
	var sum int64
	for i := 0; i < window; i++ {
		if s := m.secs[i]; s > cur-w && s <= cur {
			sum += m.bytes[i]
		}
	}
	d := float64(w)
	if running {
		if age := now.Sub(m.start).Seconds(); age < d {
			d = max(age, 1)
		}
	}
	return float64(sum) / d
}
