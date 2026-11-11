// Package ftpx is a small FTP client for the Ultimate64 FTP server:
// MLSD/MLST for metadata, passive RETR for content. Nothing else is needed.
package ftpx

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/textproto"
	"strconv"
	"strings"
	"time"
)

// ReplyError is a negative reply from the server (e.g. 550 not found).
// Anything else returned by Client methods is a connection problem.
type ReplyError struct {
	Code int
	Msg  string
}

func (e *ReplyError) Error() string { return fmt.Sprintf("ftp %d %s", e.Code, e.Msg) }

type Entry struct {
	Name    string
	Dir     bool
	Size    int64
	ModTime time.Time // device local time, interpreted as UTC (FAT has no zone)
}

type Client struct {
	host    string
	nc      net.Conn
	tc      *textproto.Conn
	timeout time.Duration
	// Pace, when set, is called before each command that touches the device.
	Pace func(ctx context.Context) error
}

func Dial(ctx context.Context, host string, port int, user, pass string) (*Client, error) {
	d := net.Dialer{Timeout: 5 * time.Second}
	nc, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return nil, err
	}
	c := &Client{host: host, nc: nc, tc: textproto.NewConn(nc), timeout: 30 * time.Second}
	if code, msg, err := c.reply(); err != nil || code != 220 {
		nc.Close()
		return nil, firstErr(err, &ReplyError{code, msg})
	}
	code, msg, err := c.cmd("USER %s", user)
	if err == nil && code == 331 {
		code, msg, err = c.cmd("PASS %s", pass)
	}
	if err != nil || code != 230 {
		nc.Close()
		return nil, firstErr(err, &ReplyError{code, msg})
	}
	if code, msg, err := c.cmd("TYPE I"); err != nil || code != 200 {
		nc.Close()
		return nil, firstErr(err, &ReplyError{code, msg})
	}
	return c, nil
}

func firstErr(a, b error) error {
	if a != nil {
		return a
	}
	return b
}

func (c *Client) Close() {
	if c.nc != nil {
		c.nc.Close()
	}
}

func (c *Client) reply() (int, string, error) {
	c.nc.SetDeadline(time.Now().Add(c.timeout))
	code, msg, err := c.tc.ReadResponse(0)
	if tpe, ok := err.(*textproto.Error); ok {
		return tpe.Code, tpe.Msg, nil
	}
	return code, msg, err
}

func (c *Client) cmd(format string, args ...any) (int, string, error) {
	c.nc.SetDeadline(time.Now().Add(c.timeout))
	if err := c.tc.PrintfLine(format, args...); err != nil {
		return 0, "", err
	}
	return c.reply()
}

func (c *Client) pace(ctx context.Context) error {
	if c.Pace != nil {
		return c.Pace(ctx)
	}
	return ctx.Err()
}

func (c *Client) pasv() (net.Conn, error) {
	code, msg, err := c.cmd("PASV")
	if err != nil {
		return nil, err
	}
	if code != 227 {
		return nil, &ReplyError{code, msg}
	}
	i, j := strings.Index(msg, "("), strings.Index(msg, ")")
	if i < 0 || j < i {
		return nil, fmt.Errorf("bad PASV reply %q", msg)
	}
	p := strings.Split(msg[i+1:j], ",")
	if len(p) != 6 {
		return nil, fmt.Errorf("bad PASV reply %q", msg)
	}
	hi, e1 := strconv.Atoi(strings.TrimSpace(p[4]))
	lo, e2 := strconv.Atoi(strings.TrimSpace(p[5]))
	if e1 != nil || e2 != nil {
		return nil, fmt.Errorf("bad PASV reply %q", msg)
	}
	// The advertised IP is ignored; we always talk to the host we dialed.
	return net.DialTimeout("tcp", net.JoinHostPort(c.host, strconv.Itoa(hi*256+lo)), 10*time.Second)
}

// transfer runs a data command and hands the data stream to fn. On any error
// other than a clean ReplyError before the transfer, the control connection is
// closed because its state is unknown; callers must redial.
func (c *Client) transfer(ctx context.Context, fn func(io.Reader) error, format string, args ...any) error {
	if err := c.pace(ctx); err != nil {
		return err
	}
	dc, err := c.pasv()
	if err != nil {
		if _, ok := err.(*ReplyError); !ok {
			c.Close()
		}
		return err
	}
	defer dc.Close()
	code, msg, err := c.cmd(format, args...)
	if err != nil {
		c.Close()
		return err
	}
	if code >= 400 {
		return &ReplyError{code, msg}
	}
	dr := &deadlineReader{c: dc, d: c.timeout}
	if err := fn(dr); err != nil {
		c.Close()
		return err
	}
	dc.Close()
	code, msg, err = c.reply()
	if err != nil {
		c.Close()
		return err
	}
	if code >= 400 {
		return &ReplyError{code, msg}
	}
	return nil
}

// deadlineReader refreshes the read deadline per read: only stalls time out, not slow transfers.
type deadlineReader struct {
	c net.Conn
	d time.Duration
}

func (r *deadlineReader) Read(p []byte) (int, error) {
	r.c.SetReadDeadline(time.Now().Add(r.d))
	return r.c.Read(p)
}

// List returns the entries of a directory using MLSD. Only files and dirs are returned.
func (c *Client) List(ctx context.Context, dir string) ([]Entry, error) {
	var out []Entry
	err := c.transfer(ctx, func(r io.Reader) error {
		b, err := io.ReadAll(io.LimitReader(r, 64<<20))
		if err != nil {
			return err
		}
		for _, line := range strings.Split(string(b), "\n") {
			if e, ok := ParseFacts(strings.TrimRight(line, "\r")); ok {
				out = append(out, e)
			}
		}
		return nil
	}, "MLSD %s", dir)
	return out, err
}

// Stat returns metadata of a single file or directory using MLST.
func (c *Client) Stat(ctx context.Context, p string) (Entry, error) {
	if err := c.pace(ctx); err != nil {
		return Entry{}, err
	}
	code, msg, err := c.cmd("MLST %s", p)
	if err != nil {
		c.Close()
		return Entry{}, err
	}
	if code >= 400 {
		return Entry{}, &ReplyError{code, msg}
	}
	for _, line := range strings.Split(msg, "\n") {
		if e, ok := ParseFacts(strings.TrimRight(strings.TrimPrefix(line, " "), "\r")); ok {
			return e, nil
		}
	}
	return Entry{}, fmt.Errorf("MLST %s: no entry in reply", p)
}

// Retrieve downloads a file; fn consumes the stream.
func (c *Client) Retrieve(ctx context.Context, p string, fn func(io.Reader) error) error {
	return c.transfer(ctx, fn, "RETR %s", p)
}

// ParseFacts parses one MLSD/MLST line: "type=file;size=1;modify=20260101000000; name".
func ParseFacts(line string) (Entry, bool) {
	i := strings.IndexByte(line, ' ')
	if i < 0 {
		return Entry{}, false
	}
	var e Entry
	e.Name = line[i+1:]
	typ := ""
	for _, f := range strings.Split(line[:i], ";") {
		k, v, ok := strings.Cut(f, "=")
		if !ok {
			continue
		}
		switch strings.ToLower(k) {
		case "type":
			typ = strings.ToLower(v)
		case "size":
			e.Size, _ = strconv.ParseInt(v, 10, 64)
		case "modify":
			if t, err := time.Parse("20060102150405", strings.SplitN(v, ".", 2)[0]); err == nil {
				e.ModTime = t
			}
		}
	}
	switch typ {
	case "file":
	case "dir":
		e.Dir = true
	default:
		return Entry{}, false // cdir, pdir, unknown
	}
	if e.Name == "" || e.Name == "." || e.Name == ".." {
		return Entry{}, false
	}
	return e, true
}
