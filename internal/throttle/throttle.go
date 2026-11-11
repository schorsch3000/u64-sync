// Package throttle paces FTP commands and bytes so the device is never flooded.
// One Throttle is shared by every worker. Settings can change at runtime.
package throttle

import (
	"context"
	"io"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

const chunk = 8192

type Throttle struct {
	mu     sync.Mutex
	last   time.Time
	delay  time.Duration
	bps    int
	bytes  *rate.Limiter // nil = unlimited
	paused bool
}

func New(bytesPerSecond int, commandDelay time.Duration) *Throttle {
	t := &Throttle{}
	t.Set(bytesPerSecond, commandDelay)
	return t
}

// Set changes the byte rate (0 = unlimited) and the gap between commands.
func (t *Throttle) Set(bytesPerSecond int, commandDelay time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.delay, t.bps = commandDelay, bytesPerSecond
	if bytesPerSecond <= 0 {
		t.bytes = nil
		return
	}
	t.bytes = rate.NewLimiter(rate.Limit(bytesPerSecond), max(chunk, bytesPerSecond/10))
}

func (t *Throttle) Settings() (bytesPerSecond int, commandDelay time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.bps, t.delay
}

// SetPaused stops all device traffic at the next command or read; a running transfer resumes later.
func (t *Throttle) SetPaused(p bool) {
	t.mu.Lock()
	t.paused = p
	t.mu.Unlock()
}

func (t *Throttle) Paused() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.paused
}

func (t *Throttle) waitResume(ctx context.Context) error {
	for t.Paused() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return ctx.Err()
}

func (t *Throttle) limiter() *rate.Limiter {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.bytes
}

// Command blocks until at least the command delay passed since the previous command.
func (t *Throttle) Command(ctx context.Context) error {
	if err := t.waitResume(ctx); err != nil {
		return err
	}
	t.mu.Lock()
	wait := time.Until(t.last.Add(t.delay))
	if wait < 0 {
		wait = 0
	}
	t.last = time.Now().Add(wait) // reserve the slot
	t.mu.Unlock()
	if wait == 0 {
		return ctx.Err()
	}
	select {
	case <-time.After(wait):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Reader wraps r so reads honor pause and the byte rate.
func (t *Throttle) Reader(ctx context.Context, r io.Reader) io.Reader {
	return &reader{ctx: ctx, r: r, t: t}
}

type reader struct {
	ctx context.Context
	r   io.Reader
	t   *Throttle
}

func (r *reader) Read(p []byte) (int, error) {
	if err := r.t.waitResume(r.ctx); err != nil {
		return 0, err
	}
	if len(p) > chunk {
		p = p[:chunk]
	}
	n, err := r.r.Read(p)
	if l := r.t.limiter(); l != nil && n > 0 {
		if werr := l.WaitN(r.ctx, n); werr != nil {
			return n, werr
		}
	}
	return n, err
}
