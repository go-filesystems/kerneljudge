package kerneljudge

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestCorpusIsDeterministicAndATree(t *testing.T) {
	o := Options{Caps: Caps{Symlinks: true, Sparse: true, Mtime: true, Xattrs: true}, LargeBytes: 1 << 20, SparseBytes: 1 << 20, SmallFiles: 250}
	a, b := Build(o), Build(o)
	if len(a) != len(b) {
		t.Fatalf("two builds differ in length: %d, %d", len(a), len(b))
	}
	seen := map[string]bool{"/": true}
	for i := range a {
		if a[i].Path != b[i].Path || a[i].Seed != b[i].Seed {
			t.Fatalf("entry %d differs between builds", i)
		}
		parent := a[i].Path[:strings.LastIndex(a[i].Path, "/")]
		if parent == "" {
			parent = "/"
		}
		if !seen[parent] {
			t.Errorf("%s comes before its parent %s", a[i].Path, parent)
		}
		if seen[a[i].Path] {
			t.Errorf("%s twice", a[i].Path)
		}
		if a[i].Kind == Dir {
			seen[a[i].Path] = true
		}
	}
	for _, e := range a {
		if e.Kind == File && !bytes.Equal(e.Content(), e.Content()) {
			t.Fatalf("%s: content is not stable", e.Path)
		}
	}
}

func TestCorpusHonoursCaps(t *testing.T) {
	for _, e := range Build(Options{}) {
		if e.Kind == Symlink || len(e.Xattrs) > 0 || !e.Mtime.IsZero() || (e.Kind == File && e.Head > 0) {
			t.Errorf("%s: a capability the writer lacks was asked of it", e.Path)
		}
	}
	aligned := Build(Options{SizeAlign: 4096})
	for _, e := range aligned {
		if e.Kind == File && e.Size%4096 != 0 {
			t.Errorf("%s: size %d not aligned", e.Path, e.Size)
		}
	}
}

func TestSparseEntryHasAHole(t *testing.T) {
	var sp Entry
	for _, e := range Build(Options{Caps: Caps{Sparse: true}, SparseBytes: 1 << 20}) {
		if e.Path == "/sparse.bin" {
			sp = e
		}
	}
	c := sp.Content()
	if int64(len(c)) != sp.Size || sp.DataLen() != 4096 || bytes.Count(c[4096:], []byte{0}) != len(c)-4096 || bytes.Count(c[:4096], []byte{0}) > 100 {
		t.Fatalf("sparse entry: size %d datalen %d", len(c), sp.DataLen())
	}
}

func TestDiffNamesEveryDifference(t *testing.T) {
	sum := sha256.Sum256([]byte("a"))
	other := sha256.Sum256([]byte("b"))
	when := time.Date(2021, 3, 4, 5, 6, 8, 0, time.UTC)
	want := []Node{
		{Path: "/d", Kind: Dir, Perm: 0o755},
		{Path: "/f", Kind: File, Perm: 0o644, Size: 1, SHA256: sum, Mtime: when, Xattrs: map[string]string{"user.a": "x"}},
		{Path: "/l", Kind: Symlink, Target: "f"},
		{Path: "/gone", Kind: File},
		{Path: "/kind", Kind: File},
	}
	got := []Node{
		{Path: "/d", Kind: Dir, Perm: 0o700},
		{Path: "/f", Kind: File, Perm: 0o644, Size: 2, SHA256: other, Mtime: when.Add(time.Hour)},
		{Path: "/l", Kind: Symlink, Target: "g"},
		{Path: "/kind", Kind: Dir},
		{Path: "/extra", Kind: File},
	}
	out := strings.Join(Diff(want, got, Compare{Mtime: true, Xattrs: true, Perm: true}), "\n")
	for _, s := range []string{"/d: perm want 0755 got 0700", "/f: size want 1 got 2", "/f: sha256", "/f: mtime", "/f: xattr user.a missing", `/l: link target want "f" got "g"`, "/gone: missing", "/kind: kind want file got dir", "/extra: unexpected file"} {
		if !strings.Contains(out, s) {
			t.Errorf("no line %q in:\n%s", s, out)
		}
	}
	if d := Diff(want, want, Compare{Mtime: true, Xattrs: true, Perm: true}); len(d) != 0 {
		t.Errorf("a snapshot differs from itself: %v", d)
	}
	// Without the metadata flags only kind, size, content and targets count.
	if strings.Contains(strings.Join(Diff(want, got, Compare{}), "\n"), "perm") {
		t.Error("perm compared although not asked")
	}
	// Two seconds of slack is the default for times.
	near := []Node{{Path: "/f", Kind: File, Perm: 0o644, Size: 1, SHA256: sum, Mtime: when.Add(time.Second)}}
	if d := Diff(want[1:2], near, Compare{Mtime: true}); len(d) != 0 {
		t.Errorf("one second apart is within tolerance: %v", d)
	}
}

func TestPopulateAndSnapshotDirRoundTrip(t *testing.T) {
	caps := Caps{Symlinks: true, Sparse: true, Mtime: true, Xattrs: runtime.GOOS == "linux"}
	es := Build(Options{Caps: caps, LargeBytes: 1 << 20, SparseBytes: 1 << 20, SmallFiles: 30})
	root := t.TempDir()
	if err := PopulateDir(root, es); err != nil {
		if caps.Xattrs && strings.Contains(err.Error(), "xattr") {
			t.Skipf("this filesystem has no user xattrs: %v", err)
		}
		t.Fatal(err)
	}
	got, err := SnapshotDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if d := Diff(Expected(es), got, Compare{Mtime: true, Xattrs: caps.Xattrs, Perm: false}); len(d) != 0 {
		t.Fatalf("the host filesystem disagrees with the corpus:\n%s", strings.Join(d, "\n"))
	}
}

func TestSubst(t *testing.T) {
	got := subst([]string{"mksquashfs", "{src}", "{img}", "x{img}"}, "/i", "/s")
	if strings.Join(got, " ") != "mksquashfs /s /i x/i" {
		t.Fatalf("%v", got)
	}
}

func TestKnownMatchesTestAndLine(t *testing.T) {
	k := Known{Test: `/write$`, Match: `^fsck .*bad file extent`}
	if !k.matches("TestKernelJudge/write", "fsck btrfs: a\nb bad file extent") {
		t.Error("multi-line message not matched")
	}
	if k.matches("TestKernelJudge/write-aligned", "fsck btrfs: bad file extent") {
		t.Error("another subtest matched")
	}
	if k.matches("TestKernelJudge/write", "kernel mount: bad file extent") {
		t.Error("another step matched")
	}
}

func TestJudgeSkipsWithoutTheVariable(t *testing.T) {
	if os.Getenv(EnvJudge) == "1" {
		t.Skip("the judge is on")
	}
	ran := false
	t.Run("inner", func(t *testing.T) {
		Enabled(t)
		ran = true
	})
	if ran {
		t.Fatal("Enabled did not skip")
	}
}

func TestMedian(t *testing.T) {
	if m := (Result{Rate: []float64{9, 1, 5}}).Median(); m != 5 {
		t.Fatalf("median %v", m)
	}
}

func TestMapRenamesEveryRunsCorpus(t *testing.T) {
	up := func(e Entry) (Entry, bool) {
		e.Path = strings.ToUpper(e.Path)
		return e, !strings.HasPrefix(e.Path, "/DOCS/NESTED")
	}
	for _, e := range Build(Options{Map: up}) {
		if e.Path != strings.ToUpper(e.Path) || strings.HasPrefix(e.Path, "/DOCS/NESTED") {
			t.Errorf("%s survived the map", e.Path)
		}
	}
}

func TestDiffFoldCaseComparesLikeTheLinuxIsoDriver(t *testing.T) {
	want := []Node{{Path: "/docs", Kind: Dir}, {Path: "/docs/a.txt", Kind: File}}
	got := []Node{{Path: "/DOCS", Kind: Dir}, {Path: "/DOCS/A.TXT;1", Kind: File}}
	if d := Diff(want, got, Compare{}); len(d) == 0 {
		t.Fatal("without FoldCase the names differ")
	}
	if d := Diff(want, got, Compare{FoldCase: true}); len(d) != 0 {
		t.Fatalf("with FoldCase: %v", d)
	}
}

func TestParseOleLines(t *testing.T) {
	p := fmt.Sprintf("%x", "dir/é.txt")
	h := sha256.Sum256([]byte("abc"))
	out := []byte("d\t" + fmt.Sprintf("%x", "dir") + "\t0\t-\n" + "f\t" + p + "\t3\t" + fmt.Sprintf("%x", h) + "\n")
	ns, err := parseOle(out)
	if err != nil || len(ns) != 2 {
		t.Fatalf("%v %v", ns, err)
	}
	if ns[0].Kind != Dir || ns[0].Path != "/dir" || ns[1].Kind != File || ns[1].Path != "/dir/é.txt" || ns[1].Size != 3 || ns[1].SHA256 != h {
		t.Fatalf("%+v", ns)
	}
	if _, err := parseOle([]byte("f\tzz\t1\t00\n")); err == nil {
		t.Error("a bad path parsed")
	}
	if _, err := parseOle([]byte("x\n")); err == nil {
		t.Error("a short line parsed")
	}
}

func TestMountRootOfAPoolUsesTheMountpoint(t *testing.T) {
	if got := (Driver{}).mountRoot("/m"); got != "/m" {
		t.Errorf("no pool: %s", got)
	}
}

func TestFoldLookupFindsTheKernelsSpelling(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "small", "d000"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "large.bin;1"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := foldLookup(root, "/LARGE.BIN"); got != filepath.Join(root, "large.bin;1") {
		t.Errorf("file: %s", got)
	}
	if got := foldLookup(root, "/SMALL/D000"); got != filepath.Join(root, "small", "d000") {
		t.Errorf("dirs: %s", got)
	}
	if got := foldLookup(root, "/MISSING"); got != filepath.Join(root, "/MISSING") {
		t.Errorf("missing: %s", got)
	}
}
