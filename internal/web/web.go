// Package web serves the status API and the htmx UI.
package web

import (
	"archive/zip"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/schorsch3000/u64-sync/internal/copier"
	"github.com/schorsch3000/u64-sync/internal/crawler"
	"github.com/schorsch3000/u64-sync/internal/device"
	"github.com/schorsch3000/u64-sync/internal/mirror"
	"github.com/schorsch3000/u64-sync/internal/snapshot"
	"github.com/schorsch3000/u64-sync/internal/store"
	"github.com/schorsch3000/u64-sync/internal/throttle"
	"github.com/schorsch3000/u64-sync/internal/verify"
)

//go:embed static templates
var assets embed.FS

type Server struct {
	Store     *store.Store
	Device    *device.Monitor
	Crawler   *crawler.Crawler
	Copier    *copier.Copier
	Snapshots *snapshot.Manager
	Throttle  *throttle.Throttle
	Mirror    *mirror.Mirror
	Verifier  *verify.Verifier
	Roots     []string

	// Basic auth is enforced when Password is not empty. /healthz is always open.
	User, Password string

	pages map[string]*template.Template
}

type throttleView struct {
	KBps    int  `json:"kbytes_per_second"` // 0 = unlimited
	DelayMS int  `json:"command_delay_ms"`
	Paused  bool `json:"paused"`
}

type status struct {
	Time       time.Time       `json:"time"`
	Device     device.State    `json:"device"`
	Roots      []string        `json:"roots"`
	Crawler    crawler.Status  `json:"crawler"`
	Copier     copier.Status   `json:"copier"`
	Counts     store.Counts    `json:"counts"`
	Snapshots  snapshot.Status `json:"snapshots"`
	Verify     verify.Status   `json:"verify"`
	Throttle   throttleView    `json:"throttle"`
	CompleteAt string          `json:"last_complete_sync,omitempty"`
	CrawlOKAt  string          `json:"last_complete_crawl,omitempty"`
	Problems   []store.Problem `json:"-"`
}

func (s *Server) status() status {
	c, _ := s.Store.Counts()
	bps, delay := s.Throttle.Settings()
	pr, _ := s.Store.Problems(15)
	return status{
		Time: time.Now(), Device: s.Device.State(), Roots: s.Roots,
		Crawler: s.Crawler.Status(), Copier: s.Copier.Status(), Counts: c, Snapshots: s.Snapshots.Status(), Verify: s.Verifier.Status(),
		Throttle:   throttleView{bps / 1024, int(delay / time.Millisecond), s.Throttle.Paused()},
		CompleteAt: s.Store.GetKV("complete_at"), CrawlOKAt: s.Store.GetKV("crawl_ok_at"),
		Problems: pr,
	}
}

var funcs = template.FuncMap{
	"bytes": func(n int64) string {
		const u = "KMGT"
		if n < 1024 {
			return fmt.Sprintf("%d B", n)
		}
		f, i := float64(n)/1024, 0
		for f >= 1024 && i < len(u)-1 {
			f /= 1024
			i++
		}
		return fmt.Sprintf("%.1f %ciB", f, u[i])
	},
	"ago": func(t time.Time) string {
		if t.IsZero() || t.Year() < 2000 {
			return "never"
		}
		d := time.Since(t).Round(time.Second)
		switch {
		case d < time.Minute:
			return d.String() + " ago"
		case d < time.Hour:
			return fmt.Sprintf("%dm ago", int(d.Minutes()))
		case d < 48*time.Hour:
			return fmt.Sprintf("%dh ago", int(d.Hours()))
		}
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	},
	"date": func(unix int64) string { return time.Unix(unix, 0).UTC().Format("2006-01-02 15:04") },
	"unixtime": func(unix int64) string {
		if unix <= 0 {
			return ""
		}
		return time.Unix(unix, 0).Format("15:04:05")
	},
	"str": func(v any) string { return fmt.Sprint(v) },
}

func (s *Server) load() error {
	s.pages = map[string]*template.Template{}
	for _, name := range []string{"index", "files"} {
		t, err := template.New("").Funcs(funcs).ParseFS(assets, "templates/base.html", "templates/"+name+".html", "templates/status.html")
		if err != nil {
			return err
		}
		s.pages[name] = t
	}
	return nil
}

func (s *Server) render(w http.ResponseWriter, page, block string, data any) {
	var err error
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if block != "" {
		err = s.pages[page].ExecuteTemplate(w, block, data)
	} else {
		err = s.pages[page].ExecuteTemplate(w, "base", data)
	}
	if err != nil {
		slog.Error("render", "page", page, "err", err)
	}
}

func (s *Server) Handler() (http.Handler, error) {
	if err := s.load(); err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	top := http.NewServeMux()
	top.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok\n") })
	top.Handle("/", s.auth(mux))
	static, _ := fs.Sub(assets, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))

	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { s.render(w, "index", "", s.status()) })
	mux.HandleFunc("GET /ui/status", func(w http.ResponseWriter, r *http.Request) { s.render(w, "index", "status", s.status()) })

	// Actions return the refreshed status partial so htmx can swap it in place.
	act := func(f func(r *http.Request) error) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if err := f(r); err != nil {
				slog.Warn("action", "path", r.URL.Path, "err", err)
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			s.render(w, "index", "status", s.status())
		}
	}
	mux.HandleFunc("POST /ui/crawl", act(func(r *http.Request) error {
		select {
		case s.Crawler.Trigger <- struct{}{}:
		default:
		}
		return nil
	}))
	mux.HandleFunc("POST /ui/pause", act(func(r *http.Request) error {
		s.Throttle.SetPaused(r.FormValue("paused") == "1")
		return nil
	}))
	mux.HandleFunc("POST /ui/snapshot", act(func(r *http.Request) error { return s.Snapshots.Now() }))
	mux.HandleFunc("POST /ui/approve", act(func(r *http.Request) error {
		root := r.FormValue("root")
		for _, known := range s.Roots {
			if known == root {
				s.Crawler.ApproveBrake(root)
				return nil
			}
		}
		return fmt.Errorf("unknown root %q", root)
	}))
	mux.HandleFunc("POST /ui/throttle", act(func(r *http.Request) error {
		kb, err1 := strconv.Atoi(r.FormValue("kbps"))
		ms, err2 := strconv.Atoi(r.FormValue("delay_ms"))
		if err1 != nil || err2 != nil || kb < 0 || ms < 0 || ms > 60000 {
			return fmt.Errorf("invalid throttle values")
		}
		s.Throttle.Set(kb*1024, time.Duration(ms)*time.Millisecond)
		s.Store.SetKV("throttle_bps", strconv.Itoa(kb*1024))
		s.Store.SetKV("throttle_delay_ms", strconv.Itoa(ms))
		return nil
	}))

	mux.HandleFunc("GET /files/{path...}", s.files)
	mux.HandleFunc("GET /dl/{path...}", s.download)
	mux.HandleFunc("GET /zip/{path...}", s.zip)

	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		enc.Encode(s.status())
	})
	mux.HandleFunc("POST /api/crawl", func(w http.ResponseWriter, r *http.Request) {
		select {
		case s.Crawler.Trigger <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusAccepted)
	})
	return top, nil
}

// auth enforces HTTP basic auth when a password is configured.
func (s *Server) auth(next http.Handler) http.Handler {
	if s.Password == "" {
		return next
	}
	// Compare fixed-length digests so neither length nor content leaks through timing.
	sum := func(v string) [32]byte { return sha256.Sum256([]byte(v)) }
	wantU, wantP := sum(s.User), sum(s.Password)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		gotU, gotP := sum(u), sum(p)
		okU := subtle.ConstantTimeCompare(gotU[:], wantU[:])
		okP := subtle.ConstantTimeCompare(gotP[:], wantP[:])
		if !ok || okU&okP != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="u64-sync", charset="UTF-8"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// resolve splits a URL path like "SD/Games/x" into the configured root ("/SD") and the rest.
func (s *Server) resolve(p string) (root, rel string, ok bool) {
	p = strings.Trim(p, "/")
	first, rest, _ := strings.Cut(p, "/")
	for _, r := range s.Roots {
		if strings.TrimPrefix(r, "/") == first {
			return r, rest, true
		}
	}
	return "", "", false
}

type crumb struct{ Name, URL string }

type filesView struct {
	Title    string
	Crumbs   []crumb
	Roots    []rootRow
	Entries  []store.DirEntry
	Base     string // URL prefix of the current dir, for links
	ZipURL   string
	IsRoot   bool
	NoteDead bool
}

type rootRow struct {
	Name    string
	Files   int
	Bytes   int64
	Pending int
}

func (s *Server) files(w http.ResponseWriter, r *http.Request) {
	p := strings.Trim(r.PathValue("path"), "/")
	v := filesView{Title: "Files"}
	if p == "" {
		v.IsRoot = true
		for _, root := range s.Roots {
			n, b, pend, _ := s.Store.DirStat(root, "")
			v.Roots = append(v.Roots, rootRow{strings.TrimPrefix(root, "/"), n, b, pend})
		}
		s.render(w, "files", "", v)
		return
	}
	root, rel, ok := s.resolve(p)
	if !ok || path.Clean(rel) != rel && rel != "" {
		http.NotFound(w, r)
		return
	}
	ents, err := s.Store.ListDir(root, rel)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if len(ents) == 0 && rel != "" {
		http.NotFound(w, r)
		return
	}
	v.Entries, v.Base, v.ZipURL = ents, "/files/"+p+"/", "/zip/"+p
	v.Crumbs = []crumb{{"/", "/files/"}}
	acc := ""
	for _, part := range strings.Split(p, "/") {
		acc += "/" + part
		v.Crumbs = append(v.Crumbs, crumb{part, "/files" + acc})
	}
	s.render(w, "files", "", v)
}

func (s *Server) localPath(urlPath string) (string, bool) {
	root, rel, ok := s.resolve(urlPath)
	if !ok || rel == "" {
		return "", false
	}
	p, err := s.Mirror.Path(root, rel)
	return p, err == nil
}

func (s *Server) download(w http.ResponseWriter, r *http.Request) {
	p, ok := s.localPath(r.PathValue("path"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(p)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filepath.Base(p)))
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, filepath.Base(p), st.ModTime(), f)
}

// zip streams a directory (or a whole root) of the mirror as a zip archive.
func (s *Server) zip(w http.ResponseWriter, r *http.Request) {
	urlPath := strings.Trim(r.PathValue("path"), "/")
	root, rel, ok := s.resolve(urlPath)
	if !ok {
		http.NotFound(w, r)
		return
	}
	dir := filepath.Join(s.Mirror.Current, strings.TrimPrefix(root, "/"))
	if rel != "" {
		var err error
		if dir, err = s.Mirror.Path(root, rel); err != nil {
			http.NotFound(w, r)
			return
		}
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", strings.ReplaceAll(urlPath, "/", "_")+".zip"))
	zw := zip.NewWriter(w)
	defer zw.Close()
	base := filepath.Dir(dir)
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return nil
		}
		if r.Context().Err() != nil {
			return r.Context().Err()
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		rp, _ := filepath.Rel(base, p)
		h, err := zip.FileInfoHeader(info)
		if err != nil {
			return nil
		}
		h.Name, h.Method = filepath.ToSlash(rp), zip.Deflate
		out, err := zw.CreateHeader(h)
		if err != nil {
			return err
		}
		f, err := os.Open(p)
		if err != nil {
			return nil
		}
		defer f.Close()
		_, err = io.Copy(out, f)
		return err
	})
}
