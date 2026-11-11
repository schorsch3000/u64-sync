// u64-sync keeps a slow, throttled mirror of an Ultimate64's SD card.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/schorsch3000/u64-sync/internal/config"
	"github.com/schorsch3000/u64-sync/internal/copier"
	"github.com/schorsch3000/u64-sync/internal/crawler"
	"github.com/schorsch3000/u64-sync/internal/device"
	"github.com/schorsch3000/u64-sync/internal/mirror"
	"github.com/schorsch3000/u64-sync/internal/snapshot"
	"github.com/schorsch3000/u64-sync/internal/store"
	"github.com/schorsch3000/u64-sync/internal/throttle"
	"github.com/schorsch3000/u64-sync/internal/verify"
	"github.com/schorsch3000/u64-sync/internal/web"
)

// version is set at build time: -ldflags "-X main.version=v0.1.0".
var version = "dev"

func main() {
	cfgFile := flag.String("config", "", "config file (YAML); defaults plus U64_HOST env if empty")
	health := flag.Bool("healthcheck", false, "probe the running service's /healthz and exit (for container health checks)")
	flag.Parse()
	if *health {
		os.Exit(healthcheck(*cfgFile))
	}
	if err := run(*cfgFile); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

// healthcheck needs no shell or curl, so it works in a FROM scratch image.
func healthcheck(cfgFile string) int {
	cfg, err := config.Load(cfgFile)
	if err != nil {
		return 1
	}
	_, port, err := net.SplitHostPort(cfg.Web.Listen)
	if err != nil {
		return 1
	}
	c := http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://" + net.JoinHostPort("127.0.0.1", port) + "/healthz")
	if err != nil {
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

func run(cfgFile string) error {
	cfg, err := config.Load(cfgFile)
	if err != nil {
		return err
	}
	for _, d := range []string{cfg.Storage.DataDir, cfg.Storage.CurrentDir, cfg.Storage.TmpDir, cfg.Storage.SnapshotDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	dirs := []string{cfg.Storage.CurrentDir, cfg.Storage.TmpDir}
	if cfg.Snapshots.Enabled {
		dirs = append(dirs, cfg.Storage.SnapshotDir)
	}
	if err := mirror.CheckSameFS(dirs...); err != nil {
		return errors.New("storage dirs must share one filesystem (atomic rename, hardlinks): " + err.Error())
	}
	mir := &mirror.Mirror{Current: cfg.Storage.CurrentDir, Tmp: cfg.Storage.TmpDir}
	mir.CleanTemp()

	st, err := store.Open(cfg.Storage.DB)
	if err != nil {
		return err
	}
	defer st.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dev := device.New(cfg.Device.Host, cfg.Device.RESTPort, cfg.Device.RESTPassword, cfg.Sync.DevicePollTimeout)
	th := throttle.New(cfg.Throttle.BytesPerSecond, cfg.Throttle.CommandDelay)
	if v, err := strconv.Atoi(st.GetKV("throttle_bps")); err == nil {
		d := cfg.Throttle.CommandDelay
		if ms, err := strconv.Atoi(st.GetKV("throttle_delay_ms")); err == nil {
			d = time.Duration(ms) * time.Millisecond
		}
		th.Set(v, d) // runtime settings from the UI win over the config file
	}
	cp := copier.New(cfg, st, dev, th, mir)
	cr := crawler.New(cfg, st, dev, th, mir, cp.Wake)

	sn := snapshot.New(cfg, st, mir)
	vf := verify.New(cfg.Verify, st, mir, cp.Wake)

	var wg sync.WaitGroup
	for _, f := range []func(){
		func() { dev.Run(ctx, 15*time.Second) },
		func() { cr.Run(ctx) },
		func() { cp.Run(ctx) },
		func() { sn.Run(ctx) },
		func() { vf.Run(ctx) },
	} {
		wg.Add(1)
		go func() { defer wg.Done(); f() }()
	}

	handler, err := (&web.Server{Store: st, Device: dev, Crawler: cr, Copier: cp, Snapshots: sn,
		Throttle: th, Mirror: mir, Verifier: vf, Roots: cfg.Device.Roots,
		User: cfg.Web.User, Password: cfg.Web.Password}).Handler()
	if err != nil {
		return err
	}
	srv := &http.Server{Addr: cfg.Web.Listen, Handler: handler}
	go func() {
		slog.Info("web listening", "addr", cfg.Web.Listen)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("web", "err", err)
			stop()
		}
	}()

	if cfg.Web.Password == "" && !loopback(cfg.Web.Listen) {
		slog.Warn("web UI is reachable from the network without authentication; set web.password (or U64_WEB_PASSWORD)", "listen", cfg.Web.Listen)
	}
	slog.Info("started", "version", version, "host", cfg.Device.Host, "roots", cfg.Device.Roots, "data", cfg.Storage.DataDir)
	<-ctx.Done()
	shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(shut)
	wg.Wait()
	return nil
}

func loopback(listen string) bool {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
