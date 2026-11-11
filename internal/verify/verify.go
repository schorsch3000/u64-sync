// Package verify slowly re-hashes the local mirror against the sha256 recorded
// at download time, so bit rot or a deleted file on the backup disk is found
// and repaired by downloading the file again. It never touches the device.
package verify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/schorsch3000/u64-sync/internal/config"
	"github.com/schorsch3000/u64-sync/internal/mirror"
	"github.com/schorsch3000/u64-sync/internal/store"
	"golang.org/x/time/rate"
)

type Verifier struct {
	cfg  config.Verify
	st   *store.Store
	mir  *mirror.Mirror
	wake chan<- struct{} // tells the copier new work exists

	// Poll is how long to wait when no file is due. Default 10 minutes.
	Poll time.Duration

	mu     sync.Mutex
	status Status
}

type Status struct {
	Enabled  bool      `json:"enabled"`
	Current  string    `json:"current,omitempty"`
	Checked  int       `json:"checked"` // since start
	Repaired int       `json:"repaired"`
	LastBad  string    `json:"last_bad,omitempty"`
	LastTime time.Time `json:"last_bad_time"`
	Verified int       `json:"verified"` // files checked within the interval
	Stale    int       `json:"stale"`    // files due for a check
}

func New(cfg config.Verify, st *store.Store, mir *mirror.Mirror, wake chan<- struct{}) *Verifier {
	return &Verifier{cfg: cfg, st: st, mir: mir, wake: wake, Poll: 10 * time.Minute}
}

func (v *Verifier) Status() Status {
	v.mu.Lock()
	s := v.status
	v.mu.Unlock()
	s.Enabled = v.cfg.Enabled
	if v.cfg.Enabled {
		if vs, err := v.st.VerifyStats(time.Now().Add(-v.cfg.Interval)); err == nil {
			s.Verified, s.Stale = vs.Verified, vs.Stale
		}
	}
	return s
}

func (v *Verifier) update(f func(*Status)) {
	v.mu.Lock()
	f(&v.status)
	v.mu.Unlock()
}

func (v *Verifier) Run(ctx context.Context) {
	if !v.cfg.Enabled {
		return
	}
	var lim *rate.Limiter
	if v.cfg.BytesPerSecond > 0 {
		lim = rate.NewLimiter(rate.Limit(v.cfg.BytesPerSecond), max(64<<10, v.cfg.BytesPerSecond/10))
	}
	for ctx.Err() == nil {
		n := v.pass(ctx, lim)
		if n == 0 { // nothing due: look again later
			select {
			case <-ctx.Done():
			case <-time.After(v.Poll):
			}
		}
	}
}

// pass checks one batch of due files and returns how many it looked at.
func (v *Verifier) pass(ctx context.Context, lim *rate.Limiter) int {
	batch, err := v.st.VerifyBatch(200, time.Now().Add(-v.cfg.Interval))
	if err != nil {
		slog.Error("verify batch", "err", err)
		return 0
	}
	for _, b := range batch {
		if ctx.Err() != nil {
			break
		}
		v.check(ctx, lim, b)
	}
	v.update(func(s *Status) { s.Current = "" })
	return len(batch)
}

func (v *Verifier) check(ctx context.Context, lim *rate.Limiter, b store.Backup) {
	name := b.Root + "/" + b.Path
	v.update(func(s *Status) { s.Current = name })
	reason := ""
	// A mismatch is confirmed a second time: the copier may have replaced the
	// file between our DB read and our file read.
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
		}
		sum, size, err := v.hash(ctx, lim, b.File)
		if ctx.Err() != nil {
			return
		}
		if now := v.st.BackupSHA(b.File); now != b.SHA256 {
			// File was re-downloaded (or flagged) meanwhile; its new state is fresh.
			if now != "" {
				b.SHA256 = now
				continue
			}
			return
		}
		switch {
		case errors.Is(err, os.ErrNotExist):
			reason = "local copy missing"
		case err != nil:
			slog.Warn("verify read error", "file", name, "err", err)
			return // transient I/O problem: try again next pass, do not touch state
		case size != b.BSize:
			reason = "local copy has wrong size"
		case sum != b.SHA256:
			reason = "local copy corrupt (sha256 mismatch)"
		default:
			v.st.MarkVerified(b.File)
			v.update(func(s *Status) { s.Checked++ })
			return
		}
	}
	slog.Error("verify failed, scheduling re-download", "file", name, "reason", reason)
	if err := v.st.FlagCorrupt(b.File, reason); err != nil {
		slog.Error("verify flag", "err", err)
		return
	}
	v.update(func(s *Status) {
		s.Checked++
		s.Repaired++
		s.LastBad, s.LastTime = name+": "+reason, time.Now()
	})
	select {
	case v.wake <- struct{}{}:
	default:
	}
}

func (v *Verifier) hash(ctx context.Context, lim *rate.Limiter, f store.File) (string, int64, error) {
	p, err := v.mir.Path(f.Root, f.Path)
	if err != nil {
		return "", 0, err
	}
	fh, err := mirror.OpenShared(p)
	if err != nil {
		return "", 0, err
	}
	defer fh.Close()
	h := sha256.New()
	buf := make([]byte, 64<<10)
	var n int64
	for {
		m, rerr := fh.Read(buf)
		if m > 0 {
			h.Write(buf[:m])
			n += int64(m)
			if lim != nil {
				if err := lim.WaitN(ctx, m); err != nil {
					return "", 0, err
				}
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return "", 0, rerr
		}
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}
