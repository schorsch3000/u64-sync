// Package fakeftp is an in-process FTP server that speaks the subset of the
// Ultimate64's dialect u64-sync uses (MLSD, MLST, RETR over PASV), with fault
// injection for tests.
package fakeftp

import (
	"bufio"
	"fmt"
	"net"
	"path"
	"sort"
	"strings"
	"sync"
	"time"
)

type file struct {
	data  []byte
	mtime time.Time
}

type Server struct {
	ln net.Listener

	mu    sync.Mutex
	files map[string]*file
	dirs  map[string]bool

	// Fault injection, guarded by mu; use the setter methods.
	failList map[string]bool
	dropList map[string]bool
	dropRetr map[string]bool
	onRetr   func(path string)
	retrs    map[string]int
}

// New starts a server on a random local port and stops it when the test ends.
func New(t interface{ Cleanup(func()) }) *Server {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	s := &Server{ln: ln, files: map[string]*file{}, dirs: map[string]bool{},
		failList: map[string]bool{}, dropList: map[string]bool{}, dropRetr: map[string]bool{}, retrs: map[string]int{}}
	go s.accept()
	t.Cleanup(func() { ln.Close() })
	return s
}

func (s *Server) Host() string { return "127.0.0.1" }
func (s *Server) Port() int    { return s.ln.Addr().(*net.TCPAddr).Port }

// Put creates or replaces a file; parent directories exist implicitly.
func (s *Server) Put(p string, data string, mtime time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.files[path.Clean(p)] = &file{[]byte(data), mtime.UTC().Truncate(time.Second)}
}

func (s *Server) Remove(p string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.files, path.Clean(p))
}

// MkDir creates an explicit (possibly empty) directory.
func (s *Server) MkDir(p string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dirs[path.Clean(p)] = true
}

// FailList makes MLSD of dir answer 550.
func (s *Server) FailList(dir string, on bool) { s.set(s.failList, path.Clean(dir), on) }

// DropList makes the server hang up when dir is listed.
func (s *Server) DropList(dir string, on bool) { s.set(s.dropList, path.Clean(dir), on) }

// DropRetr makes the server send half of the file and hang up.
func (s *Server) DropRetr(p string, on bool) { s.set(s.dropRetr, path.Clean(p), on) }

func (s *Server) set(m map[string]bool, k string, on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if on {
		m[k] = true
	} else {
		delete(m, k)
	}
}

// OnRetr registers a hook run after file data was sent and before the 226 reply.
func (s *Server) OnRetr(f func(path string)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onRetr = f
}

func (s *Server) Retrs(p string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.retrs[path.Clean(p)]
}

func (s *Server) accept() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.serve(c)
	}
}

func facts(typ string, size int, t time.Time, name string) string {
	return fmt.Sprintf("type=%s;size=%d;modify=%s; %s", typ, size, t.UTC().Format("20060102150405"), name)
}

// children lists direct entries of dir; ok=false if dir does not exist.
func (s *Server) children(dir string) (lines []string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prefix := strings.TrimSuffix(dir, "/") + "/"
	seen := map[string]bool{}
	var out []string
	exists := s.dirs[dir]
	add := func(p string, f *file) {
		if !strings.HasPrefix(p, prefix) {
			return
		}
		exists = true
		rest := p[len(prefix):]
		name, _, isDir := strings.Cut(rest, "/")
		if seen[name] {
			return
		}
		seen[name] = true
		if isDir || f == nil {
			out = append(out, facts("dir", 0, time.Unix(0, 0), name))
		} else {
			out = append(out, facts("file", len(f.data), f.mtime, name))
		}
	}
	for p, f := range s.files {
		add(p, f)
	}
	for d := range s.dirs {
		add(d, nil)
	}
	sort.Strings(out)
	return out, exists
}

func (s *Server) serve(c net.Conn) {
	defer c.Close()
	br := bufio.NewReader(c)
	send := func(format string, a ...any) { fmt.Fprintf(c, format+"\r\n", a...) }
	send("220 fake ultimate")
	var pasv net.Listener
	defer func() {
		if pasv != nil {
			pasv.Close()
		}
	}()
	data := func() (net.Conn, error) {
		defer func() { pasv.Close(); pasv = nil }()
		return pasv.Accept()
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		cmd, arg, _ := strings.Cut(strings.TrimRight(line, "\r\n"), " ")
		arg = path.Clean("/" + strings.TrimPrefix(arg, "/"))
		switch strings.ToUpper(cmd) {
		case "USER":
			send("331 User name okay, need password.")
		case "PASS":
			send("230 User logged in, proceed.")
		case "TYPE", "NOOP":
			send("200 Command okay.")
		case "QUIT":
			send("221 bye")
			return
		case "PASV":
			if pasv != nil {
				pasv.Close()
			}
			pasv, err = net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				send("425 no port")
				continue
			}
			p := pasv.Addr().(*net.TCPAddr).Port
			send("227 Entering Passive Mode (127,0,0,1,%d,%d)", p/256, p%256)
		case "MLSD":
			s.mu.Lock()
			fail, drop := s.failList[arg], s.dropList[arg]
			s.mu.Unlock()
			if drop {
				return
			}
			lines, ok := s.children(arg)
			if fail || !ok || pasv == nil {
				send("550 Cannot list %s", arg)
				continue
			}
			send("150 File status okay; about to open data connection.")
			dc, err := data()
			if err != nil {
				return
			}
			fmt.Fprint(dc, strings.Join(append(lines, ""), "\r\n"))
			dc.Close()
			send("226 Closing data connection.")
		case "MLST":
			s.mu.Lock()
			f := s.files[arg]
			s.mu.Unlock()
			if f == nil {
				send("550 not found")
				continue
			}
			send("250-Listing")
			send(" %s", facts("file", len(f.data), f.mtime, arg))
			send("250 End")
		case "RETR":
			s.mu.Lock()
			f, drop := s.files[arg], s.dropRetr[arg]
			hook := s.onRetr
			if f != nil {
				s.retrs[arg]++
			}
			s.mu.Unlock()
			if f == nil || pasv == nil {
				send("550 not found")
				continue
			}
			send("150 File status okay; about to open data connection.")
			dc, err := data()
			if err != nil {
				return
			}
			if drop {
				dc.Write(f.data[:len(f.data)/2])
				dc.Close()
				return // hang up the control connection too
			}
			dc.Write(f.data)
			dc.Close()
			if hook != nil {
				hook(arg)
			}
			send("226 Closing data connection.")
		default:
			send("502 Command not implemented.")
		}
	}
}
