package kerneljudge

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// EnvExtra names a directory (in the VM) of existing files the external judge
// also compares, reader against oracle. They are copied in for the run and
// removed after it; the judge never writes them anywhere else.
const EnvExtra = "FS_KERNEL_JUDGE_EXTRA"

// External describes a driver whose format has no kernel filesystem (CFB). The
// judge is an independent reader (olefile) and an independent writer (gsf).
type External struct {
	Name string
	// Write makes an image from a corpus with our writer.
	Write func(img string, es []Entry) error
	// Read describes every path of an image as our reader sees it.
	Read func(img string) ([]Node, error)
	// Oracle describes an image as the independent reader sees it
	// (OleOracle by default).
	Oracle func(img string) ([]Node, error)
	// Natives write an image from a directory (Args with {img}, {src}).
	Natives []Native
	// Map renames or drops corpus entries (names the format cannot hold).
	Map func(Entry) (Entry, bool)
	// ExtraSuffix selects the files of EnvExtra to compare.
	ExtraSuffix string
	Known       []Known
}

// oleScript lists an OLE2 / CFB file with olefile: one line per storage and
// stream, "kind<TAB>hex-encoded UTF-8 path<TAB>size<TAB>sha256", where kind is
// d or f. -I (isolated mode) is used by the caller.
const oleScript = `
import sys, hashlib, olefile
o = olefile.OleFileIO(sys.argv[1])
for e in o.listdir(streams=True, storages=True):
    p = "/".join(e).encode("utf-8").hex()
    if o.get_type(e) == olefile.STGTY_STREAM:
        d = o.openstream(e).read()
        print("f\t%s\t%d\t%s" % (p, len(d), hashlib.sha256(d).hexdigest()))
    else:
        print("d\t%s\t0\t-" % p)
`

// OleOracle reads a CFB file with Python's olefile (python3 -I, so no module
// is loaded from the current directory). The script is passed with -c and
// never written to disk: a fixed name in the shared temporary directory let
// another local user pre-create it as a symlink (clobbering a file of ours)
// or swap its contents between the write and python3 reading it (running
// their code as us).
func OleOracle(img string) ([]Node, error) {
	out, err := exec.Command("python3", "-I", "-c", oleScript, img).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("olefile: %v\n%s", err, bytes.TrimSpace(out))
	}
	return parseOle(out)
}

func parseOle(out []byte) ([]Node, error) {
	var ns []Node
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		f := strings.Split(sc.Text(), "\t")
		if len(f) != 4 {
			return nil, fmt.Errorf("olefile: unexpected line %q", sc.Text())
		}
		p, err := hex.DecodeString(f[1])
		if err != nil {
			return nil, err
		}
		n := Node{Path: "/" + string(p), Kind: Dir}
		if f[0] == "f" {
			n.Kind = File
			if n.Size, err = strconv.ParseInt(f[2], 10, 64); err != nil {
				return nil, err
			}
			h, err := hex.DecodeString(f[3])
			if err != nil || len(h) != 32 {
				return nil, fmt.Errorf("olefile: bad hash %q", f[3])
			}
			copy(n.SHA256[:], h)
		}
		ns = append(ns, n)
	}
	return ns, sc.Err()
}

// RunExternal judges a driver against an independent reader and writer:
//
//   - write: our writer makes the image; the oracle and our own reader each
//     compare it with the corpus;
//   - reverse: each native writer makes the image from the same corpus; the
//     oracle and our reader compare it;
//   - extra: every file of FS_KERNEL_JUDGE_EXTRA is read by both and the two
//     trees must be equal (no expectation of content: these are foreign files).
func RunExternal(t *testing.T, x External) {
	Enabled(t)
	if x.Oracle == nil {
		x.Oracle = OleOracle
	}
	d := Driver{Name: x.Name, Known: x.Known}
	check := func(t *testing.T, img string, want []Node) {
		t.Helper()
		keep(t, img)
		got, err := x.Oracle(img)
		if err != nil {
			d.report(t, "oracle", []string{err.Error()})
		} else {
			d.report(t, "oracle vs corpus", Diff(want, got, Compare{}))
		}
		ours, err := x.Read(img)
		if err != nil {
			d.report(t, "our reader", []string{err.Error()})
		} else {
			d.report(t, "our reader vs corpus", Diff(want, ours, Compare{}))
		}
	}
	t.Run("write", func(t *testing.T) {
		es := Build(Options{Map: x.Map})
		img := filepath.Join(t.TempDir(), "ours.img")
		if err := x.Write(img, es); err != nil {
			t.Fatalf("our writer: %v", err)
		}
		check(t, img, Expected(es))
	})
	for _, n := range x.Natives {
		t.Run("reverse-"+n.Name, func(t *testing.T) {
			es := Build(Options{Map: x.Map})
			src := filepath.Join(t.TempDir(), "src")
			if err := os.Mkdir(src, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := PopulateDir(src, es); err != nil {
				t.Fatalf("populate source: %v", err)
			}
			img := filepath.Join(t.TempDir(), "native.img")
			if out, err := run(subst(n.Args, img, src)...); err != nil {
				t.Fatalf("%s refused: %v\n%s", n.Name, err, out)
			}
			check(t, img, Expected(es))
		})
	}
	if dir := os.Getenv(EnvExtra); dir != "" {
		t.Run("extra", func(t *testing.T) {
			files, _ := filepath.Glob(filepath.Join(dir, "*"+x.ExtraSuffix))
			sort.Strings(files)
			if len(files) == 0 {
				t.Fatalf("%s holds no *%s file", EnvExtra, x.ExtraSuffix)
			}
			for _, f := range files {
				t.Run(filepath.Base(f), func(t *testing.T) {
					want, err := x.Oracle(f)
					if err != nil {
						d.report(t, "oracle", []string{err.Error()})
						return
					}
					ours, err := x.Read(f)
					if err != nil {
						d.report(t, "our reader", []string{err.Error()})
						return
					}
					d.report(t, "our reader vs oracle", Diff(want, ours, Compare{}))
					t.Logf("%d entries, %d streams hashed", len(want), countFiles(want))
				})
			}
		})
	}
}

func countFiles(ns []Node) (n int) {
	for _, x := range ns {
		if x.Kind == File {
			n++
		}
	}
	return n
}
