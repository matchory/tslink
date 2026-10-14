package tailscale

import (
	"context"
	"testing"
)

// The output of a command comes back whole, not just its last unterminated
// line: callers match it and parse JSON from it.
func TestExecCLIKeepsOutput(t *testing.T) {
	out, err := execCLI(context.Background(), "sh", cliCall{
		prefix: "test",
		stdin:  "three",
		args:   []string{"-c", "echo one; echo two; cat >&2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := (cliOutput{stdout: "one\ntwo\n", stderr: "three"}); out != want {
		t.Errorf("output = %+v, want %+v", out, want)
	}
}
