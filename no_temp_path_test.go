package kerneljudge

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Non-test code must not name a path in the shared temporary directory: a
// fixed name there can be pre-created or swapped by another local user. The
// OLE oracle once wrote its script to os.TempDir()/kerneljudge-ole.py and then
// ran it, which let such a user run code as the caller.
func TestSourceNamesNoSharedTempPath(t *testing.T) {
	forbidden := []string{`"/tmp`, `"/var/tmp`, "os.TempDir(", "ioutil.TempFile(", "ioutil.TempDir("}
	err := filepath.WalkDir(".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != "." && (d.Name() == "testdata" || strings.HasPrefix(d.Name(), ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		for _, bad := range forbidden {
			if strings.Contains(string(b), bad) {
				t.Errorf("%s contains %s: non-test code must not name a shared temporary path", p, bad)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
