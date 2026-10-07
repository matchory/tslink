package tailscale

import (
	"context"
	"testing"
)

// The output of a command comes back whole, not just its last unterminated
// line: callers match it and parse JSON from it.
func TestExecCLIKeepsOutput(t *testing.T) {
	for _, prefix := range []string{"test", ""} {
		out, err := execCLI(context.Background(), "sh", cliCall{
			prefix: prefix,
			stdin:  "three",
			args:   []string{"-c", "echo one; echo two; cat >&2"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if want := (cliOutput{stdout: "one\ntwo\n", stderr: "three"}); out != want {
			t.Errorf("prefix %q: output = %+v, want %+v", prefix, out, want)
		}
	}
}
