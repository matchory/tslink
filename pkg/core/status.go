package core

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/aaomidi/tslink/pkg/logger"
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
	Endpoint string    `json:"endpoint"`
	Hostname string    `json:"hostname"`
	Stack    string    `json:"stack,omitempty"`
	State    string    `json:"state"`
	Error    string    `json:"error,omitempty"`
	Attempts int       `json:"attempts"`
	Updated  time.Time `json:"updated"`
}

// StatusPath returns the status file of an endpoint.
func StatusPath(dataDir, endpointID string) string {
	return filepath.Join(dataDir, "status", endpointID[:12]+".json")
}

// writeStatus records the endpoint's Tailscale state; failing to is logged only.
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
	if err := os.Remove(StatusPath(e.DataDir, e.ID)); err != nil && !errors.Is(err, os.ErrNotExist) {
		logger.Warnf("Failed to remove status of endpoint %s: %v", e.ID[:12], err)
	}
}
