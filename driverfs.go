package kerneljudge

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"

	filesystem "github.com/go-filesystems/interface"
)

type xattrSetter interface {
	SetXattr(path, name string, value []byte) error
}

type xattrLister interface {
	Xattrs(path string) (map[string][]byte, error)
}

// CapsOf reads off a driver's filesystem which optional parts of a corpus it
// can write: what it offers is judged, what it does not is not invented.
func CapsOf(f filesystem.Filesystem) Caps {
	_, sym := f.(filesystem.Symlinker)
	_, tr := f.(filesystem.Truncater)
	_, ms := f.(filesystem.MetadataSetter)
	_, xs := f.(xattrSetter)
	return Caps{Symlinks: sym, Sparse: tr, Mtime: ms, Xattrs: xs}
}

// PopulateFS writes a corpus through a driver's own API.
func PopulateFS(f filesystem.Filesystem, es []Entry) error {
	for _, e := range es {
		switch e.Kind {
		case Dir:
			if err := f.MkDir(e.Path, e.Perm); err != nil {
				return fmt.Errorf("MkDir %s: %w", e.Path, err)
			}
		case Symlink:
			if err := f.(filesystem.Symlinker).Symlink(e.Target, e.Path); err != nil {
				return fmt.Errorf("Symlink %s: %w", e.Path, err)
			}
		case File:
			if err := f.WriteFile(e.Path, e.Content()[:e.DataLen()], e.Perm); err != nil {
				return fmt.Errorf("WriteFile %s: %w", e.Path, err)
			}
			if e.DataLen() < e.Size {
				if err := f.(filesystem.Truncater).Truncate(e.Path, e.Size); err != nil {
					return fmt.Errorf("Truncate %s: %w", e.Path, err)
				}
			}
			if !e.Mtime.IsZero() {
				if err := f.(filesystem.MetadataSetter).Chtimes(e.Path, e.Mtime, e.Mtime); err != nil {
					return fmt.Errorf("Chtimes %s: %w", e.Path, err)
				}
			}
		}
		if len(e.Xattrs) > 0 {
			xs := f.(xattrSetter)
			for k, v := range e.Xattrs {
				if err := xs.SetXattr(e.Path, k, []byte(v)); err != nil {
					return fmt.Errorf("SetXattr %s %s: %w", e.Path, k, err)
				}
			}
		}
	}
	return nil
}

func kindOfMode(mode uint16) Kind {
	switch mode & 0xF000 {
	case 0x8000:
		return File
	case 0x4000:
		return Dir
	case 0xA000:
		return Symlink
	}
	return 0
}

// SnapshotFS describes every path of a driver's filesystem as the driver reads
// it back. A read error is part of the snapshot's report, not a stop: the
// failing path is returned with the other errors so one defect does not hide
// the rest.
//
// mtime, when not nil, reads a file's modification time for a driver that has
// its own accessor instead of filesystem.TimesReader.
func SnapshotFS(f filesystem.Filesystem, mtime func(f filesystem.Filesystem, path string) (time.Time, error)) ([]Node, []string) {
	var ns []Node
	var errs []string
	tr, _ := f.(filesystem.TimesReader)
	xl, _ := f.(xattrLister)
	var walk func(dir string)
	walk = func(dir string) {
		ents, err := f.ListDir(dir)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: ListDir: %v", dir, err))
			return
		}
		sort.Slice(ents, func(i, j int) bool { return ents[i].Name() < ents[j].Name() })
		for _, de := range ents {
			name := de.Name()
			if name == "." || name == ".." || (dir == "/" && name == "lost+found") {
				continue
			}
			p := strings.TrimSuffix(dir, "/") + "/" + name
			st, err := f.Stat(p)
			if err != nil {
				errs = append(errs, fmt.Sprintf("%s: Stat: %v", p, err))
				continue
			}
			n := Node{Path: p, Kind: kindOfMode(st.Mode()), Perm: fs.FileMode(st.Mode() & 0o777)}
			switch n.Kind {
			case Dir:
				walk(p)
			case Symlink:
				if n.Target, err = f.ReadLink(p); err != nil {
					errs = append(errs, fmt.Sprintf("%s: ReadLink: %v", p, err))
				}
			case File:
				n.Size = int64(st.Size())
				b, err := f.ReadFile(p)
				if err != nil {
					errs = append(errs, fmt.Sprintf("%s: ReadFile: %v", p, err))
				}
				n.SHA256 = sha256.Sum256(b)
				if int64(len(b)) != n.Size {
					errs = append(errs, fmt.Sprintf("%s: Stat size %d but ReadFile returned %d bytes", p, n.Size, len(b)))
				}
				n.Size = int64(len(b))
			default:
				errs = append(errs, fmt.Sprintf("%s: Stat mode %#o has no file type", p, st.Mode()))
			}
			if n.Kind == File {
				switch {
				case mtime != nil:
					if m, err := mtime(f, p); err == nil {
						n.Mtime = m
					} else {
						errs = append(errs, fmt.Sprintf("%s: mtime: %v", p, err))
					}
				case tr != nil:
					if ft, err := tr.Times(p); err == nil {
						n.Mtime = ft.Mtime.Or(time.Time{})
					}
				}
			}
			if xl != nil && n.Kind != Symlink {
				if xs, err := xl.Xattrs(p); err == nil && len(xs) > 0 {
					n.Xattrs = map[string]string{}
					for k, v := range xs {
						n.Xattrs[k] = string(v)
					}
				}
			}
			ns = append(ns, n)
		}
	}
	walk("/")
	return ns, errs
}
