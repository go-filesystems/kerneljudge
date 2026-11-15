package kerneljudge

import (
	"fmt"
	"sort"
	"time"
)

// Compare selects what Diff holds two snapshots to.
type Compare struct {
	Mtime          bool
	MtimeTolerance time.Duration // default 2 s
	Xattrs         bool
	Perm           bool
}

// Diff lists every difference between want and got, one line each, with the
// exact path and the two values. An empty result is equality.
func Diff(want, got []Node, c Compare) []string {
	if c.MtimeTolerance == 0 {
		c.MtimeTolerance = 2 * time.Second
	}
	gm := make(map[string]Node, len(got))
	for _, n := range got {
		gm[n.Path] = n
	}
	var out []string
	seen := map[string]bool{}
	for _, w := range want {
		seen[w.Path] = true
		g, ok := gm[w.Path]
		if !ok {
			out = append(out, fmt.Sprintf("%s: missing (want %s)", w.Path, w.Kind))
			continue
		}
		if g.Kind != w.Kind {
			out = append(out, fmt.Sprintf("%s: kind want %s got %s", w.Path, w.Kind, g.Kind))
			continue
		}
		if c.Perm && w.Kind != Symlink && g.Perm != w.Perm {
			out = append(out, fmt.Sprintf("%s: perm want %04o got %04o", w.Path, w.Perm, g.Perm))
		}
		switch w.Kind {
		case File:
			if g.Size != w.Size {
				out = append(out, fmt.Sprintf("%s: size want %d got %d", w.Path, w.Size, g.Size))
			}
			if g.SHA256 != w.SHA256 {
				out = append(out, fmt.Sprintf("%s: sha256 want %x got %x", w.Path, w.SHA256[:6], g.SHA256[:6]))
			}
			if c.Mtime && !w.Mtime.IsZero() {
				if d := g.Mtime.Sub(w.Mtime).Abs(); d > c.MtimeTolerance {
					out = append(out, fmt.Sprintf("%s: mtime want %s got %s", w.Path, w.Mtime.UTC().Format(time.RFC3339), g.Mtime.UTC().Format(time.RFC3339)))
				}
			}
		case Symlink:
			if g.Target != w.Target {
				out = append(out, fmt.Sprintf("%s: link target want %q got %q", w.Path, w.Target, g.Target))
			}
		}
		if c.Xattrs && w.Kind != Symlink {
			for k, v := range w.Xattrs {
				if gv, ok := g.Xattrs[k]; !ok {
					out = append(out, fmt.Sprintf("%s: xattr %s missing", w.Path, k))
				} else if gv != v {
					out = append(out, fmt.Sprintf("%s: xattr %s want %d bytes got %d bytes", w.Path, k, len(v), len(gv)))
				}
			}
		}
	}
	for p, g := range gm {
		if !seen[p] {
			out = append(out, fmt.Sprintf("%s: unexpected %s", p, g.Kind))
		}
	}
	sort.Strings(out)
	return out
}
