package ftpx

import "testing"

func TestParseFacts(t *testing.T) {
	e, ok := ParseFacts("type=file;size=255;modify=20261109094844; my file; v2.prg")
	if !ok || e.Dir || e.Size != 255 || e.Name != "my file; v2.prg" || e.ModTime.Unix() != 1794217724 {
		t.Fatalf("got %+v ok=%v", e, ok)
	}
	if e, ok := ParseFacts("type=dir;size=0;modify=19800101000000; SD"); !ok || !e.Dir {
		t.Fatalf("dir: %+v", e)
	}
	for _, l := range []string{"type=cdir;modify=19800101000000; .", "type=pdir;modify=1; ..", "garbage", ""} {
		if _, ok := ParseFacts(l); ok {
			t.Errorf("%q should be skipped", l)
		}
	}
}
