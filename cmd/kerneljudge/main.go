// Command kerneljudge runs a go-filesystems driver's judge test as root inside
// a Tart Linux VM, where the Linux kernel and the filesystem's native tools
// (btrfs check, fsck.exfat, unsquashfs, mkfs, mksquashfs) are the judges.
//
//	kerneljudge run --driver ~/clone/btrfs --vm fsjudge-vm [--perf]
//
// It builds the driver's tests for linux/arm64, starts the VM, copies the test
// binary in, runs it as root with FS_KERNEL_JUDGE=1 and prints the result,
// then stops the VM. The driver's test calls kerneljudge.Run (and RunPerf).
//
// With --perf it first refuses when the host's one-minute load average is above
// --max-load (default 8): a measurement made under load is not a measurement.
package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

var osExit = os.Exit

// packages the VM needs: the native tools of every supported filesystem.
const packages = "btrfs-progs exfatprogs squashfs-tools dosfstools e2fsprogs python3-olefile p7zip-full attr xorriso genisoimage zfsutils-linux"

type host struct {
	vm, driver, run, bin string
	perf, attach, keep   bool
	maxLoad              float64
	rounds               int
	out                  io.Writer
}

func (h host) exec(stdin string, args ...string) error {
	a := []string{"exec"}
	if stdin != "" {
		a = append(a, "-i")
	}
	cmd := exec.Command("tart", append(append(a, h.vm), args...)...)
	if stdin != "" {
		f, err := os.Open(stdin)
		if err != nil {
			return err
		}
		defer f.Close()
		cmd.Stdin = f
	} else {
		cmd.Stdin = nil
	}
	cmd.Stdout, cmd.Stderr = h.out, h.out
	return cmd.Run()
}

// hostLoad is the host's one-minute load average, from sysctl vm.loadavg
// ("{ 1.20 2.30 3.40 }").
func hostLoad() (float64, error) {
	b, err := exec.Command("sysctl", "-n", "vm.loadavg").Output()
	if err != nil {
		return 0, err
	}
	return parseLoad(string(b))
}

func parseLoad(s string) (float64, error) {
	f := strings.Fields(strings.Trim(strings.TrimSpace(s), "{}"))
	if len(f) < 1 {
		return 0, errors.New("empty load average")
	}
	return strconv.ParseFloat(f[0], 64)
}

func (h host) build() error {
	if err := os.MkdirAll(filepath.Dir(h.bin), 0o700); err != nil {
		return err
	}
	cmd := exec.Command("go", "test", "-c", "-o", h.bin, ".")
	cmd.Dir = h.driver
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH=arm64", "CGO_ENABLED=0")
	cmd.Stdout, cmd.Stderr = h.out, h.out
	return cmd.Run()
}

func (h host) runHost() error {
	if strings.ContainsAny(h.run, "'\\\n") {
		return errors.New("--test-run must not contain a quote, a backslash or a newline")
	}
	if err := h.loadGate(); err != nil {
		return err
	}
	if err := h.build(); err != nil {
		return fmt.Errorf("build the driver's tests: %w", err)
	}
	if !h.attach {
		if h.exec("", "true") == nil {
			return errors.New("the VM is already running (another session's?): stop it first, or use --attach")
		}
		run := exec.Command("tart", "run", "--no-graphics", h.vm)
		if err := run.Start(); err != nil {
			return err
		}
		var once sync.Once
		cleanup := func() {
			once.Do(func() {
				exec.Command("tart", "stop", h.vm).Run()
				run.Process.Kill()
				run.Wait()
			})
		}
		defer cleanup()
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		go func() { <-sig; cleanup(); osExit(130) }()
	}
	deadline := time.Now().Add(3 * time.Minute)
	for h.exec("", "true") != nil {
		if time.Now().After(deadline) {
			return errors.New("the VM does not answer tart exec")
		}
		time.Sleep(3 * time.Second)
	}
	if err := h.exec("", "sh", "-c", "command -v mkfs.btrfs >/dev/null && command -v mksquashfs >/dev/null && command -v fsck.exfat >/dev/null && command -v xorriso >/dev/null && command -v isoinfo >/dev/null && command -v zpool >/dev/null || sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -q "+packages+" >/dev/null"); err != nil {
		return fmt.Errorf("prepare the VM: %w", err)
	}
	if err := h.exec(h.bin, "sh", "-c", "mkdir -p ~/kj && umask 077 && cat > ~/kj/driver.test && chmod 700 ~/kj/driver.test"); err != nil {
		return err
	}
	env := "FS_KERNEL_JUDGE=1"
	if h.keep {
		env += " FS_KERNEL_JUDGE_KEEP=$HOME/kj/images"
	}
	if h.perf {
		env += " FS_KERNEL_JUDGE_PERF=1 FS_KERNEL_JUDGE_ROUNDS=" + strconv.Itoa(h.rounds)
	}
	// The builds and the boot raised the load: look again just before measuring.
	if err := h.loadGate(); err != nil {
		return err
	}
	var buf bytes.Buffer
	h.out = io.MultiWriter(h.out, &buf)
	err := h.exec("", "sh", "-c", "cd ~/kj && sudo env "+env+" ./driver.test -test.v -test.timeout 40m -test.run '"+h.run+"'")
	if err != nil {
		return err
	}
	return verdict(buf.String(), h.perf)
}

// loadGate refuses --perf when the host is busy.
func (h host) loadGate() error {
	if !h.perf {
		return nil
	}
	l, err := hostLoad()
	if err != nil {
		return fmt.Errorf("cannot read the host load: %w", err)
	}
	if l > h.maxLoad {
		return fmt.Errorf("host 1-minute load %.1f is above %.1f: not measuring (defer, or raise --max-load)", l, h.maxLoad)
	}
	return nil
}

// verdict refuses an exit status of 0 that proves nothing: no test passed
// (the pattern matched nothing, or everything skipped), or a requested
// performance run skipped itself (a busy VM).
func verdict(out string, perf bool) error {
	if !strings.Contains(out, "\n--- PASS: ") && !strings.HasPrefix(out, "--- PASS: ") {
		return errors.New("no test passed: the --test-run pattern matched nothing, or every test skipped")
	}
	if perf && strings.Contains(out, "\n--- SKIP: TestKernelJudgePerf") {
		return errors.New("the performance run skipped itself (VM load?): nothing was measured")
	}
	return nil
}

// RootCmd is the command.
func RootCmd() *cobra.Command {
	root := &cobra.Command{Use: "kerneljudge", SilenceUsage: true}
	h := host{maxLoad: 8}
	run := &cobra.Command{
		Use: "run", Short: "run a driver's judge test as root in a Tart VM", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			h.out = cmd.OutOrStdout()
			if h.bin == "" {
				c, err := os.UserCacheDir()
				if err != nil {
					return err
				}
				h.bin = filepath.Join(c, "kerneljudge", filepath.Base(h.driver)+".test")
			}
			return h.runHost()
		},
	}
	run.Flags().StringVar(&h.vm, "vm", "fsjudge-vm", "name of a Tart Ubuntu arm64 VM (a clone of ghcr.io/cirruslabs/ubuntu:latest)")
	run.Flags().StringVar(&h.driver, "driver", "", "a clone of the driver whose test calls kerneljudge.Run")
	run.Flags().StringVar(&h.run, "test-run", "KernelJudge", "the -test.run pattern of the judge tests")
	run.Flags().StringVar(&h.bin, "bin", "", "where the linux/arm64 test binary is built (default: user cache)")
	run.Flags().BoolVar(&h.perf, "perf", false, "also measure (needs a quiet host)")
	run.Flags().BoolVar(&h.attach, "attach", false, "use a VM that is already running; do not start or stop it")
	run.Flags().BoolVar(&h.keep, "keep", false, "keep every examined image in ~/kj/images in the VM, to reproduce a mismatch by hand")
	run.Flags().Float64Var(&h.maxLoad, "max-load", 8, "refuse --perf above this host 1-minute load")
	run.Flags().IntVar(&h.rounds, "rounds", 5, "interleaved rounds for --perf")
	run.MarkFlagRequired("driver")
	root.AddCommand(run)
	return root
}

func main() {
	if err := RootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		osExit(1)
	}
}
