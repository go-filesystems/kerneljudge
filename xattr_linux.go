package kerneljudge

import (
	"strings"
	"syscall"
)

func setXattr(path, name string, v []byte) error { return syscall.Setxattr(path, name, v, 0) }

// listXattrs returns the user.* attributes of path (the namespace corpora use).
func listXattrs(path string) map[string]string {
	n, err := syscall.Listxattr(path, nil)
	if err != nil || n <= 0 {
		return nil
	}
	buf := make([]byte, n)
	if n, err = syscall.Listxattr(path, buf); err != nil {
		return nil
	}
	out := map[string]string{}
	for _, name := range strings.Split(string(buf[:n]), "\x00") {
		if !strings.HasPrefix(name, "user.") {
			continue
		}
		sz, err := syscall.Getxattr(path, name, nil)
		if err != nil {
			continue
		}
		v := make([]byte, sz)
		if sz, err = syscall.Getxattr(path, name, v); err != nil {
			continue
		}
		out[name] = string(v[:sz])
	}
	return out
}
