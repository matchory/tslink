package main

import (
	"bufio"
	"io"
	"os"
	"path"
	"strings"
)

// pluginSocketDir is where Docker mounts a plugin's socket directory,
// <exec root>/plugins/<plugin ID> on the host.
const pluginSocketDir = "/run/docker/plugins"

// pluginID returns the ID of the Docker plugin this process runs in, from
// its mounts, and false outside one.
func pluginID() (string, bool) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return "", false
	}
	defer f.Close()
	ok, id := inDockerPlugin(f)
	return id, ok
}

// inDockerPlugin reports whether the mount table r, in the format of
// /proc/self/mountinfo, is a Docker plugin's, and the plugin's ID: Docker
// mounts the plugin's directory of its exec root, plugins/<ID>, at
// pluginSocketDir. A host, or a container that mounts the host's socket
// directory there, has no such mount. Lines it cannot parse are skipped.
func inDockerPlugin(r io.Reader) (bool, string) {
	s := bufio.NewScanner(r)
	// An overlay mount's line lists all its layers
	s.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for s.Scan() {
		// ID, parent ID, major:minor, root, mount point, ...
		f := strings.Fields(s.Text())
		if len(f) < 5 || unescapeMountinfo(f[4]) != pluginSocketDir {
			continue
		}
		dir, id := path.Split(unescapeMountinfo(f[3]))
		if path.Base(dir) == "plugins" && isPluginID(id) {
			return true, id
		}
	}
	return false, ""
}

// isPluginID reports whether s is a plugin ID: 64 lower-case hex digits.
func isPluginID(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// unescapeMountinfo decodes the octal escapes (\040 for a space) of a path in
// /proc/self/mountinfo. Anything else, an incomplete escape too, stays.
func unescapeMountinfo(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+4 <= len(s) && isOctalByte(s[i+1:i+4]) {
			b.WriteByte((s[i+1]-'0')<<6 | (s[i+2]-'0')<<3 | (s[i+3] - '0'))
			i += 3
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// isOctalByte reports whether the three characters of s are a byte in octal.
func isOctalByte(s string) bool {
	return s[0] >= '0' && s[0] <= '3' &&
		s[1] >= '0' && s[1] <= '7' &&
		s[2] >= '0' && s[2] <= '7'
}
