// Package config loads and validates the service configuration.
package config

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Device    Device    `yaml:"device"`
	Storage   Storage   `yaml:"storage"`
	Sync      Sync      `yaml:"sync"`
	Throttle  Throttle  `yaml:"throttle"`
	Snapshots Snapshots `yaml:"snapshots"`
	Verify    Verify    `yaml:"verify"`
	Web       Web       `yaml:"web"`
}

type Device struct {
	Host         string   `yaml:"host"`
	FTPPort      int      `yaml:"ftp_port"`
	RESTPort     int      `yaml:"rest_port"`
	FTPUser      string   `yaml:"ftp_user"`
	FTPPassword  string   `yaml:"ftp_password"`  // env U64_FTP_PASSWORD overrides
	RESTPassword string   `yaml:"rest_password"` // env U64_REST_PASSWORD overrides
	Roots        []string `yaml:"roots"`         // remote folders to sync, e.g. /SD
}

type Storage struct {
	DataDir     string `yaml:"data_dir"`
	CurrentDir  string `yaml:"current_dir"`  // default <data_dir>/current
	SnapshotDir string `yaml:"snapshot_dir"` // default <data_dir>/snapshots; must be on same fs as current_dir
	TmpDir      string `yaml:"tmp_dir"`      // default <data_dir>/tmp; must be on same fs as current_dir
	DB          string `yaml:"db"`           // default <data_dir>/u64sync.db
}

type Sync struct {
	CrawlInterval     time.Duration `yaml:"crawl_interval"`
	DeleteGrace       time.Duration `yaml:"delete_grace"`        // keep locally this long after remote delete; 0 = at once
	DeleteBrakePct    int           `yaml:"delete_brake_pct"`    // refuse deletions when more than this % of a root vanished at once
	DeleteBrakeMin    int           `yaml:"delete_brake_min"`    // ...and at least this many files
	Excludes          []string      `yaml:"excludes"`            // path.Match globs, tested against full relative path and base name
	DevicePollTimeout time.Duration `yaml:"device_poll_timeout"` // REST liveness timeout
}

type Throttle struct {
	BytesPerSecond int           `yaml:"bytes_per_second"` // 0 = unlimited
	CommandDelay   time.Duration `yaml:"command_delay"`    // minimum gap between FTP commands, shared by all workers
}

type Snapshots struct {
	Enabled  bool          `yaml:"enabled"`
	Interval time.Duration `yaml:"interval"` // minimum gap between snapshots
	Keep     struct {
		Last    int `yaml:"last"`
		Daily   int `yaml:"daily"`
		Weekly  int `yaml:"weekly"`
		Monthly int `yaml:"monthly"`
	} `yaml:"keep"`
}

// Verify re-hashes local files against the stored sha256 to catch bit rot on the backup disk.
type Verify struct {
	Enabled        bool          `yaml:"enabled"`
	Interval       time.Duration `yaml:"interval"`         // every file is re-checked at least this often
	BytesPerSecond int           `yaml:"bytes_per_second"` // local disk read rate, 0 = unlimited
}

type Web struct {
	Listen   string `yaml:"listen"`
	User     string `yaml:"user"`     // default "admin"; env U64_WEB_USER overrides
	Password string `yaml:"password"` // basic auth is on when set; env U64_WEB_PASSWORD overrides
}

func Default() Config {
	var c Config
	c.Device.FTPPort = 21
	c.Device.RESTPort = 80
	c.Device.FTPUser = "anonymous"
	c.Device.Roots = []string{"/SD", "/Flash"}
	c.Storage.DataDir = "./data"
	c.Sync.CrawlInterval = 6 * time.Hour
	c.Sync.DeleteGrace = 7 * 24 * time.Hour
	c.Sync.DeleteBrakePct = 20
	c.Sync.DeleteBrakeMin = 20
	c.Sync.DevicePollTimeout = 5 * time.Second
	c.Throttle.BytesPerSecond = 200 * 1024
	c.Throttle.CommandDelay = 200 * time.Millisecond
	c.Snapshots.Enabled = true
	c.Snapshots.Interval = 24 * time.Hour
	c.Snapshots.Keep.Last = 1
	c.Verify.Enabled = true
	c.Verify.Interval = 30 * 24 * time.Hour
	c.Verify.BytesPerSecond = 20 << 20
	c.Web.Listen = "127.0.0.1:8064"
	c.Web.User = "admin"
	return c
}

// Load reads path (may be empty for defaults only), applies env overrides and validates.
func Load(file string) (Config, error) {
	c := Default()
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return c, err
		}
		dec := yaml.NewDecoder(strings.NewReader(string(b)))
		dec.KnownFields(true)
		if err := dec.Decode(&c); err != nil {
			return c, fmt.Errorf("%s: %w", file, err)
		}
	}
	if v := os.Getenv("U64_HOST"); v != "" {
		c.Device.Host = v
	}
	if v := os.Getenv("U64_FTP_PASSWORD"); v != "" {
		c.Device.FTPPassword = v
	}
	if v := os.Getenv("U64_REST_PASSWORD"); v != "" {
		c.Device.RESTPassword = v
	}
	if v := os.Getenv("U64_WEB_USER"); v != "" {
		c.Web.User = v
	}
	if v := os.Getenv("U64_WEB_PASSWORD"); v != "" {
		c.Web.Password = v
	}
	if v := os.Getenv("U64_WEB_LISTEN"); v != "" {
		c.Web.Listen = v
	}
	if v := os.Getenv("U64_DATA_DIR"); v != "" {
		c.Storage.DataDir = v
	}
	return c, c.finish()
}

func (c *Config) finish() error {
	if c.Device.Host == "" {
		return fmt.Errorf("device.host is required (set it in config.yaml next to where you run the binary, pass -config <file>, or set U64_HOST)")
	}
	roots, err := NormalizeRoots(c.Device.Roots)
	if err != nil {
		return err
	}
	c.Device.Roots = roots
	s := &c.Storage
	if s.CurrentDir == "" {
		s.CurrentDir = filepath.Join(s.DataDir, "current")
	}
	if s.SnapshotDir == "" {
		s.SnapshotDir = filepath.Join(s.DataDir, "snapshots")
	}
	if s.TmpDir == "" {
		s.TmpDir = filepath.Join(s.DataDir, "tmp")
	}
	if s.DB == "" {
		s.DB = filepath.Join(s.DataDir, "u64sync.db")
	}
	if c.Verify.Enabled && c.Verify.Interval < time.Hour {
		return fmt.Errorf("verify.interval must be at least 1h")
	}
	if c.Sync.CrawlInterval < time.Minute {
		return fmt.Errorf("sync.crawl_interval must be at least 1m")
	}
	for _, p := range c.Sync.Excludes {
		if _, err := path.Match(p, ""); err != nil {
			return fmt.Errorf("sync.excludes: bad pattern %q: %w", p, err)
		}
	}
	return nil
}

// NormalizeRoots cleans roots and rejects "/", duplicates and overlaps.
func NormalizeRoots(in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, fmt.Errorf("device.roots must not be empty")
	}
	var out []string
	for _, r := range in {
		r = path.Clean("/" + strings.TrimSpace(r))
		if r == "/" {
			return nil, fmt.Errorf("device.roots: generic root \"/\" not allowed, list folders like /SD")
		}
		out = append(out, r)
	}
	for i, a := range out {
		for j, b := range out {
			if i != j && (a == b || strings.HasPrefix(b, a+"/")) {
				return nil, fmt.Errorf("device.roots: %q and %q overlap", a, b)
			}
		}
	}
	return out, nil
}
