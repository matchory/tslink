package tailscale

import (
	"bufio"
	"io"
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
				fn(string(line))
			}
			return
		}
		if isPrefix {
			continue
		}
		if truncated {
			line = append(line, truncatedMark...)
		}
		fn(string(line))
		line, truncated = line[:0], false
	}
}
