package kerneljudge

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	filesystem "github.com/go-filesystems/interface"
)

// EnvPerf turns the performance run on (it needs a quiet VM and host).
const EnvPerf = "FS_KERNEL_JUDGE_PERF"

// PerfOptions sizes the performance image.
type PerfOptions struct {
	LargeBytes int64 // default 256 MiB
	SmallFiles int   // default 4000
	Rounds     int   // default 5, or FS_KERNEL_JUDGE_ROUNDS
}

// Result is one measurement series.
type Result struct {
	Op, Impl string
	Rate     []float64 // MiB/s for "seq read", files/s for "walk"
	Unit     string
	Note     string
}

// Median is the middle round.
func (r Result) Median() float64 {
	s := append([]float64(nil), r.Rate...)
	sort.Float64s(s)
	return s[len(s)/2]
}

func dropCaches() error {
	if err := os.WriteFile("/proc/sys/vm/drop_caches", []byte("3"), 0); err != nil {
		return err
	}
	return nil
}

func load1() float64 {
	b, _ := os.ReadFile("/proc/loadavg")
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return 99
	}
	v, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return 99
	}
	return v
}

// quiet waits for the VM's 1-minute load to fall under max, or skips.
func quiet(t *testing.T, max float64) {
	t.Helper()
	for i := 0; i < 24; i++ {
		if load1() <= max {
			return
		}
		time.Sleep(5 * time.Second)
	}
	t.Skipf("VM 1-minute load stays above %.1f: not measuring", max)
}

const seqChunk = 1 << 20

func oursSeq(f filesystem.Filesystem, path string) (int64, string, error) {
	if o, ok := f.(filesystem.Opener); ok {
		fl, err := o.OpenFile(path)
		if err != nil {
			return 0, "", err
		}
		defer fl.Close()
		buf := make([]byte, seqChunk)
		var off int64
		for off < fl.Size() {
			n, err := fl.ReadAt(buf, off)
			off += int64(n)
			if err != nil && err != io.EOF {
				return off, "", err
			}
			if n == 0 {
				break
			}
		}
		return off, "OpenFile+ReadAt 1 MiB", nil
	}
	b, err := f.ReadFile(path)
	return int64(len(b)), "ReadFile", err
}

func oursWalk(f filesystem.Filesystem, dir string) (files int, err error) {
	ents, err := f.ListDir(dir)
	if err != nil {
		return 0, err
	}
	for _, de := range ents {
		name := de.Name()
		if name == "." || name == ".." {
			continue
		}
		p := strings.TrimSuffix(dir, "/") + "/" + name
		st, err := f.Stat(p)
		if err != nil {
			return files, err
		}
		switch kindOfMode(st.Mode()) {
		case Dir:
			n, err := oursWalk(f, p)
			files += n
			if err != nil {
				return files, err
			}
		case File:
			if _, err := f.ReadFile(p); err != nil {
				return files, err
			}
			files++
		}
	}
	return files, nil
}

func kernelWalk(root string) (int, error) {
	files := 0
	ents, err := os.ReadDir(root)
	if err != nil {
		return 0, err
	}
	for _, de := range ents {
		p := filepath.Join(root, de.Name())
		if de.IsDir() {
			n, err := kernelWalk(p)
			files += n
			if err != nil {
				return files, err
			}
			continue
		}
		if _, err := os.Lstat(p); err != nil {
			return files, err
		}
		if _, err := os.ReadFile(p); err != nil {
			return files, err
		}
		files++
	}
	return files, nil
}

// RunPerf measures the kernel (mounted image) and our driver on one image
// written by the native tools, in the same VM, rounds interleaved, caches
// dropped before every measurement. Each measurement includes the mount (or
// Open) it needs, so the two sides pay for their own metadata.
func RunPerf(t *testing.T, d Driver, o PerfOptions) []Result {
	Enabled(t)
	if os.Getenv(EnvPerf) != "1" {
		t.Skipf("set %s=1 to measure", EnvPerf)
	}
	if o.LargeBytes == 0 {
		o.LargeBytes = 256 << 20
	}
	if o.SmallFiles == 0 {
		o.SmallFiles = 4000
	}
	if o.Rounds == 0 {
		o.Rounds = 5
		if v, err := strconv.Atoi(os.Getenv("FS_KERNEL_JUDGE_ROUNDS")); err == nil && v > 0 {
			o.Rounds = v
		}
	}
	quiet(t, 1.0)
	es := Build(Options{LargeBytes: o.LargeBytes, SmallFiles: o.SmallFiles, Caps: Caps{Mtime: false}})
	// Only the large file and the small files matter here; the driver's name
	// mapping (if any) is applied after the selection, and the two paths the
	// measurements need are mapped the same way.
	var keep []Entry
	for _, e := range es {
		if e.Path == "/large.bin" || e.Path == "/small" || strings.HasPrefix(e.Path, "/small/") {
			if d.Map != nil {
				var ok bool
				if e, ok = d.Map(e); !ok {
					continue
				}
			}
			keep = append(keep, e)
		}
	}
	largePath, smallPath := "/large.bin", "/small"
	if d.Map != nil {
		l, _ := d.Map(Entry{Path: largePath, Kind: File})
		s, _ := d.Map(Entry{Path: smallPath, Kind: Dir})
		largePath, smallPath = l.Path, s.Path
	}
	// The kernel shows ISO names in lower case.
	kLarge, kSmall := largePath, smallPath
	if d.Compare.FoldCase {
		kLarge, kSmall = strings.ToLower(largePath), strings.ToLower(smallPath)
	}
	dir := t.TempDir()
	img := filepath.Join(dir, "perf.img")
	size := d.size()
	if need := o.LargeBytes*2 + int64(o.SmallFiles)*8192 + 64<<20; need > size {
		size = need
	}
	switch {
	case len(d.Mkfs) > 0:
		f, err := os.Create(img)
		if err != nil {
			t.Fatal(err)
		}
		f.Close()
		if err := os.Truncate(img, size); err != nil {
			t.Fatal(err)
		}
		if out, err := run(subst(d.Mkfs, img, "")...); err != nil {
			t.Fatalf("mkfs: %v\n%s", err, out)
		}
		mnt := filepath.Join(dir, "w")
		unmount := mount(t, d, img, mnt, true)
		perr := PopulateDir(mnt, keep)
		run("sync")
		unmount()
		if perr != nil {
			t.Fatalf("kernel write: %v", perr)
		}
	case len(d.Natives) > 0:
		src := filepath.Join(dir, "src")
		if err := os.Mkdir(src, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := PopulateDir(src, keep); err != nil {
			t.Fatal(err)
		}
		if out, err := run(subst(d.Natives[0].Args, img, src)...); err != nil {
			t.Fatalf("%s: %v\n%s", d.Natives[0].Name, err, out)
		}
		os.RemoveAll(src)
	default:
		t.Fatal("no native writer to make the performance image")
	}
	fi, err := os.Stat(img)
	if err != nil {
		t.Fatalf("the performance image was not written: %v", err)
	}
	t.Logf("image %s: %d MiB, %d MiB large file, %d small files, %d rounds", d.Name, fi.Size()>>20, o.LargeBytes>>20, o.SmallFiles, o.Rounds)

	res := []Result{
		{Op: "seq read", Impl: "kernel", Unit: "MiB/s"}, {Op: "seq read", Impl: "ours", Unit: "MiB/s"},
		{Op: "metadata walk", Impl: "kernel", Unit: "files/s"}, {Op: "metadata walk", Impl: "ours", Unit: "files/s"},
	}
	for r := 0; r < o.Rounds; r++ {
		// The four measurements are queued and run in an order that alternates
		// with the round (kernel first, ours second; then the reverse), so a
		// drift of the VM or the host, or a warm-up that favours whoever goes
		// second, falls on both sides equally.
		var queue [4]func()
		measure := func(i int, f func() (float64, string, error)) {
			queue[i] = func() {
				if err := dropCaches(); err != nil {
					t.Fatalf("drop_caches: %v", err)
				}
				rate, note, err := f()
				if err != nil {
					t.Fatalf("%s %s: %v", res[i].Op, res[i].Impl, err)
				}
				res[i].Rate = append(res[i].Rate, rate)
				if note != "" {
					res[i].Note = note
				}
			}
		}
		order := [4]int{0, 1, 2, 3}
		if r%2 == 1 {
			order = [4]int{1, 0, 3, 2}
		}
		measure(0, func() (float64, string, error) {
			mnt := filepath.Join(dir, "m")
			os.MkdirAll(mnt, 0o755)
			start := time.Now()
			if out, err := run("mount", "-t", d.FSType, "-o", "loop,ro", img, mnt); err != nil {
				return 0, "", fmt.Errorf("mount: %v\n%s", err, out)
			}
			fl, err := os.Open(filepath.Join(mnt, kLarge))
			if err != nil {
				run("umount", mnt)
				return 0, "", err
			}
			n, err := io.CopyBuffer(io.Discard, struct{ io.Reader }{fl}, make([]byte, seqChunk))
			el := time.Since(start)
			fl.Close()
			if out, uerr := run("umount", mnt); uerr != nil && err == nil {
				err = fmt.Errorf("umount: %v\n%s", uerr, out)
			}
			if err == nil && n != o.LargeBytes {
				err = fmt.Errorf("read %d bytes, the file has %d", n, o.LargeBytes)
			}
			return float64(n) / (1 << 20) / el.Seconds(), "", err
		})
		measure(1, func() (float64, string, error) {
			start := time.Now()
			f, err := d.Open(img)
			if err != nil {
				return 0, "", err
			}
			defer f.Close()
			n, note, err := oursSeq(f, largePath)
			el := time.Since(start)
			if err == nil && n != o.LargeBytes {
				err = fmt.Errorf("read %d bytes, the file has %d", n, o.LargeBytes)
			}
			return float64(n) / (1 << 20) / el.Seconds(), note, err
		})
		measure(2, func() (float64, string, error) {
			mnt := filepath.Join(dir, "m")
			os.MkdirAll(mnt, 0o755)
			start := time.Now()
			if out, err := run("mount", "-t", d.FSType, "-o", "loop,ro", img, mnt); err != nil {
				return 0, "", fmt.Errorf("mount: %v\n%s", err, out)
			}
			n, err := kernelWalk(filepath.Join(mnt, kSmall))
			el := time.Since(start)
			if out, uerr := run("umount", mnt); uerr != nil && err == nil {
				err = fmt.Errorf("umount: %v\n%s", uerr, out)
			}
			if err == nil && n != o.SmallFiles {
				err = fmt.Errorf("walked %d files, the image has %d", n, o.SmallFiles)
			}
			return float64(n) / el.Seconds(), "", err
		})
		measure(3, func() (float64, string, error) {
			start := time.Now()
			f, err := d.Open(img)
			if err != nil {
				return 0, "", err
			}
			defer f.Close()
			n, err := oursWalk(f, smallPath)
			el := time.Since(start)
			if err == nil && n != o.SmallFiles {
				err = fmt.Errorf("walked %d files, the image has %d", n, o.SmallFiles)
			}
			return float64(n) / el.Seconds(), "", err
		})
		for _, i := range order {
			queue[i]()
		}
	}
	for _, r := range res {
		t.Logf("PERF %s | %-13s | %-6s | median %9.1f %s | rounds %v | %s", d.Name, r.Op, r.Impl, r.Median(), r.Unit, fmtRates(r.Rate), r.Note)
	}
	return res
}

func fmtRates(r []float64) string {
	s := make([]string, len(r))
	for i, v := range r {
		s[i] = strconv.FormatFloat(v, 'f', 1, 64)
	}
	return strings.Join(s, " ")
}
