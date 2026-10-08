package secmodel

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestParseGuarantees(t *testing.T) {
	md := "# Security policy\r\n\r\n## Security model\r\n\r\n" +
		"### G1: Containers reach the tailnet only as their own node\r\n\r\nAssumes: E1.\r\n\r\n" +
		"### G5: Credentials do not leak\r\n\r\nManual: canary scan before releases.\r\n\r\n" +
		"## Environment properties\r\n\r\nManual: a line outside any guarantee\r\n"
	got := ParseGuarantees([]byte(md))
	want := []Guarantee{{ID: "G1"}, {ID: "G5", Manual: true}}
	if !slices.Equal(got, want) {
		t.Errorf("ParseGuarantees = %+v, want %+v", got, want)
	}
}

func TestMarkers(t *testing.T) {
	root := t.TempDir()
	write := func(path, content string) {
		t.Helper()
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("pkg/a/a_test.go", "package a\n\n// "+"Guards: G1, G5\nfunc TestA(t *testing.T) {}\n")
	write("pkg/a/a.go", "package a\n\n// "+"Guards: G2\n")
	write("test/models/drain.tla", "---- MODULE drain ----\n\\* "+"Guards: G7\n")
	write("test/integration/run.sh", "#!/bin/bash\nprobe() { :; }\n  pro"+"be G1 \"desc\" attack\n")
	write(".git/hooks/x.sh", "pro"+"be G9 \"x\" y\n")
	write("pkg/a/testdata/x_test.go", "// "+"Guards: G8\n")

	got, err := Markers(root)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"G1": {"pkg/a/a_test.go:3", "test/integration/run.sh:3"},
		"G5": {"pkg/a/a_test.go:3"},
		"G7": {"test/models/drain.tla:2"},
	}
	if !maps.EqualFunc(got, want, slices.Equal) {
		t.Errorf("Markers = %v, want %v", got, want)
	}
}

func TestCheck(t *testing.T) {
	published := []Guarantee{{ID: "G1"}, {ID: "G5"}, {ID: "G7", Manual: true}}
	markers := map[string][]string{
		"G1":  {"a_test.go:3"},
		"G4":  {"b_test.go:9"},
		"G11": {"c_test.go:1"},
	}
	got := Check(published, markers)
	want := []string{
		"G5 is published, but no test, model or probe guards it and it has no Manual: line",
		"G11 is not a guarantee (G1 to G10), but c_test.go:1 guards it",
	}
	if !slices.Equal(got, want) {
		t.Errorf("Check = %q, want %q", got, want)
	}
}

// TestRepository checks this repository: every guarantee SECURITY.md
// publishes is guarded. With -v it lists the tests guarding each one.
func TestRepository(t *testing.T) {
	md, err := os.ReadFile("../../SECURITY.md")
	if err != nil {
		t.Fatal(err)
	}
	markers, err := Markers("../..")
	if err != nil {
		t.Fatal(err)
	}
	published := ParseGuarantees(md)
	for _, problem := range Check(published, markers) {
		t.Error(problem)
	}
	for _, g := range published {
		t.Logf("%s: %s", g.ID, strings.Join(markers[g.ID], " "))
	}
}
