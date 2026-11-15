// Package kerneljudge makes the Linux kernel, and the filesystem's own native
// tools, the judge of a go-filesystems driver that has no independent oracle.
//
// A driver's test calls Run (and RunPerf) behind FS_KERNEL_JUDGE=1. The test
// binary is built for linux/arm64 and run as root inside a Tart VM by the
// kerneljudge command (see cmd/kerneljudge); on any other machine, or without
// the variable, the judge skips and says why.
//
// The truth is never the driver: it is a deterministic Corpus. Every image,
// whoever wrote it, is checked against the Corpus, so a driver that is wrong in
// the same way on both sides of its own round trip is still caught.
package kerneljudge

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"sort"
	"strings"
	"time"
)

// Kind is the type of a corpus entry.
type Kind uint8

// The kinds a corpus holds.
const (
	File Kind = iota + 1
	Dir
	Symlink
)

func (k Kind) String() string {
	switch k {
	case File:
		return "file"
	case Dir:
		return "dir"
	case Symlink:
		return "symlink"
	}
	return "?"
}

// Entry is one object of a corpus. Path is absolute inside the filesystem.
type Entry struct {
	Path   string
	Kind   Kind
	Perm   fs.FileMode // permission bits; files and directories
	Size   int64       // logical size of a file
	Head   int         // bytes of seeded data at the start of a file; the rest is a hole. 0 = the whole file is data
	Seed   uint64
	Target string // symlink target
	Mtime  time.Time
	Xattrs map[string]string
}

// Caps says which optional parts of a corpus a writer can honour.
type Caps struct {
	Symlinks bool
	Sparse   bool // Truncate can grow a file into a hole
	Mtime    bool // Chtimes
	Xattrs   bool
}

// Options sizes a corpus.
type Options struct {
	LargeBytes  int64 // the large file; default 32 MiB
	SparseBytes int64 // logical size of the sparse file; default 16 MiB
	SmallFiles  int   // extra small files spread over directories, for the metadata walk
	SizeAlign   int64 // round every non-empty file size up to a multiple of this (0 = exact sizes)
	Caps        Caps
}

// Content returns the logical bytes of a file entry.
func (e Entry) Content() []byte {
	b := make([]byte, e.Size)
	n := int(e.Size)
	if e.Head > 0 && e.Head < n {
		n = e.Head
	}
	fill(b[:n], e.Seed)
	return b
}

// DataLen is how many leading bytes of the file are real data.
func (e Entry) DataLen() int64 {
	if e.Head > 0 && int64(e.Head) < e.Size {
		return int64(e.Head)
	}
	return e.Size
}

func fill(b []byte, seed uint64) {
	r := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	for i := 0; i+8 <= len(b); i += 8 {
		v := r.Uint64()
		for j := 0; j < 8; j++ {
			b[i+j] = byte(v >> (8 * j))
		}
	}
	if rem := len(b) % 8; rem != 0 {
		v := r.Uint64()
		for j := 0; j < rem; j++ {
			b[len(b)-rem+j] = byte(v >> (8 * j))
		}
	}
}

// baseTime is even to the second (exFAT stores two-second pairs) and inside
// every format's range.
var baseTime = time.Date(2021, 3, 4, 5, 6, 8, 0, time.UTC)

// Build returns the corpus: directories first, parents before children.
func Build(o Options) []Entry {
	if o.LargeBytes == 0 {
		o.LargeBytes = 32 << 20
	}
	if o.SparseBytes == 0 {
		o.SparseBytes = 16 << 20
	}
	var es []Entry
	seed := uint64(1)
	dir := func(p string, perm fs.FileMode) {
		es = append(es, Entry{Path: p, Kind: Dir, Perm: perm})
	}
	file := func(p string, size int64, perm fs.FileMode) {
		seed++
		if a := o.SizeAlign; a > 0 && size > 0 {
			size = (size + a - 1) / a * a
		}
		e := Entry{Path: p, Kind: File, Perm: perm, Size: size, Seed: seed}
		if o.Caps.Mtime {
			e.Mtime = baseTime.Add(time.Duration(seed) * 3600 * time.Second)
		}
		es = append(es, e)
	}
	dir("/docs", 0o755)
	dir("/docs/nested", 0o755)
	dir("/docs/nested/deep", 0o700)
	dir("/données", 0o755)
	dir("/日本語", 0o755)
	dir("/emoji-📁", 0o755)
	dir("/spaces and (parens)", 0o755)
	file("/empty", 0, 0o644)
	file("/one", 1, 0o644)
	file("/docs/a.txt", 511, 0o644)
	file("/docs/b.txt", 4096, 0o600)
	file("/docs/c.txt", 4097, 0o755)
	file("/docs/nested/deep/e.bin", 65537, 0o644)
	file("/données/café.txt", 1000, 0o644)
	file("/日本語/ファイル.txt", 3000, 0o644)
	file("/emoji-📁/😀.bin", 12345, 0o644)
	file("/spaces and (parens)/a b.txt", 77, 0o644)
	file("/medium.bin", 1<<20, 0o644)
	file("/large.bin", o.LargeBytes, 0o644)
	if o.Caps.Sparse {
		seed++
		e := Entry{Path: "/sparse.bin", Kind: File, Perm: 0o644, Size: o.SparseBytes, Head: 4096, Seed: seed}
		if o.Caps.Mtime {
			e.Mtime = baseTime.Add(time.Hour)
		}
		es = append(es, e)
	}
	if o.Caps.Symlinks {
		es = append(es,
			Entry{Path: "/link-rel", Kind: Symlink, Target: "docs/a.txt"},
			Entry{Path: "/link-abs", Kind: Symlink, Target: "/docs/a.txt"},
			Entry{Path: "/link-dangling", Kind: Symlink, Target: "missing/nowhere"},
			Entry{Path: "/docs/link-long", Kind: Symlink, Target: "../" + strings.Repeat("d/", 90) + "x"},
		)
	}
	if o.Caps.Xattrs {
		for i := range es {
			switch es[i].Path {
			case "/docs/a.txt":
				es[i].Xattrs = map[string]string{"user.kj": "value", "user.empty": ""}
			case "/docs/b.txt":
				es[i].Xattrs = map[string]string{"user.big": strings.Repeat("x", 600)}
			}
		}
	}
	if o.SmallFiles > 0 {
		dirs := o.SmallFiles/100 + 1
		for d := 0; d < dirs; d++ {
			dir(fmt.Sprintf("/small/d%03d", d), 0o755)
		}
		// "/small" must precede its children.
		es = append([]Entry{{Path: "/small", Kind: Dir, Perm: 0o755}}, es...)
		r := rand.New(rand.NewPCG(7, 7))
		for i := 0; i < o.SmallFiles; i++ {
			file(fmt.Sprintf("/small/d%03d/f%05d", i%dirs, i), int64(1+r.IntN(600)), 0o644)
		}
	}
	sort.SliceStable(es, func(i, j int) bool { return depth(es[i].Path) < depth(es[j].Path) })
	return es
}

func depth(p string) int { return strings.Count(p, "/") }

// Node is what one side says a path is.
type Node struct {
	Path   string
	Kind   Kind
	Perm   fs.FileMode
	Size   int64
	SHA256 [32]byte // files
	Target string   // symlinks
	Mtime  time.Time
	Xattrs map[string]string
}

// Expected is the truth: the nodes a corpus must yield from any reader.
func Expected(es []Entry) []Node {
	ns := make([]Node, 0, len(es))
	for _, e := range es {
		n := Node{Path: e.Path, Kind: e.Kind, Perm: e.Perm, Target: e.Target, Mtime: e.Mtime, Xattrs: e.Xattrs}
		if e.Kind == File {
			n.Size = e.Size
			n.SHA256 = sha256.Sum256(e.Content())
		}
		ns = append(ns, n)
	}
	return ns
}
