package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestBootstrapUsage(t *testing.T) {
	for _, args := range [][]string{{}, {"--org", "x"}, {"--org", "x", "--project", "y"}, {"--bogus"}, {"--org", "x", "--project", "y", "--owner", "a@b.c", "extra"}} {
		var out, errb bytes.Buffer
		if code := bootstrapMain(args, &out, &errb); code != 2 || out.Len() != 0 {
			t.Errorf("%v: exit %d, stdout %q", args, code, out.String())
		}
	}
}

func TestWriteSecret(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a", "b", "key")
	for _, data := range []string{"one\n", "two\n"} {
		if err := writeSecret(p, data); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(p)
		fi, _ := os.Stat(p)
		di, _ := os.Stat(filepath.Dir(p))
		if err != nil || string(b) != data || fi.Mode().Perm() != 0o600 || di.Mode().Perm() != 0o700 {
			t.Errorf("writeSecret: %q %v %v %v", b, err, fi.Mode(), di.Mode())
		}
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(p), ".key.*")); len(left) != 0 {
		t.Errorf("temp files left: %v", left)
	}
}
