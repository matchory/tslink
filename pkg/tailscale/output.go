package tailscale

import (
	"bufio"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/matchory/tslink/pkg/logger"
)

// truncatedMark ends a line drainLines shortened.
const truncatedMark = " [truncated]"

// drainLines reads r until EOF and calls fn for every line, cut to maxLen
// bytes. It never stops early: tailscaled blocks once its output pipe is full,
// so a reader that gave up, as bufio.Scanner does on a line over 64 KiB,
// would freeze it.
func drainLines(r io.Reader, maxLen int, fn func(string)) {
	br := bufio.NewReaderSize(r, maxLen)
	var line []byte
	truncated := false
	for {
		chunk, isPrefix, err := br.ReadLine()
		if len(chunk) > 0 && !truncated {
			if room := maxLen - len(line); len(chunk) > room {
				line = append(line, chunk[:room]...)
				truncated = true
			} else {
				line = append(line, chunk...)
			}
		}
		if err != nil {
			if len(line) > 0 {
				fn(string(marked(line, truncated)))
			}
			return
		}
		if isPrefix {
			continue
		}
		fn(string(marked(line, truncated)))
		line, truncated = line[:0], false
	}
}

// marked returns line with truncatedMark if it was cut.
func marked(line []byte, truncated bool) []byte {
	if truncated {
		return append(line, truncatedMark...)
	}
	return line
}

// Limits for tailscaled's output: a line longer than maxLogLine is cut, and
// its log is rotated at tailscaledLogMaxBytes.
const (
	maxLogLine            = 16 << 10
	tailscaledLogMaxBytes = 10 << 20
)

// closePipe closes both ends of an output pipe that has no writer.
func closePipe(r *io.PipeReader, w *io.PipeWriter) {
	if err := errors.Join(r.Close(), w.Close()); err != nil {
		logger.Debugf("Failed to close the output pipe: %v", err)
	}
}

// drainOutput writes tailscaled's output to its own size-capped log, until
// watchProcess closes the pipe after tailscaled exited.
func (d *daemon) drainOutput(output io.Reader) {
	// tailscaled's output goes to its own size-capped log: it is verbose, and
	// the plugin log is shared by every endpoint on the node
	logPath := filepath.Join(d.config.StateDir, "tailscaled.log")
	logFile, err := logger.OpenRotating(logPath, tailscaledLogMaxBytes)
	if err != nil {
		logger.Warnf("Failed to open %s, discarding tailscaled output: %v", logPath, err)
	}
	writeLine := func(line string) {
		d.handleLine(line)
		if logFile != nil {
			if _, err := logFile.Write([]byte(line + "\n")); err != nil {
				logger.Debugf("Failed to write %s: %v", logPath, err)
			}
		}
	}

	d.wg.Go(func() {
		drainLines(output, maxLogLine, writeLine)
		if logFile != nil {
			if err := logFile.Close(); err != nil {
				logger.Debugf("Failed to close %s: %v", logPath, err)
			}
		}
	})
}

// handleLine notes what tslink acts on in a line of tailscaled's output.
func (d *daemon) handleLine(line string) {
	switch {
	case strings.Contains(line, nodeNotFoundLog):
		d.nodeNotFound.Store(time.Now().Unix())
	case strings.Contains(line, servicesFetchLog):
		d.fetchMu.Lock()
		d.fetches++
		if d.fetched != nil {
			close(d.fetched)
			d.fetched = nil
		}
		d.fetchMu.Unlock()
	case strings.Contains(line, `cert("`):
		d.handleCertLine(line)
	}
}

// servicesFetches returns how often control has fetched the node's Service
// list, and a channel closed at the next fetch.
func (d *daemon) servicesFetches() (int64, <-chan struct{}) {
	d.fetchMu.Lock()
	defer d.fetchMu.Unlock()
	if d.fetched == nil {
		d.fetched = make(chan struct{})
	}
	return d.fetches, d.fetched
}
