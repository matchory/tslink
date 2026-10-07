package tailscale

import (
	"context"
	"testing"
)

// The output of a command comes back whole, not just its last unterminated
// line: callers match it and parse JSON from it.
func TestRunCommandWithStreamingKeepsOutput(t *testing.T) {
	out, err := runCommandWithStreaming(
		context.Background(), "test", "sh", "-c", "echo one; echo two; echo three >&2",
	)
	if err != nil {
		t.Fatal(err)
	}
	if want := "one\ntwo\nthree\n"; out != want {
		t.Errorf("output = %q, want %q", out, want)
	}
}
