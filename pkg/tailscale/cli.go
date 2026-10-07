package tailscale

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"sync"

	"github.com/matchory/tslink/pkg/logger"
)

// streamingWriter keeps output and logs each line as it arrives, unless its
// prefix is empty.
type streamingWriter struct {
	prefix string
	buf    bytes.Buffer
	logged int // Bytes of buf logged so far
	mu     sync.Mutex
}

func (w *streamingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	n, err := w.buf.Write(p)
	if err != nil || w.prefix == "" {
		return n, err
	}

	// Log complete lines as they arrive; the buffer keeps them for String
	for {
		rest := w.buf.Bytes()[w.logged:]
		i := bytes.IndexByte(rest, '\n')
		if i < 0 {
			break
		}
		w.logged += i + 1
		if line := strings.TrimRight(string(rest[:i]), "\r"); line != "" {
			logger.Debugf("[%s] %s", w.prefix, line)
		}
	}
	return n, nil
}

func (w *streamingWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// cliCall is a run of the tailscale CLI.
type cliCall struct {
	prefix string // Logs the output line by line under prefix; empty logs nothing
	stdin  string // The CLI's standard input
	args   []string
}

// cliOutput is what a run of the tailscale CLI wrote.
type cliOutput struct {
	stdout string
	stderr string
}

// combined returns the standard output followed by the standard error.
func (o cliOutput) combined() string {
	return o.stdout + o.stderr
}

// execCLI runs the CLI at bin and streams its output to the logger.
func execCLI(ctx context.Context, bin string, c cliCall) (cliOutput, error) {
	cmd := exec.CommandContext(ctx, bin, c.args...)
	cmd.Stdin = strings.NewReader(c.stdin)

	stdout := &streamingWriter{}
	stderr := &streamingWriter{}
	if c.prefix != "" {
		stdout.prefix = c.prefix + ":stdout"
		stderr.prefix = c.prefix + ":stderr"
		logger.Debugf("[%s] Running: %s %v", c.prefix, bin, c.args)
	}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	err := cmd.Run()
	return cliOutput{stdout: stdout.String(), stderr: stderr.String()}, err
}

// runTailscale runs the tailscale CLI.
func (d *Daemon) runTailscale(ctx context.Context, c cliCall) (cliOutput, error) {
	if d.runCLI != nil {
		return d.runCLI(ctx, c)
	}
	return execCLI(ctx, d.config.TailscaleBin, c)
}

// tailscale runs the tailscale CLI with args, logging its output under prefix,
// and returns its standard output followed by its standard error.
func (d *Daemon) tailscale(ctx context.Context, prefix string, args ...string) (string, error) {
	out, err := d.runTailscale(ctx, cliCall{prefix: prefix, args: args})
	return out.combined(), err
}
