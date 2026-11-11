// Package store keeps the inventory of remote files and their backup state in SQLite.
package store

import (
	"database/sql"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// File is one remote file as last seen by the crawler.
type File struct {
	Root  string // remote root, e.g. /SD
	Path  string // relative to root, '/' separated, no leading slash
	Size  int64
	MTime int64 // unix seconds, device local time read as UTC
}

type Store struct{ db *sql.DB }

var migrations = []string{`
CREATE TABLE files(
  root TEXT NOT NULL,
  path TEXT NOT NULL,
  size INTEGER NOT NULL,
  mtime INTEGER NOT NULL,
  seen_gen INTEGER NOT NULL,
  first_seen INTEGER NOT NULL,
  deleted_at INTEGER,
  pending INTEGER NOT NULL DEFAULT 1,
  b_size INTEGER,
  b_mtime INTEGER,
  sha256 TEXT,
  backed_up_at INTEGER,
  fail_count INTEGER NOT NULL DEFAULT 0,
  last_error TEXT,
  next_try INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(root, path)
) WITHOUT ROWID;
CREATE INDEX files_pending ON files(next_try) WHERE pending = 1 AND deleted_at IS NULL;
CREATE INDEX files_deleted ON files(deleted_at) WHERE deleted_at IS NOT NULL;
CREATE TABLE kv(k TEXT PRIMARY KEY, v TEXT NOT NULL) WITHOUT ROWID;
`, `
ALTER TABLE files ADD COLUMN verified_at INTEGER;
CREATE INDEX files_verify ON files(verified_at) WHERE b_mtime IS NOT NULL;
`}

func Open(file string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+file+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	var v int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		return err
	}
	for i := v; i < len(migrations); i++ {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) GetKV(k string) string {
	var v string
	s.db.QueryRow("SELECT v FROM kv WHERE k=?", k).Scan(&v)
	return v
}

func (s *Store) SetKV(k, v string) error {
	_, err := s.db.Exec("INSERT INTO kv(k,v) VALUES(?,?) ON CONFLICT(k) DO UPDATE SET v=excluded.v", k, v)
	return err
}

// NextGen allocates the generation number for a new crawl pass.
func (s *Store) NextGen() (int64, error) {
	g, _ := strconv.ParseInt(s.GetKV("gen"), 10, 64)
	g++
	return g, s.SetKV("gen", strconv.FormatInt(g, 10))
}

// Upsert records files seen in one directory listing. Changed size or mtime
// re-queues the file; unchanged files keep their state. A file that reappears
// after deletion is revived.
func (s *Store) Upsert(files []File, gen int64) error {
	if len(files) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	st, err := tx.Prepare(`
INSERT INTO files(root,path,size,mtime,seen_gen,first_seen,pending) VALUES(?,?,?,?,?,?,1)
ON CONFLICT(root,path) DO UPDATE SET
  pending    = CASE WHEN b_size IS NOT excluded.size OR b_mtime IS NOT excluded.mtime THEN 1 ELSE 0 END,
  fail_count = CASE WHEN size != excluded.size OR mtime != excluded.mtime THEN 0 ELSE fail_count END,
  next_try   = CASE WHEN size != excluded.size OR mtime != excluded.mtime THEN 0 ELSE next_try END,
  size = excluded.size, mtime = excluded.mtime, seen_gen = excluded.seen_gen, deleted_at = NULL`)
	if err != nil {
		return err
	}
	defer st.Close()
	now := time.Now().Unix()
	for _, f := range files {
		if _, err := st.Exec(f.Root, f.Path, f.Size, f.MTime, gen, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// CountMissing returns live files of root not seen in generation gen, and all live files of root.
func (s *Store) CountMissing(root string, gen int64) (missing, live int, err error) {
	err = s.db.QueryRow(`SELECT
	  COALESCE(SUM(seen_gen < ?),0), COUNT(*) FROM files WHERE root=? AND deleted_at IS NULL`, gen, root).Scan(&missing, &live)
	return
}

func (s *Store) MarkMissingDeleted(root string, gen int64) (int64, error) {
	r, err := s.db.Exec(`UPDATE files SET deleted_at=? WHERE root=? AND seen_gen<? AND deleted_at IS NULL`,
		time.Now().Unix(), root, gen)
	if err != nil {
		return 0, err
	}
	return r.RowsAffected()
}

// Purgeable returns deleted files whose grace period ended.
func (s *Store) Purgeable(grace time.Duration) ([]File, error) {
	rows, err := s.db.Query(`SELECT root,path,size,mtime FROM files WHERE deleted_at IS NOT NULL AND deleted_at<=?`,
		time.Now().Add(-grace).Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []File
	for rows.Next() {
		var f File
		if err := rows.Scan(&f.Root, &f.Path, &f.Size, &f.MTime); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (s *Store) Forget(f File) error {
	_, err := s.db.Exec(`DELETE FROM files WHERE root=? AND path=? AND deleted_at IS NOT NULL`, f.Root, f.Path)
	return err
}

// NextPending returns the next file to back up: never-backed-up files first, then changed ones.
func (s *Store) NextPending() (File, bool, error) {
	var f File
	err := s.db.QueryRow(`SELECT root,path,size,mtime FROM files
	  WHERE pending=1 AND deleted_at IS NULL AND next_try<=?
	  ORDER BY (b_mtime IS NULL) DESC, root, path LIMIT 1`, time.Now().Unix()).Scan(&f.Root, &f.Path, &f.Size, &f.MTime)
	if err == sql.ErrNoRows {
		return f, false, nil
	}
	return f, err == nil, err
}

// MarkBackedUp records a finished download. The file stays pending if the
// crawler saw newer metadata while we were copying.
func (s *Store) MarkBackedUp(f File, sha string) error {
	_, err := s.db.Exec(`UPDATE files SET b_size=?, b_mtime=?, sha256=?, backed_up_at=?, verified_at=?,
	  fail_count=0, last_error=NULL, next_try=0, pending = (size != ? OR mtime != ?)
	  WHERE root=? AND path=?`, f.Size, f.MTime, sha, time.Now().Unix(), time.Now().Unix(), f.Size, f.MTime, f.Root, f.Path)
	return err
}

// UpdateRemote stores fresher metadata found by the copier (file changed while downloading).
func (s *Store) UpdateRemote(f File) error {
	_, err := s.db.Exec(`UPDATE files SET size=?, mtime=?, pending=1, next_try=0 WHERE root=? AND path=?`,
		f.Size, f.MTime, f.Root, f.Path)
	return err
}

// Fail records a failed copy with exponential backoff.
func (s *Store) Fail(f File, cause error) error {
	var n int
	s.db.QueryRow(`SELECT fail_count FROM files WHERE root=? AND path=?`, f.Root, f.Path).Scan(&n)
	n++
	backoff := time.Minute << min(n, 9) // 2m .. ~8.5h
	_, err := s.db.Exec(`UPDATE files SET fail_count=?, last_error=?, next_try=? WHERE root=? AND path=?`,
		n, cause.Error(), time.Now().Add(backoff).Unix(), f.Root, f.Path)
	return err
}

type Counts struct {
	Files       int   `json:"files"`
	Bytes       int64 `json:"bytes"`
	Pending     int   `json:"pending"`
	PendingByte int64 `json:"pending_bytes"`
	Failing     int   `json:"failing"`
	Deleted     int   `json:"deleted_remote"`
}

func (s *Store) Counts() (c Counts, err error) {
	err = s.db.QueryRow(`SELECT
	  COALESCE(SUM(deleted_at IS NULL),0),
	  COALESCE(SUM(CASE WHEN deleted_at IS NULL THEN size END),0),
	  COALESCE(SUM(pending=1 AND deleted_at IS NULL),0),
	  COALESCE(SUM(CASE WHEN pending=1 AND deleted_at IS NULL THEN size END),0),
	  COALESCE(SUM(fail_count>0 AND pending=1 AND deleted_at IS NULL),0),
	  COALESCE(SUM(deleted_at IS NOT NULL),0)
	  FROM files`).Scan(&c.Files, &c.Bytes, &c.Pending, &c.PendingByte, &c.Failing, &c.Deleted)
	return
}

// DirEntry is one row of a directory listing built from the inventory.
type DirEntry struct {
	Name    string
	IsDir   bool
	Size    int64 // file size, or total bytes below a dir
	MTime   int64
	Files   int // files below a dir
	Pending int // files below a dir (or this file) not yet backed up
	Deleted int // files deleted on the device below a dir (or this file)
	Failing int
	Err     string
}

// DirStat aggregates everything below prefix ("" = whole root) that is live on the device.
func (s *Store) DirStat(root, dir string) (files int, bytes int64, pending int, err error) {
	lo, hi := bounds(dir)
	err = s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(size),0), COALESCE(SUM(pending),0) FROM files
	  WHERE root=? AND deleted_at IS NULL AND path>=? AND path<?`, root, lo, hi).Scan(&files, &bytes, &pending)
	return
}

func bounds(dir string) (lo, hi string) {
	if dir == "" {
		return "", "\U0010ffff"
	}
	return dir + "/", dir + "0" // '0' is the byte after '/'
}

// ListDir lists the direct children of dir, derived from file paths, directories first.
func (s *Store) ListDir(root, dir string) ([]DirEntry, error) {
	lo, hi := bounds(dir)
	rows, err := s.db.Query(`SELECT path,size,mtime,pending,deleted_at IS NOT NULL,fail_count,COALESCE(last_error,'')
	  FROM files WHERE root=? AND path>=? AND path<?`, root, lo, hi)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	dirs := map[string]*DirEntry{}
	var out []DirEntry
	for rows.Next() {
		var p, lastErr string
		var size, mtime int64
		var pending, deleted, fails int
		if err := rows.Scan(&p, &size, &mtime, &pending, &deleted, &fails, &lastErr); err != nil {
			return nil, err
		}
		rest := p[len(lo):]
		name, isDir := rest, false
		for i := 0; i < len(rest); i++ {
			if rest[i] == '/' {
				name, isDir = rest[:i], true
				break
			}
		}
		if !isDir {
			e := DirEntry{Name: name, Size: size, MTime: mtime, Err: lastErr}
			if deleted == 1 {
				e.Deleted = 1
			} else if pending == 1 {
				e.Pending = 1
				if fails > 0 {
					e.Failing = 1
				}
			}
			out = append(out, e)
			continue
		}
		d := dirs[name]
		if d == nil {
			d = &DirEntry{Name: name, IsDir: true}
			dirs[name] = d
		}
		if deleted == 1 {
			d.Deleted++
			continue
		}
		d.Files++
		d.Size += size
		if mtime > d.MTime {
			d.MTime = mtime
		}
		if pending == 1 {
			d.Pending++
			if fails > 0 {
				d.Failing++
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, d := range dirs {
		out = append(out, *d)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].IsDir != out[j].IsDir {
			return out[i].IsDir
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, nil
}

type Problem struct {
	Root, Path string
	Fails      int
	Err        string
	NextTry    int64
}

// Problems returns files whose backup keeps failing.
func (s *Store) Problems(limit int) ([]Problem, error) {
	rows, err := s.db.Query(`SELECT root,path,fail_count,COALESCE(last_error,''),next_try FROM files
	  WHERE pending=1 AND fail_count>0 AND deleted_at IS NULL ORDER BY fail_count DESC, root, path LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Problem
	for rows.Next() {
		var p Problem
		if err := rows.Scan(&p.Root, &p.Path, &p.Fails, &p.Err, &p.NextTry); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Backup is a stored local copy as recorded at download time.
type Backup struct {
	File
	BSize  int64
	SHA256 string
}

// VerifyBatch returns up to limit backed-up files not verified since olderThan, least recently verified first.
func (s *Store) VerifyBatch(limit int, olderThan time.Time) ([]Backup, error) {
	rows, err := s.db.Query(`SELECT root,path,size,mtime,b_size,sha256 FROM files
	  WHERE b_mtime IS NOT NULL AND sha256 IS NOT NULL AND (verified_at IS NULL OR verified_at<?)
	  ORDER BY COALESCE(verified_at,0) LIMIT ?`, olderThan.Unix(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Backup
	for rows.Next() {
		var b Backup
		if err := rows.Scan(&b.Root, &b.Path, &b.Size, &b.MTime, &b.BSize, &b.SHA256); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// BackupSHA returns the currently recorded backup hash of a file ("" if none).
func (s *Store) BackupSHA(f File) string {
	var h sql.NullString
	s.db.QueryRow(`SELECT sha256 FROM files WHERE root=? AND path=?`, f.Root, f.Path).Scan(&h)
	return h.String
}

func (s *Store) MarkVerified(f File) error {
	_, err := s.db.Exec(`UPDATE files SET verified_at=? WHERE root=? AND path=?`, time.Now().Unix(), f.Root, f.Path)
	return err
}

// FlagCorrupt forgets the backup state so the copier downloads the file again.
// The backup columns are cleared on purpose: the crawler recomputes "pending"
// from them and would otherwise un-queue the file.
func (s *Store) FlagCorrupt(f File, reason string) error {
	_, err := s.db.Exec(`UPDATE files SET pending=1, b_size=NULL, b_mtime=NULL, sha256=NULL, verified_at=NULL,
	  last_error=?, next_try=0 WHERE root=? AND path=?`, reason, f.Root, f.Path)
	return err
}

type VerifyStats struct {
	Verified int `json:"verified"` // checked within the interval
	Stale    int `json:"stale"`    // backed up, check due
}

func (s *Store) VerifyStats(olderThan time.Time) (v VerifyStats, err error) {
	err = s.db.QueryRow(`SELECT COALESCE(SUM(verified_at>=?),0), COALESCE(SUM(verified_at IS NULL OR verified_at<?),0)
	  FROM files WHERE b_mtime IS NOT NULL`, olderThan.Unix(), olderThan.Unix()).Scan(&v.Verified, &v.Stale)
	return
}
