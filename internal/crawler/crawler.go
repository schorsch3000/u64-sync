// Package crawler walks the remote filesystem slowly and keeps the inventory current.
package crawler

import (
	"context"
	"errors"
	"log/slog"
	"path"
	"strconv"
	"sync"
	"time"

	"github.com/schorsch3000/u64-sync/internal/config"
	"github.com/schorsch3000/u64-sync/internal/device"
	"github.com/schorsch3000/u64-sync/internal/ftpx"
	"github.com/schorsch3000/u64-sync/internal/mirror"
	"github.com/schorsch3000/u64-sync/internal/store"
	"github.com/schorsch3000/u64-sync/internal/throttle"
)

type Crawler struct {
	cfg     config.Config
	st      *store.Store
	dev     *device.Monitor
	th      *throttle.Throttle
	mir     *mirror.Mirror
	Wake    chan<- struct{} // poked when new work may exist
	Trigger chan struct{}   // poke to start a crawl now

	mu       sync.Mutex
	status   Status
	approved map[string]bool // roots whose next deletion brake is overridden
}

type Status struct {
	Running   bool      `json:"running"`
	Dir       string    `json:"dir,omitempty"`
	Dirs      int       `json:"dirs"`
	Files     int       `json:"files"`
	LastStart time.Time `json:"last_start"`
	LastEnd   time.Time `json:"last_end"`
	LastOK    bool      `json:"last_ok"`
	LastError string    `json:"last_error,omitempty"`
	Brake     []string  `json:"brake,omitempty"` // roots whose deletions were refused
}

func New(cfg config.Config, st *store.Store, dev *device.Monitor, th *throttle.Throttle, mir *mirror.Mirror, wake chan<- struct{}) *Crawler {
	return &Crawler{cfg: cfg, st: st, dev: dev, th: th, mir: mir, Wake: wake, Trigger: make(chan struct{}, 1), approved: map[string]bool{}}
}

func (c *Crawler) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status
}

func (c *Crawler) update(f func(*Status)) {
	c.mu.Lock()
	f(&c.status)
	c.mu.Unlock()
}

func (c *Crawler) Run(ctx context.Context) {
	for ctx.Err() == nil {
		if c.dev.WaitUp(ctx) != nil {
			return
		}
		err := c.pass(ctx)
		wait := c.cfg.Sync.CrawlInterval
		if err != nil && ctx.Err() == nil {
			slog.Warn("crawl failed", "err", err)
			wait = time.Minute
		}
		select {
		case <-ctx.Done():
			return
		case <-c.Trigger:
		case <-time.After(wait):
		}
	}
}

func (c *Crawler) pass(ctx context.Context) error {
	gen, err := c.st.NextGen()
	if err != nil {
		return err
	}
	c.update(func(s *Status) {
		*s = Status{Running: true, LastStart: time.Now(), LastEnd: s.LastEnd, Brake: s.Brake}
	})
	cl, err := ftpx.Dial(ctx, c.cfg.Device.Host, c.cfg.Device.FTPPort, c.cfg.Device.FTPUser, c.cfg.Device.FTPPassword)
	if err != nil {
		c.finish(false, err, nil)
		return err
	}
	defer cl.Close()
	cl.Pace = c.th.Command
	stop := context.AfterFunc(ctx, cl.Close)
	defer stop()

	allOK := true
	var brake []string
	var firstErr error
	for _, root := range c.cfg.Device.Roots {
		ok, err := c.crawlRoot(ctx, cl, root, gen)
		if err != nil {
			c.finish(false, err, brake)
			return err
		}
		if !ok {
			allOK = false
			continue
		}
		if !c.applyDeletions(root, gen) {
			brake = append(brake, root)
		}
	}
	if allOK {
		c.st.SetKV("crawl_ok_gen", strconv.FormatInt(gen, 10))
		c.st.SetKV("crawl_ok_at", time.Now().UTC().Format(time.RFC3339))
	} else {
		firstErr = errors.New("some directories could not be listed")
	}
	c.purge()
	c.finish(allOK, firstErr, brake)
	return nil
}

func (c *Crawler) finish(ok bool, err error, brake []string) {
	c.update(func(s *Status) {
		s.Running, s.Dir, s.LastEnd, s.LastOK, s.Brake = false, "", time.Now(), ok, brake
		if err != nil {
			s.LastError = err.Error()
		}
	})
}

// crawlRoot lists a root breadth-first. ok=false means some directory was
// skipped, so deletions for this root must not be applied. err means the
// connection is unusable.
func (c *Crawler) crawlRoot(ctx context.Context, cl *ftpx.Client, root string, gen int64) (ok bool, err error) {
	ok = true
	queue := []string{""}
	for len(queue) > 0 {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		dir := queue[0]
		queue = queue[1:]
		c.update(func(s *Status) { s.Dir = root + "/" + dir })
		ents, lerr := cl.List(ctx, path.Join(root, dir))
		if lerr != nil {
			var re *ftpx.ReplyError
			if errors.As(lerr, &re) && dir != "" {
				slog.Warn("cannot list directory, skipping", "dir", root+"/"+dir, "err", lerr)
				ok = false
				continue
			}
			return false, lerr
		}
		var files []store.File
		for _, e := range ents {
			rel := path.Join(dir, e.Name)
			if c.excluded(rel, e.Name) {
				continue
			}
			if e.Dir {
				queue = append(queue, rel)
			} else {
				files = append(files, store.File{Root: root, Path: rel, Size: e.Size, MTime: e.ModTime.Unix()})
			}
		}
		if err := c.st.Upsert(files, gen); err != nil {
			return false, err
		}
		c.update(func(s *Status) { s.Dirs++; s.Files += len(files) })
		if len(files) > 0 {
			select {
			case c.Wake <- struct{}{}:
			default:
			}
		}
	}
	return ok, nil
}

func (c *Crawler) excluded(rel, name string) bool {
	for _, p := range c.cfg.Sync.Excludes {
		if m, _ := path.Match(p, rel); m {
			return true
		}
		if m, _ := path.Match(p, name); m {
			return true
		}
	}
	return false
}

// applyDeletions marks files missing from a complete crawl as deleted, unless
// too many vanished at once (a flaky listing must not wipe the mirror).
func (c *Crawler) applyDeletions(root string, gen int64) bool {
	missing, live, err := c.st.CountMissing(root, gen)
	if err != nil || missing == 0 {
		return err == nil
	}
	s := c.cfg.Sync
	if missing >= s.DeleteBrakeMin && missing*100 > live*s.DeleteBrakePct && !c.takeApproval(root) {
		slog.Error("deletion brake: refusing to mark files deleted", "root", root, "missing", missing, "of", live)
		return false
	}
	n, err := c.st.MarkMissingDeleted(root, gen)
	if err != nil {
		slog.Error("mark deleted", "err", err)
		return false
	}
	slog.Info("files deleted on device", "root", root, "count", n)
	return true
}

// purge removes local copies of files whose deletion grace period ended.
func (c *Crawler) purge() {
	fs, err := c.st.Purgeable(c.cfg.Sync.DeleteGrace)
	if err != nil {
		slog.Error("purge query", "err", err)
		return
	}
	for _, f := range fs {
		if err := c.mir.Remove(f.Root, f.Path); err != nil {
			slog.Error("purge remove", "file", f.Root+"/"+f.Path, "err", err)
			continue
		}
		c.st.Forget(f)
	}
}

// ApproveBrake lets the next crawl apply a refused mass deletion for root, then starts a crawl.
func (c *Crawler) ApproveBrake(root string) {
	c.mu.Lock()
	c.approved[root] = true
	c.mu.Unlock()
	select {
	case c.Trigger <- struct{}{}:
	default:
	}
}

func (c *Crawler) takeApproval(root string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	ok := c.approved[root]
	delete(c.approved, root)
	return ok
}
