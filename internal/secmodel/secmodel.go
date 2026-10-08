// Package secmodel checks the security model in SECURITY.md against the
// tests: every guarantee it publishes must be guarded by a test, a model or a
// probe, or be marked as checked by hand.
package secmodel

import (
	"bytes"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// knownGuarantees is the number of guarantees: Guarantee IDs run from G1 to
// G10. Tests may guard any of them, published or not.
const knownGuarantees = 10

// Guarantee is a guarantee SECURITY.md publishes.
type Guarantee struct {
	ID     string // G1, G2, ...
	Manual bool   // its section has a "Manual:" line: checked by hand before releases
}

var (
	guaranteeHeading = regexp.MustCompile(`^### (G[0-9]+):`)
	goMarker         = regexp.MustCompile(`(?m)^//\s*Guards:\s*(.+)$`)
	tlaMarker        = regexp.MustCompile(`(?m)^\\\*\s*Guards:\s*(.+)$`)
	shellProbe       = regexp.MustCompile(`(?m)^\s*probe\s+(G[0-9]+)\s`)
	guaranteeID      = regexp.MustCompile(`G[0-9]+`)
)

// ParseGuarantees returns the guarantees in a SECURITY.md: its "### G<n>:"
// headings, and whether each one's section has a "Manual:" line. A section
// ends at the next heading.
func ParseGuarantees(md []byte) []Guarantee {
	var guarantees []Guarantee
	current := -1
	for line := range strings.SplitSeq(string(md), "\n") {
		line = strings.TrimRight(line, "\r")
		if m := guaranteeHeading.FindStringSubmatch(line); m != nil {
			guarantees = append(guarantees, Guarantee{ID: m[1]})
			current = len(guarantees) - 1
			continue
		}
		if strings.HasPrefix(line, "#") {
			current = -1
			continue
		}
		if current >= 0 && strings.HasPrefix(line, "Manual:") {
			guarantees[current].Manual = true
		}
	}
	return guarantees
}

// markerPattern returns the pattern of guarantee markers in a file, or nil
// for files that hold none: Go tests, TLA+ models and shell probes.
func markerPattern(path string) *regexp.Regexp {
	switch {
	case strings.HasSuffix(path, "_test.go"):
		return goMarker
	case strings.HasSuffix(path, ".tla"):
		return tlaMarker
	case strings.HasSuffix(path, ".sh"):
		return shellProbe
	}
	return nil
}

// Markers returns, for each guarantee ID, where tests, models and probes
// under root name it, as "path:line". It skips directories whose names start
// with a dot, and testdata.
func Markers(root string) (map[string][]string, error) {
	markers := make(map[string][]string)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if path != root && (strings.HasPrefix(name, ".") || name == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		pattern := markerPattern(path)
		if pattern == nil {
			return nil
		}
		//nolint:gosec // root is the local repository checkout, not attacker-controlled input
		content, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("failed to read %s: %w", path, err)
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return fmt.Errorf("failed to locate %s: %w", path, err)
		}
		for _, loc := range pattern.FindAllSubmatchIndex(content, -1) {
			at := fmt.Sprintf(
				"%s:%d",
				filepath.ToSlash(rel),
				1+bytes.Count(content[:loc[0]], []byte("\n")),
			)
			for _, id := range guaranteeID.FindAll(content[loc[2]:loc[3]], -1) {
				markers[string(id)] = append(markers[string(id)], at)
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to collect guarantee markers: %w", err)
	}
	return markers, nil
}

// Check returns the problems of a security model: published guarantees that
// nothing guards and that are not checked by hand, and markers naming a
// guarantee that does not exist.
func Check(published []Guarantee, markers map[string][]string) []string {
	var problems []string
	for _, g := range published {
		if len(markers[g.ID]) == 0 && !g.Manual {
			problems = append(
				problems,
				g.ID+" is published, but no test, model or probe guards it and it has no Manual: line",
			)
		}
	}
	for _, id := range slices.Sorted(maps.Keys(markers)) {
		n, err := strconv.Atoi(strings.TrimPrefix(id, "G"))
		if err != nil || n < 1 || n > knownGuarantees {
			problems = append(problems, fmt.Sprintf(
				"%s is not a guarantee (G1 to G%d), but %s guards it",
				id, knownGuarantees, strings.Join(markers[id], ", "),
			))
		}
	}
	return problems
}
