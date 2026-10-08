package core

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// hostileAlphabet holds the characters that could take a path out of its
// directory or split a line.
var hostileAlphabet = []string{"a", "-", "_", ".", "/", "\x00", "\n"}

// allStrings returns every string of up to maxLen elements of alphabet,
// including the empty string.
func allStrings(alphabet []string, maxLen int) []string {
	words, level := []string{""}, []string{""}
	for range maxLen {
		var next []string
		for _, w := range level {
			for _, c := range alphabet {
				next = append(next, w+c)
			}
		}
		words = append(words, next...)
		level = next
	}
	return words
}

// checkStateDir fails t unless stateDirFor rejects stack and hostname, or
// returns <data>/by-hostname/<hostname> or <data>/by-stack/<stack>/<hostname>.
func checkStateDir(t *testing.T, stack, hostname string) {
	t.Helper()
	const data = "/data"
	dir, err := stateDirFor(data, stack, hostname)
	if err != nil {
		return
	}
	want := []string{"by-hostname", hostname}
	if stack != "" {
		want = []string{"by-stack", stack, hostname}
	}
	rel, relErr := filepath.Rel(data, dir)
	parts := strings.Split(rel, string(filepath.Separator))
	if relErr != nil || !slices.Equal(parts, want) {
		t.Fatalf(
			"stateDirFor(%q, %q) = %q, want %s/%s",
			stack,
			hostname,
			dir,
			data,
			strings.Join(want, "/"),
		)
	}
	for _, p := range parts {
		if p == "." || p == ".." || strings.ContainsAny(p, "\x00\n") {
			t.Fatalf("stateDirFor(%q, %q) = %q: element %q", stack, hostname, dir, p)
		}
	}
}

// Guards: G6
func FuzzStateDirFor(f *testing.F) {
	for _, s := range []string{"", ".", "..", "../x", "a/b", "/abs", "-x", "web", "app_web.1.abc", "a\x00b", "a\nb"} {
		f.Add("app", s)
		f.Add(s, "web")
	}
	f.Fuzz(func(t *testing.T, stack, hostname string) {
		checkStateDir(t, stack, hostname)
	})
}

// Guards: G6
func TestStateDirForExhaustive(t *testing.T) {
	for _, s := range allStrings(hostileAlphabet, 4) {
		checkStateDir(t, "", s)
		checkStateDir(t, "app", s)
		checkStateDir(t, s, "web")
	}
}

// TestCheckStackScopeOnStackNetworks checks that a stack's network takes
// tasks of that stack only.
//
// Guards: G4
func TestCheckStackScopeOnStackNetworks(t *testing.T) {
	stacks := []string{"", "a", "b", "a-b", "a_b", "A"}
	network := &Network{ID: "n", AuthKey: "tskey-auth-x"}
	for _, stack := range stacks {
		for _, networkStack := range stacks[1:] {
			info := &ContainerInfo{Stack: stack, NetworkStack: networkStack}
			err := checkStackScope(info, network, nil)
			if want := stack != networkStack; (err != nil) != want {
				t.Errorf("checkStackScope(stack %q, network stack %q) = %v, want error %v",
					stack, networkStack, err, want)
			}
		}
	}
}

// TestCheckStackScopeWithClusterCredential checks checkStackScope's own
// cluster-credential branch, which TestCheckStackScopeOnStackNetworks does
// not exercise: that test's network always has an AuthKey, so
// UsesClusterCredential is always false there.
//
// Guards: G4
func TestCheckStackScopeWithClusterCredential(t *testing.T) {
	tests := []struct {
		name    string
		stack   string // info.Stack and info.NetworkStack: same stack, matching tags
		tags    []string
		strict  bool
		wantErr bool
	}{
		{"stackless network refused", "", []string{"tag:billing"}, true, true},
		{"out-of-scope tag refused", "billing", []string{"tag:shop"}, true, true},
		{"tag:<stack> accepted", "billing", []string{"tag:billing"}, true, false},
		{
			"prefix opt-out: tag:<stack>-x accepted",
			"billing",
			[]string{"tag:billing-x"},
			false,
			false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			network := &Network{ID: "n", StrictTagScope: tt.strict}
			info := &ContainerInfo{Stack: tt.stack, NetworkStack: tt.stack}
			err := checkStackScope(info, network, tt.tags)
			if (err != nil) != tt.wantErr {
				t.Errorf("checkStackScope(stack %q, tags %v, strict %v) = %v, want error %v",
					tt.stack, tt.tags, tt.strict, err, tt.wantErr)
			}
		})
	}
}
