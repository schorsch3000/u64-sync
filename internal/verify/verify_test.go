package verify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/schorsch3000/u64-sync/internal/config"
	"github.com/schorsch3000/u64-sync/internal/mirror"
	"github.com/schorsch3000/u64-sync/internal/store"
)

func setup(t *testing.T) (*Verifier, *store.Store, *mirror.Mirror) {
	d := t.TempDir()
	st, err := store.Open(filepath.Join(d, "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	mir := &mirror.Mirror{Current: filepath.Join(d, "cur")}
	cfg := config.Default().Verify
	return New(cfg, st, mir, make(chan struct{}, 1)), st, mir
}

func backup(t *testing.T, st *store.Store, mir *mirror.Mirror, content string) (store.File, string) {
	f := store.File{Root: "/SD", Path: "a/b.prg", Size: int64(len(content)), MTime: 1000}
	if err := st.Upsert([]store.File{f}, 1); err != nil {
		t.Fatal(err)
	}
	p, _ := mir.Path(f.Root, f.Path)
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, []byte(content), 0o644)
	sum := sha256.Sum256([]byte(content))
	if err := st.MarkBackedUp(f, hex.EncodeToString(sum[:])); err != nil {
		t.Fatal(err)
	}
	return f, p
}

func pending(t *testing.T, st *store.Store) bool {
	_, ok, err := st.NextPending()
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

func TestVerifyOKThenCorruptThenMissing(t *testing.T) {
	v, st, mir := setup(t)
	_, p := backup(t, st, mir, "hello")
	ctx := context.Background()
	old := time.Now().Add(time.Hour) // make everything "due"

	b, _ := st.VerifyBatch(10, old)
	v.check(ctx, nil, b[0])
	if pending(t, st) {
		t.Fatal("intact file flagged")
	}

	os.WriteFile(p, []byte("hellp"), 0o644) // same size, flipped byte
	b, _ = st.VerifyBatch(10, old)
	v.check(ctx, nil, b[0])
	if !pending(t, st) {
		t.Fatal("corrupt file not re-queued")
	}
	if v.Status().Repaired != 1 {
		t.Fatalf("status: %+v", v.Status())
	}
	// A crawl must not un-queue it again.
	st.Upsert([]store.File{{Root: "/SD", Path: "a/b.prg", Size: 5, MTime: 1000}}, 2)
	if !pending(t, st) {
		t.Fatal("crawl cleared the repair request")
	}

	// re-download fixes it
	os.WriteFile(p, []byte("hello"), 0o644)
	sum := sha256.Sum256([]byte("hello"))
	st.MarkBackedUp(store.File{Root: "/SD", Path: "a/b.prg", Size: 5, MTime: 1000}, hex.EncodeToString(sum[:]))
	os.Remove(p)
	b, _ = st.VerifyBatch(10, old)
	v.check(ctx, nil, b[0])
	if !pending(t, st) {
		t.Fatal("missing file not re-queued")
	}
}
