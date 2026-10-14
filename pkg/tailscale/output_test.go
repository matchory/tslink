package tailscale

import (
	"io"
	"strings"
	"testing"
)

func TestDrainLinesSurvivesLongLines(t *testing.T) {
	// tailscaled blocks on a full pipe, so a reader that gives up on a long
	// line freezes it: every line must be consumed until EOF.
	long := strings.Repeat("x", 1<<20)
	in := "first\n" + long + "\nlast\n"
	var got []string
	drainLines(strings.NewReader(in), 1024, func(l string) { got = append(got, l) })
	if len(got) != 3 || got[0] != "first" || got[2] != "last" {
		t.Fatalf("got %d lines: %q...", len(got), got[0])
	}
	if len(got[1]) > 1024+len(truncatedMark) || !strings.HasSuffix(got[1], truncatedMark) {
		t.Errorf("long line not truncated: %d bytes", len(got[1]))
	}
}

func TestDrainLinesReadsToEOF(t *testing.T) {
	r, w := io.Pipe()
	done := make(chan struct{})
	go func() {
		drainLines(r, 16, func(string) {})
		close(done)
	}()
	for range 1000 {
		if _, err := w.Write([]byte(strings.Repeat("y", 100) + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	w.Close()
	<-done
}
