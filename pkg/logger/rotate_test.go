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
