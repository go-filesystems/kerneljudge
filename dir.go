package kerneljudge

import (
	"crypto/sha256"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// PopulateDir writes a corpus through the host's own filesystem calls, which on
// a mounted image means the kernel's driver. Xattrs are written only where the
// platform has them (Linux).
func PopulateDir(root string, es []Entry) error {
	for _, e := range es {
		p := filepath.Join(root, filepath.FromSlash(e.Path))
		switch e.Kind {
		case Dir:
			if err := os.Mkdir(p, e.Perm); err != nil && !os.IsExist(err) {
				return err
			}
			if err := os.Chmod(p, e.Perm); err != nil {
				return err
			}
		case Symlink:
			if err := os.Symlink(e.Target, p); err != nil {
				return err
			}
		case File:
			f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_EXCL, e.Perm)
			if err != nil {
				return err
			}
			if _, err := f.Write(e.Content()[:e.DataLen()]); err != nil {
				f.Close()
				return err
			}
			if e.DataLen() < e.Size {
				if err := f.Truncate(e.Size); err != nil {
					f.Close()
					return err
				}
			}
			if err := f.Close(); err != nil {
				return err
			}
			if err := os.Chmod(p, e.Perm); err != nil {
				return err
			}
		}
		for k, v := range e.Xattrs {
			if err := setXattr(p, k, []byte(v)); err != nil {
				return fmt.Errorf("%s: setxattr %s: %w", e.Path, k, err)
			}
		}
		if !e.Mtime.IsZero() && e.Kind == File {
			if err := os.Chtimes(p, e.Mtime, e.Mtime); err != nil {
				return err
			}
		}
	}
	return nil
}

// SnapshotDir walks a directory (a kernel mount, or the source of a build) and
// describes every path under it.
func SnapshotDir(root string) ([]Node, error) {
	var ns []Node
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == root {
			return nil
		}
		rel := "/" + filepath.ToSlash(strings.TrimPrefix(p, root+string(filepath.Separator)))
		if rel == "/lost+found" {
			return fs.SkipDir
		}
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		n := Node{Path: rel, Perm: fi.Mode().Perm(), Mtime: fi.ModTime()}
		switch {
		case fi.IsDir():
			n.Kind = Dir
		case fi.Mode()&fs.ModeSymlink != 0:
			n.Kind = Symlink
			if n.Target, err = os.Readlink(p); err != nil {
				return err
			}
		case fi.Mode().IsRegular():
			n.Kind, n.Size = File, fi.Size()
			if n.SHA256, err = hashFile(p); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%s: unexpected mode %v", rel, fi.Mode())
		}
		if n.Kind != Symlink {
			n.Xattrs = listXattrs(p)
		}
		ns = append(ns, n)
		return nil
	})
	return ns, err
}

func hashFile(p string) ([32]byte, error) {
	var sum [32]byte
	f, err := os.Open(p)
	if err != nil {
		return sum, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return sum, err
	}
	copy(sum[:], h.Sum(nil))
	return sum, nil
}
