// Package copier downloads new and changed files from the device into the local mirror.
package copier

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/schorsch3000/u64-sync/internal/config"
	"github.com/schorsch3000/u64-sync/internal/device"
	"github.com/schorsch3000/u64-sync/internal/ftpx"
	"github.com/schorsch3000/u64-sync/internal/meter"
	"github.com/schorsch3000/u64-sync/internal/mirror"
	"github.com/schorsch3000/u64-sync/internal/store"
	"github.com/schorsch3000/u64-sync/internal/throttle"
)

type Copier struct {
	cfg  config.Config
	st   *store.Store
	dev  *device.Monitor
	th   *throttle.Throttle
	mir  *mirror.Mirror
	Wake chan struct{}

	meter  meter.Meter
	mu     sync.Mutex
	status Status
}

type Status struct {
	Current   string      `json:"current,omitempty"`
	Done      int64       `json:"done_bytes"`
	Total     int64       `json:"total_bytes"`
	LastOK    time.Time   `json:"last_ok"`
	LastError string      `json:"last_error,omitempty"`
	Copied    int         `json:"copied"` // since start
	Rates     meter.Rates `json:"rates"`
}

func New(cfg config.Config, st *store.Store, dev *device.Monitor, th *throttle.Throttle, mir *mirror.Mirror) *Copier {
	return &Copier{cfg: cfg, st: st, dev: dev, th: th, mir: mir, Wake: make(chan struct{}, 1)}
}

func (w *Copier) Status() Status {
	w.mu.Lock()
	s := w.status
	w.mu.Unlock()
	s.Rates = w.meter.Rates()
	return s
}

func (w *Copier) update(f func(*Status)) {
	w.mu.Lock()
	f(&w.status)
	w.mu.Unlock()
}

func (w *Copier) sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-w.Wake:
	case <-time.After(d):
	}
}

func (w *Copier) Run(ctx context.Context) {
	var cl *ftpx.Client
	closeClient := func() {
		if cl != nil {
			cl.Close()
			cl = nil
		}
	}
	defer closeClient()
	for ctx.Err() == nil {
		if w.dev.WaitUp(ctx) != nil {
			return
		}
		f, ok, err := w.st.NextPending()
		if err != nil {
			slog.Error("next pending", "err", err)
			w.sleep(ctx, 30*time.Second)
			continue
		}
		if !ok {
			closeClient() // free the device's connection slot while idle
			w.meter.Idle()
			w.markComplete()
			w.sleep(ctx, 15*time.Second)
			continue
		}
		if cl == nil {
			cl, err = ftpx.Dial(ctx, w.cfg.Device.Host, w.cfg.Device.FTPPort, w.cfg.Device.FTPUser, w.cfg.Device.FTPPassword)
			if err != nil {
				w.update(func(s *Status) { s.LastError = err.Error() })
				cl = nil
				w.sleep(ctx, 15*time.Second)
				continue
			}
			cl.Pace = w.th.Command
			c := cl
			context.AfterFunc(ctx, c.Close)
		}
		err = w.copyOne(ctx, cl, f)
		w.update(func(s *Status) { s.Current, s.Done, s.Total = "", 0, 0 })
		var re *ftpx.ReplyError
		switch {
		case err == nil:
			w.update(func(s *Status) { s.LastOK, s.LastError = time.Now(), ""; s.Copied++ })
		case errors.Is(err, errChanged):
			// metadata refreshed, file is queued again; give it time to settle
			w.sleep(ctx, 5*time.Second)
		case ctx.Err() != nil:
			return
		default:
			slog.Warn("copy failed", "file", f.Root+"/"+f.Path, "err", err)
			w.update(func(s *Status) { s.LastError = f.Root + "/" + f.Path + ": " + err.Error() })
			w.st.Fail(f, err)
			if !errors.As(err, &re) { // connection problem: redial
				closeClient()
				w.sleep(ctx, 10*time.Second)
			}
		}
	}
}

// markComplete notes that the mirror equals the device as of the last fully
// successful crawl. Snapshots hook in here.
func (w *Copier) markComplete() {
	gen := w.st.GetKV("crawl_ok_gen")
	if gen == "" || gen == w.st.GetKV("complete_gen") {
		return
	}
	c, err := w.st.Counts()
	if err != nil || c.Pending != 0 {
		return
	}
	w.st.SetKV("complete_gen", gen)
	w.st.SetKV("complete_at", time.Now().UTC().Format(time.RFC3339))
	slog.Info("mirror complete", "gen", gen, "files", c.Files)
}

var errChanged = errors.New("file changed during copy")

func (w *Copier) copyOne(ctx context.Context, cl *ftpx.Client, f store.File) error {
	target, err := w.mir.Path(f.Root, f.Path)
	if err != nil {
		return err
	}
	remote := f.Root + "/" + f.Path
	w.update(func(s *Status) { s.Current, s.Done, s.Total = remote, 0, f.Size })

	tmp, err := w.mir.NewTemp()
	if err != nil {
		return err
	}
	h := sha256.New()
	var n int64
	err = cl.Retrieve(ctx, remote, func(r io.Reader) error {
		pr := &progress{r: w.th.Reader(ctx, r), w: w}
		var cerr error
		n, cerr = io.Copy(io.MultiWriter(tmp, h), pr)
		return cerr
	})
	if err != nil {
		tmp.Close()
		removeFile(tmp.Name())
		return err
	}
	// The file may have changed while we copied; compare against fresh metadata.
	cur, serr := cl.Stat(ctx, remote)
	if serr != nil {
		var re *ftpx.ReplyError
		if !errors.As(serr, &re) {
			tmp.Close()
			removeFile(tmp.Name())
			return serr
		}
		cur = ftpx.Entry{Size: n, ModTime: time.Unix(f.MTime, 0).UTC()} // MLST unsupported for this path: trust listing
	}
	if n != f.Size || cur.Size != f.Size || cur.ModTime.Unix() != f.MTime {
		tmp.Close()
		removeFile(tmp.Name())
		// Record what the device has now, so the retry targets the current version
		// instead of downloading again against stale metadata until the next crawl.
		f.Size, f.MTime = cur.Size, cur.ModTime.Unix()
		if err := w.st.UpdateRemote(f); err != nil {
			return err
		}
		return fmt.Errorf("%w: %s", errChanged, remote)
	}
	if err := w.mir.Install(tmp, target, time.Unix(f.MTime, 0)); err != nil {
		return err
	}
	return w.st.MarkBackedUp(f, hex.EncodeToString(h.Sum(nil)))
}

type progress struct {
	r io.Reader
	w *Copier
}

func (p *progress) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.w.update(func(s *Status) { s.Done += int64(n) })
		p.w.meter.Add(n)
	}
	return n, err
}

func removeFile(name string) { _ = os.Remove(name) }
