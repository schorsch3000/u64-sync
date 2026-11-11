package mirror

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPathRejectsTraversal(t *testing.T) {
	m := &Mirror{Current: "/x/current"}
	if p, err := m.Path("/SD", "a/b.prg"); err != nil || p != "/x/current/SD/a/b.prg" {
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
