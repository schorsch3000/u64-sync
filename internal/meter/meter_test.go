package meter

import (
	"math"
	"testing"
	"time"
)

var t0 = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func near(t *testing.T, name string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > want*0.01+0.001 {
		t.Errorf("%s = %.2f, want %.2f", name, got, want)
	}
}

func TestSteadyRate(t *testing.T) {
	var m Meter
	for s := 0; s < 600; s++ { // 10 minutes at 1000 B/s
		m.AddAt(1000, t0.Add(time.Duration(s)*time.Second))
	}
	r := m.RatesAt(t0.Add(599 * time.Second))
	near(t, "1m", r.Last1m, 1000)
	near(t, "5m", r.Last5m, 1000)
	near(t, "sync", r.Sync, 600000.0/599)
	if !r.Running || r.SyncBytes != 600000 {
		t.Fatalf("%+v", r)
	}
}

func TestYoungSyncIsNotUnderstated(t *testing.T) {
	var m Meter
	for s := 0; s < 10; s++ {
		m.AddAt(2000, t0.Add(time.Duration(s)*time.Second))
	}
	r := m.RatesAt(t0.Add(10 * time.Second))
	near(t, "1m over 10s of activity", r.Last1m, 2000)
	near(t, "5m over 10s of activity", r.Last5m, 2000)
}

func TestRateDecaysAfterIdleAndSyncAverageIsKept(t *testing.T) {
	var m Meter
	for s := 0; s < 60; s++ {
		m.AddAt(1000, t0.Add(time.Duration(s)*time.Second))
	}
	m.IdleAt(t0.Add(60 * time.Second))
	r := m.RatesAt(t0.Add(90 * time.Second))
	if r.Running {
		t.Fatal("still running after Idle")
	}
	near(t, "1m 30s after the end", r.Last1m, 29*1000/60.0) // window is seconds 31..90; traffic ended at second 59
	near(t, "sync average frozen", r.Sync, 1000)
	r = m.RatesAt(t0.Add(10 * time.Minute))
	if r.Last1m != 0 || r.Last5m != 0 {
		t.Fatalf("old traffic still counted: %+v", r)
	}
	near(t, "sync average still frozen", r.Sync, 1000)
}

func TestNewBusyPeriodResetsSync(t *testing.T) {
	var m Meter
	m.AddAt(1000, t0)
	m.IdleAt(t0.Add(time.Second))
	m.AddAt(500, t0.Add(time.Hour))
	r := m.RatesAt(t0.Add(time.Hour + 2*time.Second))
	if r.SyncBytes != 500 || !r.Running {
		t.Fatalf("%+v", r)
	}
}

func TestEmptyAndOldBuckets(t *testing.T) {
	var m Meter
	if r := m.Rates(); r.Last1m != 0 || r.Sync != 0 || r.Running {
		t.Fatalf("%+v", r)
	}
	m.AddAt(100, t0)
	m.AddAt(0, t0)
	m.AddAt(-5, t0)
	// a bucket reused after 5 minutes must not keep old bytes
	m.AddAt(7, t0.Add(300*time.Second))
	r := m.RatesAt(t0.Add(300 * time.Second))
	near(t, "5m", r.Last5m, 7.0/300*1) // only the new byte count within the window (period age 300s)
}
