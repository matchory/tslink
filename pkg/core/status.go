package core

import (
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/matchory/tslink/pkg/logger"
)

// States of an endpoint's Tailscale, as written to its status file.
const (
	StatusRetrying = "retrying" // starting failed, will retry
	StatusFailed   = "failed"   // starting failed in a way retrying cannot fix
	StatusRunning  = "running"
)

// EndpointStatusFile is written to <data>/status/<endpoint>.json while the
// endpoint exists. A task without its identity looks healthy to Swarm, so
// this is how diagnostics and monitoring on the node find it.
type EndpointStatusFile struct {
	Endpoint string          `json:"endpoint"`
	Hostname string          `json:"hostname"`
	Stack    string          `json:"stack,omitempty"`
	State    string          `json:"state"`
	Error    string          `json:"error,omitempty"`
	Attempts int             `json:"attempts"`
	Updated  time.Time       `json:"updated"`
	Warnings []StatusWarning `json:"warnings,omitempty"`
}

// StatusWarning is a condition of a running endpoint that needs attention, as
// a certificate whose renewal is blocked. Key identifies it, such as
// "renewal-blocked/<domain>"; Since is when it was first seen.
type StatusWarning struct {
	Key     string    `json:"key"`
	Message string    `json:"message"`
	Since   time.Time `json:"since"`
}

// StatusPath returns the status file of an endpoint.
func StatusPath(dataDir, endpointID string) string {
	return filepath.Join(dataDir, "status", endpointID[:12]+".json")
}

// writeStatus records the endpoint's Tailscale state, with its warnings;
// failing to is logged only.
func (e *Endpoint) writeStatus(info *ContainerInfo, state string, attempts int, startErr error) {
	st := EndpointStatusFile{
		Endpoint: e.ID,
		Hostname: info.Hostname,
		Stack:    info.Stack,
		State:    state,
		Attempts: attempts,
		Updated:  time.Now().UTC(),
	}
	if startErr != nil {
		st.Error = startErr.Error()
	}
	e.statusMu.Lock()
	defer e.statusMu.Unlock()
	e.status = &st
	e.saveStatus()
}

// setWarning records a warning in the endpoint's status under key, or clears
// it if message is empty. The status file is rewritten only when its warnings
// change; before the endpoint's first status, the warning waits for it.
func (e *Endpoint) setWarning(key, message string) {
	e.statusMu.Lock()
	defer e.statusMu.Unlock()
	old, ok := e.warnings[key]
	switch {
	case message == "" && !ok, message != "" && ok && old.Message == message:
		return
	case message == "":
		delete(e.warnings, key)
	default:
		if e.warnings == nil {
			e.warnings = make(map[string]StatusWarning)
		}
		e.warnings[key] = StatusWarning{Key: key, Message: message, Since: time.Now().UTC()}
	}
	if e.status != nil {
		e.status.Updated = time.Now().UTC()
		e.saveStatus()
	}
}

// saveStatus writes e.status with the current warnings. The caller holds
// statusMu.
func (e *Endpoint) saveStatus() {
	st := *e.status
	st.Warnings = slices.SortedFunc(maps.Values(e.warnings), func(a, b StatusWarning) int {
		return strings.Compare(a.Key, b.Key)
	})
	path := StatusPath(e.DataDir, e.ID)
	data, err := json.Marshal(st)
	if err == nil {
		err = os.MkdirAll(filepath.Dir(path), 0o700)
	}
	if err == nil {
		// Write and rename, so readers never see a partial file
		tmp := path + ".tmp"
		if err = os.WriteFile(tmp, data, 0o600); err == nil {
			err = os.Rename(tmp, path)
		}
	}
	if err != nil {
		logger.Warnf("Failed to write status of endpoint %s: %v", e.ID[:12], err)
	}
}

// removeStatus deletes the endpoint's status file.
func (e *Endpoint) removeStatus() {
	e.statusMu.Lock()
	defer e.statusMu.Unlock()
	e.status = nil
	for _, path := range []string{StatusPath(e.DataDir, e.ID), ReadyPath(e.DataDir, e.ID)} {
		err := os.Remove(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			logger.Warnf("Failed to remove status of endpoint %s: %v", e.ID[:12], err)
		}
	}
}
