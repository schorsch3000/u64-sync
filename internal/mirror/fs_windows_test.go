//go:build windows

package mirror

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFsIDSameVolume(t *testing.T) {
	d := t.TempDir()
	sub := filepath.Join(d, "sub")
	os.Mkdir(sub, 0o755)
	if err := CheckSameFS(d, sub); err != nil {
		t.Fatal(err)
	}
	if err := CheckSameFS(d, filepath.Join(d, "missing")); err == nil {
		t.Fatal("missing dir accepted")
	}
}

// Names a C64 can have but Windows cannot store must round-trip through the
// real file system under their escaped names.
func TestUnstorableNamesAreWritable(t *testing.T) {
	d := t.TempDir()
	m := &Mirror{Current: filepath.Join(d, "cur"), Tmp: filepath.Join(d, "tmp")}
	os.MkdirAll(m.Tmp, 0o755)
	names := []string{"a?b*c.prg", "Q:Z.d64", `say "hi".txt`, "<x>|y", "CON", "nul.txt", "COM1", "trailing.", "50%off", "dir?/inner.prg"}
	seen := map[string]string{}
	for _, n := range names {
		target, err := m.Path("/SD", n)
		if err != nil {
			t.Fatalf("%q: %v", n, err)
		}
		if other, dup := seen[target]; dup {
			t.Fatalf("%q and %q map to the same local file", n, other)
		}
		seen[target] = n
		rel, _ := filepath.Rel(m.Current, target)
		if strings.ContainsAny(rel, `?*:"<>|`) {
			t.Fatalf("%q still has forbidden chars: %s", n, rel)
		}
		f, err := m.NewTemp()
		if err != nil {
			t.Fatal(err)
		}
		f.WriteString(n)
		if err := m.Install(f, target, time.Unix(1000, 0)); err != nil {
			t.Fatalf("install %q as %s: %v", n, target, err)
		}
		if b, err := os.ReadFile(target); err != nil || string(b) != n {
			t.Fatalf("read back %q: %q %v", n, b, err)
		}
		if err := m.Remove("/SD", n); err != nil {
			t.Fatalf("remove %q: %v", n, err)
		}
	}
}

func TestHardlinkOnVolume(t *testing.T) {
	d := t.TempDir()
	a := filepath.Join(d, "a")
	os.WriteFile(a, []byte("x"), 0o644)
	b := filepath.Join(d, "b")
	if err := os.Link(a, b); err != nil {
		t.Fatalf("hardlinks unsupported here (snapshots need NTFS): %v", err)
	}
	sa, _ := os.Stat(a)
	sb, _ := os.Stat(b)
	if !os.SameFile(sa, sb) {
		t.Fatal("not the same file")
	}
}
