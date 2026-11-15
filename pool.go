package kerneljudge

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// Pool describes an OpenZFS pool in an image file. The judge imports it
// read-only with the kernel's zfs module (zpool import -d <dir> -o readonly=on
// -R <mnt> <Name>) and exports it afterwards; a read-write run creates the pool
// (zpool create -f -O mountpoint=/ -R <mnt>), so the root dataset's files appear
// at <mnt>.
type Pool struct {
	// Name is the pool name inside the image: our Format is given it, and the
	// native writer creates it.
	Name string
	// CreateArgs are extra arguments for zpool create (e.g. "-o", "ashift=12").
	CreateArgs []string
}

// mountRoot is where the files of an image appear under the mount directory:
// mnt itself, or for a pool the root dataset's own mountpoint below the
// altroot (an image written by a driver may name it /<pool>, the native one is
// created with mountpoint=/).
func (d Driver) mountRoot(mnt string) string {
	if d.Pool == nil {
		return mnt
	}
	out, err := run("zfs", "list", "-H", "-o", "mountpoint", d.Pool.Name)
	if err != nil || out == "" || out == "none" || out == "legacy" {
		return mnt
	}
	if strings.HasPrefix(out, mnt) { // zfs list already shows it below the altroot
		return out
	}
	return filepath.Join(mnt, out)
}

func (d Driver) poolMount(t *testing.T, img, mnt string, rw bool) (func(), error) {
	t.Helper()
	name := d.Pool.Name
	var args []string
	if rw {
		args = append([]string{"zpool", "create", "-f"}, d.Pool.CreateArgs...)
		args = append(args, "-O", "mountpoint=/", "-R", mnt, name, img)
	} else {
		args = []string{"zpool", "import", "-d", filepath.Dir(img), "-o", "readonly=on", "-R", mnt, name}
	}
	out, err := run(args...)
	if err != nil && !rw && strings.Contains(out, "zpool import -f") {
		// The pool is not marked exported (an image written by a driver that
		// leaves it ACTIVE needs -f). Report that, and import with -f so the
		// rest of the image is still judged.
		d.report(t, "kernel mount", []string{fmt.Sprintf("%s refused without -f: %s", "zpool import", out)})
		forced := append([]string{"zpool", "import", "-f"}, args[2:]...)
		out, err = run(forced...)
	}
	if err != nil {
		dm, _ := run("sh", "-c", "dmesg | tail -n 6")
		return nil, fmt.Errorf("%s: %v: %s\nkernel log:\n%s", args[0]+" "+args[1], err, out, dm)
	}
	return func() {
		if out, err := run("zpool", "export", "-f", name); err != nil {
			t.Errorf("zpool export: %v\n%s", err, out)
		}
	}, nil
}
