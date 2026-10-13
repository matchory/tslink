package logger

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRotatingFileKeepsSizeBounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.log")
	f, err := OpenRotating(path, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	line := strings.Repeat("a", 39) + "\n"
	for range 10 {
		if _, err := f.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{path, path + ".1"} {
		st, err := os.Stat(p)
		if err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}
		if st.Size() > 100 {
			t.Errorf("%s is %d bytes, want <= 100", p, st.Size())
		}
	}
}

// A rotation that fails, here because <path>.1 is a directory with an entry,
// keeps the log writable: lines go on to the current file, and the rotation
// is tried again on the next write.
func TestRotatingFileWritesOnAfterFailedRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.log")
	if err := os.MkdirAll(filepath.Join(path+".1", "blocker"), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := OpenRotating(path, 10)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, line := range []string{"first line\n", "second line\n", "third line\n"} {
		if n, err := f.Write([]byte(line)); err != nil || n != len(line) {
			t.Fatalf("Write(%q) = %d, %v", line, n, err)
		}
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := "first line\nsecond line\nthird line\n"; string(got) != want {
		t.Errorf("log = %q, want %q", got, want)
	}
}
