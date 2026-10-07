package core

import (
	"encoding/json"
	"os"
	"testing"
)

func readStatus(t *testing.T, e *Endpoint) EndpointStatusFile {
	t.Helper()
	var st EndpointStatusFile
	b, err := os.ReadFile(StatusPath(e.DataDir, e.ID))
	if err != nil {
		t.Fatalf("read status: %v", err)
	}
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatal(err)
	}
	return st
}

// warningKeys returns the keys of the status file's warnings, in order.
func warningKeys(st EndpointStatusFile) []string {
	keys := make([]string, 0, len(st.Warnings))
	for _, w := range st.Warnings {
		keys = append(keys, w.Key)
	}
	return keys
}

func TestStatusWarnings(t *testing.T) {
	e := &Endpoint{ID: "0123456789abcdef", DataDir: t.TempDir()}

	// A warning before the first status is kept for it
	e.setWarning("renewal-blocked/b.example.ts.net", "blocked")
	if _, err := os.Stat(StatusPath(e.DataDir, e.ID)); !os.IsNotExist(err) {
		t.Fatalf("a warning wrote a status before the start (err=%v)", err)
	}
	e.writeStatus(&ContainerInfo{Hostname: "web", Stack: "app"}, StatusRunning, 0, nil)
	st := readStatus(t, e)
	if got := warningKeys(st); len(got) != 1 || st.Warnings[0].Message != "blocked" ||
		st.Warnings[0].Since.IsZero() {
		t.Fatalf("warnings = %+v", st.Warnings)
	}

	// Later warnings rewrite the status, sorted by key, keeping its state
	e.setWarning("cert-expiry/a.example.ts.net", "expires soon")
	st = readStatus(t, e)
	if got := warningKeys(st); len(got) != 2 || got[0] != "cert-expiry/a.example.ts.net" {
		t.Fatalf("warnings = %+v", st.Warnings)
	}
	if st.State != StatusRunning || st.Hostname != "web" || st.Stack != "app" {
		t.Errorf("status lost its state: %+v", st)
	}

	// The same warning again keeps its first time
	since := st.Warnings[1].Since
	e.setWarning("renewal-blocked/b.example.ts.net", "blocked")
	if st = readStatus(t, e); !st.Warnings[1].Since.Equal(since) {
		t.Errorf("repeated warning moved its time: %v, was %v", st.Warnings[1].Since, since)
	}

	// An empty message clears it
	e.setWarning("cert-expiry/a.example.ts.net", "")
	if got := warningKeys(readStatus(t, e)); len(got) != 1 {
		t.Errorf("cleared warning kept: %q", got)
	}

	// A state change keeps the warnings
	e.writeStatus(&ContainerInfo{Hostname: "web"}, StatusRetrying, 1, nil)
	if got := warningKeys(readStatus(t, e)); len(got) != 1 {
		t.Errorf("warnings lost on a state change: %q", got)
	}

	// Once the endpoint left, warnings write nothing
	e.removeStatus()
	e.setWarning("cert-expiry/a.example.ts.net", "expires soon")
	if _, err := os.Stat(StatusPath(e.DataDir, e.ID)); !os.IsNotExist(err) {
		t.Errorf("a warning after Leave wrote the status (err=%v)", err)
	}
}
