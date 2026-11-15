package kerneljudge

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	filesystem "github.com/go-filesystems/interface"
)

// EnvJudge enables the judge. The kerneljudge command sets it in the VM.
const EnvJudge = "FS_KERNEL_JUDGE"

// Builder is a writer of ours that makes an image from a directory (squashfs
// has no read-write API: it builds).
type Builder struct {
	Name  string
	Build func(img, srcDir string) error
}

// Native is a writer that is not ours: a program that makes an image from a
// directory. Args hold {img} and {src}.
type Native struct {
	Name string
	Args []string
}

// Known is a defect already filed: a difference whose line ("<step>: <line>")
// matches the regular expression Match, in a test whose name matches Test (all
// tests when empty), is logged as known with its issue instead of failing the
// run. Remove the entry when the issue is fixed.
type Known struct {
	Test  string
	Match string
	Issue string
}

func (k Known) matches(test, line string) bool {
	if k.Test != "" && !regexp.MustCompile(k.Test).MatchString(test) {
		return false
	}
	return regexp.MustCompile("(?s)" + k.Match).MatchString(line)
}

// Driver describes one driver to the judge.
type Driver struct {
	Name      string // "btrfs"
	FSType    string // the kernel's name for it: "btrfs", "exfat", "squashfs"
	ImageSize int64  // default 256 MiB

	// Our reader.
	Open func(img string) (filesystem.Filesystem, error)

	// Our writer: Format (read-write drivers) and/or Builders (squashfs).
	Format   func(img string, size int64) (filesystem.Filesystem, error)
	Builders []Builder
	// BuilderCaps is what a Builder is asked to carry (default: all of it).
	BuilderCaps *Caps

	// The kernel side. Mkfs makes an empty image for the kernel to write into
	// (read-write filesystems); Natives make an image from a directory.
	Mkfs      []string
	Natives   []Native
	MountOpts string
	// FSCaps is what the on-disk format can hold at all (exFAT has no symlinks,
	// no xattrs and no permissions). Default: everything.
	FSCaps *Caps
	// Fsck commands, each with {img}; every one must exit 0.
	Fsck [][]string
	// Compare says which metadata is held to the corpus.
	Compare Compare
	// Mtime reads a file's modification time when the driver has its own
	// accessor and not filesystem.TimesReader.
	Mtime func(f filesystem.Filesystem, path string) (time.Time, error)

	Known []Known

	// Pool makes the "mount" a zpool import (read-only) or create (read-write)
	// of the image file instead of a loop mount: OpenZFS.
	Pool *Pool

	// Map changes every run's corpus (for a format with its own name rules,
	// e.g. ISO 9660 level 1: 8.3 upper-case names). It must treat a directory
	// and its children alike.
	Map func(Entry) (Entry, bool)

	// Variants add "write-<name>" runs of our writer on a changed corpus, to
	// look behind a defect that hides the rest of an image.
	Variants []Variant
}

// Variant changes the corpus of a write run.
type Variant struct {
	Name string
	// Opts changes the corpus options (sizes, capabilities).
	Opts func(*Options)
	// Map changes or drops an entry (return false to drop it). It must treat a
	// directory and its children alike, or the corpus is not a tree.
	Map func(Entry) (Entry, bool)
}

func (v Variant) corpus(base Options) []Entry {
	if v.Opts != nil {
		v.Opts(&base)
	}
	es := Build(base)
	if v.Map == nil {
		return es
	}
	out := es[:0:0]
	for _, e := range es {
		if m, ok := v.Map(e); ok {
			out = append(out, m)
		}
	}
	return out
}

func (d Driver) size() int64 {
	if d.ImageSize == 0 {
		return 256 << 20
	}
	return d.ImageSize
}

func (d Driver) fsCaps() Caps {
	if d.FSCaps != nil {
		return *d.FSCaps
	}
	return Caps{Symlinks: true, Sparse: true, Mtime: true, Xattrs: true}
}

func and(a, b Caps) Caps {
	return Caps{a.Symlinks && b.Symlinks, a.Sparse && b.Sparse, a.Mtime && b.Mtime, a.Xattrs && b.Xattrs}
}

// Enabled reports whether the judge is on, and skips t with the reason when it
// is not. The judge needs Linux, root and the native tools.
func Enabled(t *testing.T) {
	t.Helper()
	if os.Getenv(EnvJudge) != "1" {
		t.Skipf("set %s=1 (run through cmd/kerneljudge, which does it in a VM)", EnvJudge)
	}
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		t.Fatalf("%s=1 but this is not root on Linux: the judge mounts images", EnvJudge)
	}
}

func subst(args []string, img, src string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		a = strings.ReplaceAll(a, "{img}", img)
		a = strings.ReplaceAll(a, "{dir}", filepath.Dir(img))
		out[i] = strings.ReplaceAll(a, "{src}", src)
	}
	return out
}

func run(args ...string) (string, error) {
	out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func tryMount(t *testing.T, d Driver, img, mnt string, rw bool) (func(), error) {
	t.Helper()
	if err := os.MkdirAll(mnt, 0o755); err != nil {
		return nil, err
	}
	if d.Pool != nil {
		return d.poolMount(t, img, mnt, rw)
	}
	opts := "loop,ro"
	if rw {
		opts = "loop"
	}
	if d.MountOpts != "" {
		opts += "," + d.MountOpts
	}
	if out, err := run("mount", "-t", d.FSType, "-o", opts, img, mnt); err != nil {
		dm, _ := run("sh", "-c", "dmesg | tail -n 6")
		return nil, fmt.Errorf("kernel mount of %s: %v: %s\nkernel log:\n%s", filepath.Base(img), err, out, dm)
	}
	return func() {
		if out, err := run("umount", mnt); err != nil {
			t.Errorf("umount: %v\n%s", err, out)
		}
	}, nil
}

func mount(t *testing.T, d Driver, img, mnt string, rw bool) func() {
	t.Helper()
	u, err := tryMount(t, d, img, mnt, rw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func (d Driver) report(t *testing.T, step string, lines []string) {
	t.Helper()
	for _, l := range lines {
		var issue string
		for _, k := range d.Known {
			if k.matches(t.Name(), step+": "+l) {
				issue = k.Issue
				break
			}
		}
		if issue != "" {
			t.Logf("KNOWN DEFECT (%s) %s: %s", issue, step, l)
			continue
		}
		t.Errorf("%s: %s", step, l)
	}
	if len(lines) == 0 {
		t.Logf("OK   %s", step)
	}
}

// judge holds one image to the corpus three ways: the native fsck, the kernel's
// read-only mount, and our own reader.
func (d Driver) judge(t *testing.T, img string, want []Node) {
	t.Helper()
	keep(t, img)
	for _, cmd := range d.Fsck {
		args := subst(cmd, img, "")
		out, err := run(args...)
		if err != nil {
			d.report(t, "fsck "+args[0], []string{fmt.Sprintf("%s failed: %v\n%s", strings.Join(args, " "), err, out)})
		} else {
			t.Logf("OK   fsck: %s", strings.Join(args[:len(args)-1], " "))
		}
	}
	mnt := filepath.Join(t.TempDir(), "mnt")
	unmount, merr := tryMount(t, d, img, mnt, false)
	if merr != nil {
		// A loop device is a whole number of 512-byte sectors, so a kernel
		// filesystem whose superblock counts bytes up to the end of an
		// unpadded file refuses it. Retry on a padded copy so the rest of the
		// image is still judged.
		reported := false
		if fi, serr := os.Stat(img); serr == nil && fi.Size()%4096 != 0 {
			padded := img + ".padded"
			if _, cerr := run("cp", "--sparse=always", img, padded); cerr == nil {
				padSize := (fi.Size() + 4095) / 4096 * 4096
				if terr := os.Truncate(padded, padSize); terr != nil {
					t.Fatalf("pad copy: %v", terr)
				}
				u2, err2 := tryMount(t, d, padded, mnt, false)
				if err2 == nil {
					d.report(t, "kernel mount", []string{fmt.Sprintf("image size %d is not a multiple of 4096: the kernel refuses it on a loop device and mounts it once padded to %d", fi.Size(), padSize)})
					unmount, merr, reported = u2, nil, true
				} else {
					// Padding is not the whole story: say what the padded copy
					// does, it is the kernel's real objection.
					merr = fmt.Errorf("%w\nstill refused after padding to %d: %v", merr, padSize, err2)
				}
			}
		}
		if !reported {
			d.report(t, "kernel mount", []string{merr.Error()})
		}
		if merr != nil {
			t.Logf("SKIPPED kernel mount vs corpus: the image did not mount")
		}
	}
	if merr == nil {
		got, werr := SnapshotDir(d.mountRoot(mnt))
		if werr != nil {
			d.report(t, "kernel mount walk", []string{werr.Error()})
		} else {
			d.report(t, "kernel mount vs corpus", Diff(want, got, d.Compare))
		}
		unmount()
	}

	f, err := d.Open(img)
	if err != nil {
		d.report(t, "our Open", []string{err.Error()})
		return
	}
	defer f.Close()
	ours, errs := SnapshotFS(f, d.Mtime)
	d.report(t, "our reader errors", errs)
	d.report(t, "our reader vs corpus", Diff(want, ours, d.Compare))
}

// Run is the correctness judge. In one run, per driver:
//
//   - write: our writer makes the image; the native fsck, the kernel's mount
//     and our own reader each compare it with the corpus;
//   - reverse: the kernel (mkfs and a read-write mount) or the native tool
//     writes the image; the same three comparisons.
func Run(t *testing.T, d Driver) {
	Enabled(t)
	tools := append([][]string{d.Mkfs}, d.Fsck...)
	for _, n := range d.Natives {
		tools = append(tools, n.Args)
	}
	for _, tool := range tools {
		if len(tool) > 0 {
			if _, err := exec.LookPath(tool[0]); err != nil {
				t.Fatalf("%s is not installed in the judge VM: %v", tool[0], err)
			}
		}
	}
	t.Logf("kernel %s", kernelRelease())

	if d.Format != nil {
		write := func(v Variant) func(t *testing.T) {
			return func(t *testing.T) {
				img := filepath.Join(t.TempDir(), "ours.img")
				probe, err := d.Format(filepath.Join(t.TempDir(), "probe.img"), d.size())
				if err != nil {
					t.Fatalf("Format: %v", err)
				}
				caps := and(CapsOf(probe), d.fsCaps())
				probe.Close()
				t.Logf("corpus caps (driver and format): %+v", caps)
				es := v.corpus(Options{Caps: caps, Map: d.Map})
				f, err := d.Format(img, d.size())
				if err != nil {
					t.Fatalf("Format: %v", err)
				}
				if err := PopulateFS(f, es); err != nil {
					f.Close()
					t.Fatalf("our writer: %v", err)
				}
				if err := f.Close(); err != nil {
					t.Fatalf("Close: %v", err)
				}
				d.judge(t, img, Expected(es))
			}
		}
		t.Run("write", write(Variant{}))
		for _, v := range d.Variants {
			t.Run("write-"+v.Name, write(v))
		}
	}
	for _, b := range d.Builders {
		t.Run("write-"+b.Name, func(t *testing.T) {
			caps := d.fsCaps()
			if d.BuilderCaps != nil {
				caps = *d.BuilderCaps
			}
			es := Build(Options{Caps: caps, Map: d.Map})
			src := filepath.Join(t.TempDir(), "src")
			if err := os.Mkdir(src, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := PopulateDir(src, es); err != nil {
				t.Fatalf("populate source: %v", err)
			}
			img := filepath.Join(t.TempDir(), "ours.img")
			if err := b.Build(img, src); err != nil {
				t.Fatalf("our builder: %v", err)
			}
			d.judge(t, img, Expected(es))
		})
	}
	if len(d.Mkfs) > 0 {
		t.Run("reverse-kernel", func(t *testing.T) {
			// The kernel writes everything the format holds, whatever our
			// writer can do: our reader is held to all of it.
			es := Build(Options{Caps: d.fsCaps(), Map: d.Map})
			img := filepath.Join(t.TempDir(), "kernel.img")
			if f, err := os.Create(img); err != nil {
				t.Fatal(err)
			} else {
				f.Close()
			}
			if err := os.Truncate(img, d.size()); err != nil {
				t.Fatal(err)
			}
			if out, err := run(subst(d.Mkfs, img, "")...); err != nil {
				t.Fatalf("mkfs: %v\n%s", err, out)
			}
			mnt := filepath.Join(t.TempDir(), "mnt")
			unmount := mount(t, d, img, mnt, true)
			perr := PopulateDir(mnt, es)
			run("sync")
			unmount()
			if perr != nil {
				t.Fatalf("kernel write: %v", perr)
			}
			d.judge(t, img, Expected(es))
		})
	}
	for _, n := range d.Natives {
		t.Run("reverse-"+n.Name, func(t *testing.T) {
			es := Build(Options{Caps: d.fsCaps(), Map: d.Map})
			src := filepath.Join(t.TempDir(), "src")
			if err := os.Mkdir(src, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := PopulateDir(src, es); err != nil {
				t.Fatalf("populate source: %v", err)
			}
			img := filepath.Join(t.TempDir(), "native.img")
			if out, err := run(subst(n.Args, img, src)...); err != nil {
				t.Fatalf("%s refused: %v\n%s", n.Name, err, bytes.TrimSpace([]byte(out)))
			}
			d.judge(t, img, Expected(es))
		})
	}
}

// EnvKeep names a directory (in the VM) that receives a copy of every image the
// judge examines, so a mismatch can be reproduced by hand.
const EnvKeep = "FS_KERNEL_JUDGE_KEEP"

func keep(t *testing.T, img string) {
	dir := os.Getenv(EnvKeep)
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Logf("keep: %v", err)
		return
	}
	dst := filepath.Join(dir, strings.NewReplacer("/", "_", " ", "_").Replace(t.Name())+".img")
	if out, err := run("cp", "--sparse=always", img, dst); err != nil {
		t.Logf("keep: %v\n%s", err, out)
		return
	}
	t.Logf("kept %s", dst)
}

func kernelRelease() string {
	b, _ := os.ReadFile("/proc/sys/kernel/osrelease")
	return strings.TrimSpace(string(b))
}
