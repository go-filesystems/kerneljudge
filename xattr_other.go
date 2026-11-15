//go:build !linux

package kerneljudge

import "errors"

func setXattr(path, name string, v []byte) error {
	return errors.New("extended attributes need Linux")
}

func listXattrs(string) map[string]string { return nil }
