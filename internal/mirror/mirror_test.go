package mirror

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPathRejectsTraversal(t *testing.T) {
	m := &Mirror{Current: "/x/current"}
	if p, err := m.Path("/SD", "a/b.prg"); err != nil || p != filepath.Join("/x/current", "SD", "a", "b.prg") {
		t.Fatalf("%q %v", p, err)
	}
	for _, rel := range []string{"../etc", "a/../../b", "", "a//b", "a/./b"} {
		if _, err := m.Path("/SD", rel); err == nil {
			t.Errorf("%q accepted", rel)
		}
	}
}

// Install must replace the inode so hardlinked snapshots keep old content.
func TestInstallKeepsHardlinkedOldVersion(t *testing.T) {
	d := t.TempDir()
	m := &Mirror{Current: filepath.Join(d, "cur"), Tmp: filepath.Join(d, "tmp")}
	os.MkdirAll(m.Tmp, 0o755)
	target, _ := m.Path("/SD", "a/f.prg")
	write := func(s string, mt time.Time) {
		f, _ := m.NewTemp()
		f.WriteString(s)
		if err := m.Install(f, target, mt); err != nil {
			t.Fatal(err)
		}
	}
	write("old", time.Unix(1000, 0))
	snap := filepath.Join(d, "snap")
	if err := os.Link(target, snap); err != nil {
		t.Fatal(err)
	}
	write("new", time.Unix(2000, 0))
	if b, _ := os.ReadFile(snap); string(b) != "old" {
		t.Fatalf("snapshot changed: %q", b)
	}
	st, _ := os.Stat(target)
	if b, _ := os.ReadFile(target); string(b) != "new" || st.ModTime().Unix() != 2000 {
		t.Fatalf("target wrong: %q %v", b, st.ModTime())
	}
	if err := m.Remove("/SD", "a/f.prg"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(m.Current, "SD")); !os.IsNotExist(err) {
		t.Fatal("empty parents not pruned")
	}
}

func TestEscapeWindows(t *testing.T) {
	for in, want := range map[string]string{
		"GAME.PRG":    "GAME.PRG",
		"a:b?c*.d64":  "a%3Ab%3Fc%2A.d64",
		"50%off":      "50%25off",
		"name. ":      "name.%20",
		"end.":        "end%2E",
		"CON":         "%43ON",
		"nul.txt":     "%6Eul.txt",
		"COM1":        "%43OM1",
		"console.prg": "console.prg",
		"a\x01b":      "a%01b",
	} {
		if got := escapeWindows(in); got != want {
			t.Errorf("escapeWindows(%q) = %q, want %q", in, got, want)
		}
	}
}

// The copier must be able to replace a file that a download, zip or verify
// pass currently has open (needs FILE_SHARE_DELETE on Windows). The reader keeps
// seeing the old content, the target gets the new one.
func TestInstallWhileFileOpen(t *testing.T) {
	d := t.TempDir()
	m := &Mirror{Current: filepath.Join(d, "cur"), Tmp: filepath.Join(d, "tmp")}
	os.MkdirAll(m.Tmp, 0o755)
	target, _ := m.Path("/SD", "f.prg")
	install := func(s string) error {
		f, _ := m.NewTemp()
		f.WriteString(s)
		return m.Install(f, target, time.Unix(1000, 0))
	}
	if err := install("old"); err != nil {
		t.Fatal(err)
	}
	r, err := OpenShared(target)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := install("new"); err != nil {
		t.Fatalf("replace while open: %v", err)
	}
	if b, _ := io.ReadAll(r); string(b) != "old" {
		t.Fatalf("open reader saw %q", b)
	}
	if b, _ := os.ReadFile(target); string(b) != "new" {
		t.Fatalf("target has %q", b)
	}
}
