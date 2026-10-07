package tailscale

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
	"sync"

	"github.com/aaomidi/tslink/pkg/logger"
)

// streamingWriter wraps output and logs each line as it arrives.
type streamingWriter struct {
	prefix string
	buf    bytes.Buffer
	mu     sync.Mutex
}

func (w *streamingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	n, err := w.buf.Write(p)
	if err != nil {
		return n, err
	}

	// Log complete lines as they arrive
	for {
		line, readErr := w.buf.ReadString('\n')
		if errors.Is(readErr, io.EOF) {
			// Put back incomplete line
			w.buf.WriteString(line)
			break
		}
		if readErr != nil {
			break
		}
		line = strings.TrimRight(line, "\n\r")
		if line != "" {
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

// runCommandWithStreaming runs a command and streams its output to the logger.
func runCommandWithStreaming(
	ctx context.Context,
	prefix string,
	name string,
	args ...string,
) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)

	stdout := &streamingWriter{prefix: prefix + ":stdout"}
	stderr := &streamingWriter{prefix: prefix + ":stderr"}

	cmd.Stdout = stdout
	cmd.Stderr = stderr

	logger.Debugf("[%s] Running: %s %v", prefix, name, args)

	err := cmd.Run()

	// Combine output for return
	output := stdout.String() + stderr.String()

	return output, err
}

// tailscale runs the tailscale CLI with args, logging its output under prefix.
func (d *Daemon) tailscale(ctx context.Context, prefix string, args ...string) (string, error) {
	if d.runCLI != nil {
		return d.runCLI(ctx, prefix, args...)
	}
	return runCommandWithStreaming(ctx, prefix, d.config.TailscaleBin, args...)
}
