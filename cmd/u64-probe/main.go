// u64-probe inspects an Ultimate64 FTP server and REST API and prints raw
// results, so design decisions rest on facts: listing format, MLSD support,
// connection limit, command latency, mtime precision.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/textproto"
	"os"
	"strconv"
	"strings"
	"time"
)

type conn struct {
	host string
	c    *textproto.Conn
}

func dial(host string, port int, timeout time.Duration) (*conn, string, error) {
	nc, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), timeout)
	if err != nil {
		return nil, "", err
	}
	c := textproto.NewConn(nc)
	_, msg, err := c.ReadResponse(220)
	if err != nil {
		nc.Close()
		return nil, msg, err
	}
	return &conn{host: host, c: c}, msg, nil
}

// cmd sends a command and reads a (possibly multi-line) reply.
func (c *conn) cmd(format string, args ...any) (int, string, time.Duration, error) {
	t := time.Now()
	id, err := c.c.Cmd(format, args...)
	if err != nil {
		return 0, "", 0, err
	}
	c.c.StartResponse(id)
	defer c.c.EndResponse(id)
	code, msg, err := c.c.ReadResponse(0)
	if tpe, ok := err.(*textproto.Error); ok {
		code, msg, err = tpe.Code, tpe.Msg, nil
	}
	return code, msg, time.Since(t), err
}

func (c *conn) pasv() (net.Conn, error) {
	code, msg, _, err := c.cmd("PASV")
	if err != nil {
		return nil, err
	}
	if code != 227 {
		return nil, fmt.Errorf("PASV: %d %s", code, msg)
	}
	i, j := strings.Index(msg, "("), strings.Index(msg, ")")
	if i < 0 || j < i {
		return nil, fmt.Errorf("cannot parse PASV reply %q", msg)
	}
	p := strings.Split(msg[i+1:j], ",")
	if len(p) != 6 {
		return nil, fmt.Errorf("cannot parse PASV reply %q", msg)
	}
	hi, _ := strconv.Atoi(p[4])
	lo, _ := strconv.Atoi(p[5])
	// ignore the advertised IP, use the host we connected to
	return net.DialTimeout("tcp", net.JoinHostPort(c.host, strconv.Itoa(hi*256+lo)), 10*time.Second)
}

// data runs a data-channel command and returns the raw payload.
func (c *conn) data(format string, args ...any) (string, int, string, time.Duration, error) {
	t := time.Now()
	dc, err := c.pasv()
	if err != nil {
		return "", 0, "", 0, err
	}
	defer dc.Close()
	code, msg, _, err := c.cmd(format, args...)
	if err != nil {
		return "", 0, "", 0, err
	}
	if code >= 400 {
		return "", code, msg, time.Since(t), nil
	}
	dc.SetReadDeadline(time.Now().Add(30 * time.Second))
	b, err := io.ReadAll(dc)
	dc.Close()
	if err != nil {
		return string(b), code, msg, time.Since(t), err
	}
	code2, msg2, err := c.c.ReadResponse(226)
	if tpe, ok := err.(*textproto.Error); ok {
		code2, msg2, err = tpe.Code, tpe.Msg, nil
	}
	_ = code2
	_ = msg2
	return string(b), code, msg, time.Since(t), err
}

func head(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\r\n"), "\n")
	if len(lines) > n {
		lines = append(lines[:n], fmt.Sprintf("... (%d lines total)", len(lines)))
	}
	return "    " + strings.Join(lines, "\n    ")
}

func section(s string) { fmt.Printf("\n== %s ==\n", s) }

func main() {
	host := flag.String("host", "", "U64 host or IP (required)")
	port := flag.Int("port", 21, "FTP port")
	user := flag.String("user", "anonymous", "FTP user")
	pass := flag.String("pass", os.Getenv("U64_FTP_PASS"), "FTP password (or env U64_FTP_PASS)")
	dir := flag.String("dir", "/", "directory to list")
	maxConns := flag.Int("max-conns", 6, "max parallel connections to try")
	flag.Parse()
	if *host == "" {
		fmt.Fprintln(os.Stderr, "usage: u64-probe -host <ip>")
		os.Exit(2)
	}

	section("REST API")
	hc := &http.Client{Timeout: 5 * time.Second}
	for _, p := range []string{"/v1/version", "/v1/info"} {
		t := time.Now()
		resp, err := hc.Get("http://" + *host + p)
		if err != nil {
			fmt.Printf("  GET %s: %v\n", p, err)
			continue
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		resp.Body.Close()
		fmt.Printf("  GET %s: %s (%v)\n%s\n", p, resp.Status, time.Since(t).Round(time.Millisecond), head(string(b), 10))
	}

	section("FTP connect")
	c, banner, err := dial(*host, *port, 5*time.Second)
	if err != nil {
		fmt.Println("  connect failed:", err)
		os.Exit(1)
	}
	defer c.cmd("QUIT")
	fmt.Println("  banner:", banner)
	code, msg, d, _ := c.cmd("USER %s", *user)
	fmt.Printf("  USER: %d %s (%v)\n", code, msg, d.Round(time.Millisecond))
	if code == 331 {
		code, msg, d, _ = c.cmd("PASS %s", *pass)
		fmt.Printf("  PASS: %d %s (%v)\n", code, msg, d.Round(time.Millisecond))
	}
	if code != 230 {
		fmt.Println("  login failed")
		os.Exit(1)
	}

	section("FEAT / SYST / misc")
	for _, x := range []string{"FEAT", "SYST", "PWD", "TYPE I", "OPTS UTF8 ON"} {
		code, msg, d, _ := c.cmd("%s", x)
		fmt.Printf("  %s: %d (%v)\n%s\n", x, code, d.Round(time.Millisecond), head(msg, 20))
	}

	section("LIST " + *dir)
	out, code, msg, d, err := c.data("LIST %s", *dir)
	fmt.Printf("  %d %s (%v) err=%v\n%s\n", code, msg, d.Round(time.Millisecond), err, head(out, 15))

	section("MLSD " + *dir)
	out, code, msg, d, err = c.data("MLSD %s", *dir)
	fmt.Printf("  %d %s (%v) err=%v\n%s\n", code, msg, d.Round(time.Millisecond), err, head(out, 15))

	section("MLST " + *dir)
	code, msg, d, _ = c.cmd("MLST %s", *dir)
	fmt.Printf("  %d (%v)\n%s\n", code, d.Round(time.Millisecond), head(msg, 6))

	section("MDTM / SIZE of first file in LIST")
	// pick first non-directory name from LIST for MDTM/SIZE tests
	list, _, _, _, _ := c.data("LIST %s", *dir)
	sc := bufio.NewScanner(strings.NewReader(list))
	var first string
	for sc.Scan() {
		l := sc.Text()
		if l != "" && !strings.HasPrefix(l, "d") {
			f := strings.Fields(l)
			if len(f) >= 9 {
				first = strings.Join(f[8:], " ")
				break
			}
		}
	}
	if first == "" {
		fmt.Println("  no plain file found in", *dir)
	} else {
		p := strings.TrimRight(*dir, "/") + "/" + first
		for _, x := range []string{"MDTM", "SIZE"} {
			code, msg, d, _ := c.cmd("%s %s", x, p)
			fmt.Printf("  %s %s: %d %s (%v)\n", x, p, code, msg, d.Round(time.Millisecond))
		}
	}

	section("latency: 10x NOOP")
	var total time.Duration
	for i := 0; i < 10; i++ {
		_, _, d, _ := c.cmd("NOOP")
		total += d
	}
	fmt.Printf("  avg %v\n", (total / 10).Round(time.Millisecond))

	section(fmt.Sprintf("parallel connections (up to %d, first one already open)", *maxConns))
	conns := []*conn{c}
	for i := 2; i <= *maxConns; i++ {
		x, _, err := dial(*host, *port, 5*time.Second)
		if err != nil {
			fmt.Printf("  connection %d refused: %v\n", i, err)
			break
		}
		code, msg, _, _ := x.cmd("USER %s", *user)
		if code == 331 {
			code, msg, _, _ = x.cmd("PASS %s", *pass)
		}
		if code != 230 {
			fmt.Printf("  connection %d login failed: %d %s\n", i, code, msg)
			x.c.Close()
			break
		}
		conns = append(conns, x)
		fmt.Printf("  connection %d ok\n", i)
	}
	for _, x := range conns[1:] {
		x.cmd("QUIT")
		x.c.Close()
	}
	fmt.Printf("  => %d simultaneous sessions worked\n", len(conns))
}
