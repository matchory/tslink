// Package diag reports the state of the plugin's endpoints for debugging.
package diag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// EndpointStatus represents the health status of a single endpoint.
type EndpointStatus struct {
	ID           string    `json:"id"`
	ShortID      string    `json:"short_id"`
	StateDir     string    `json:"state_dir"`
	SocketExists bool      `json:"socket_exists"`
	StateExists  bool      `json:"state_exists"`
	TailscaleIP  string    `json:"tailscale_ip,omitempty"`
	Hostname     string    `json:"hostname,omitempty"`
	Online       bool      `json:"online"`
	BackendState string    `json:"backend_state,omitempty"`
	LastModified time.Time `json:"last_modified"`
	DebugLog     string    `json:"debug_log,omitempty"`
	Error        string    `json:"error,omitempty"`
}

// StartStatus is an endpoint's status file, written by the plugin while the
// endpoint exists (see core.EndpointStatusFile).
type StartStatus struct {
	Endpoint string    `json:"endpoint"`
	Hostname string    `json:"hostname"`
	Stack    string    `json:"stack,omitempty"`
	State    string    `json:"state"`
	Error    string    `json:"error,omitempty"`
	Attempts int       `json:"attempts"`
	Updated  time.Time `json:"updated"`
}

// Result represents the overall diagnostic result.
type Result struct {
	Timestamp     time.Time         `json:"timestamp"`
	DataDir       string            `json:"data_dir"`
	DataDirExists bool              `json:"data_dir_exists"`
	Endpoints     []*EndpointStatus `json:"endpoints"`
	NotRunning    []*StartStatus    `json:"not_running,omitempty"`
	Summary       Summary           `json:"summary"`
}

// Summary provides a quick overview of endpoint health.
type Summary struct {
	Total   int `json:"total"`
	Online  int `json:"online"`
	Offline int `json:"offline"`
	Errors  int `json:"errors"`
}

// Run performs diagnostics on the tslink plugin state.
func Run(dataDir string, w io.Writer) error {
	result := &Result{
		Timestamp: time.Now(),
		DataDir:   dataDir,
		Endpoints: make([]*EndpointStatus, 0),
	}

	info, err := os.Stat(dataDir)
	if err != nil {
		result.DataDirExists = false
		return outputResult(result, w)
	}
	result.DataDirExists = info.IsDir()

	// Each state directory links to its daemon's socket under <dataDir>/sock
	// (see tailscale.NewDaemon). Sockets no state directory links to are
	// reported on their own.
	linked := make(map[string]bool)
	for _, stateDir := range stateDirs(dataDir) {
		status, socketPath := checkStateDir(dataDir, stateDir)
		if socketPath != "" {
			linked[socketPath] = true
		}
		result.Endpoints = append(result.Endpoints, status)
	}

	sockets, err := filepath.Glob(filepath.Join(dataDir, "sock", "*.sock"))
	if err != nil {
		return fmt.Errorf("failed to list sockets: %w", err)
	}
	for _, socketPath := range sockets {
		if linked[socketPath] {
			continue
		}
		id := strings.TrimSuffix(filepath.Base(socketPath), ".sock")
		status := &EndpointStatus{ID: id, ShortID: "socket " + id, SocketExists: true}
		queryDaemon(status, socketPath)
		result.Endpoints = append(result.Endpoints, status)
	}

	// Endpoints whose Tailscale is not running have no daemon to ask
	statusFiles, _ := filepath.Glob(filepath.Join(dataDir, "status", "*.json"))
	for _, file := range statusFiles {
		data, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		var st StartStatus
		if json.Unmarshal(data, &st) == nil && st.State != "running" {
			result.NotRunning = append(result.NotRunning, &st)
		}
	}
	result.Summary.Errors += len(result.NotRunning)

	for _, status := range result.Endpoints {
		result.Summary.Total++
		switch {
		case status.Error != "":
			result.Summary.Errors++
		case status.Online:
			result.Summary.Online++
		default:
			result.Summary.Offline++
		}
	}

	return outputResult(result, w)
}

// stateDirs returns the tailscaled state directories: by-hostname/<hostname>
// for standalone containers and by-stack/<stack>/<hostname> for stack tasks.
func stateDirs(dataDir string) []string {
	var dirs []string
	for _, pattern := range []string{"by-hostname/*", "by-stack/*/*"} {
		matches, err := filepath.Glob(filepath.Join(dataDir, pattern))
		if err != nil {
			continue
		}
		for _, m := range matches {
			if info, err := os.Stat(m); err == nil && info.IsDir() {
				dirs = append(dirs, m)
			}
		}
	}
	return dirs
}

// checkStateDir reports one state directory and, if its socket link points
// at a live socket, the daemon behind it. It returns that socket's path.
func checkStateDir(dataDir, stateDir string) (*EndpointStatus, string) {
	rel, err := filepath.Rel(dataDir, stateDir)
	if err != nil {
		rel = stateDir
	}
	status := &EndpointStatus{
		ShortID:  rel,
		StateDir: stateDir,
	}

	statePath := filepath.Join(stateDir, "tailscaled.state")
	if info, err := os.Stat(statePath); err == nil {
		status.StateExists = true
		status.LastModified = info.ModTime()
	}

	debugPath := filepath.Join(stateDir, "debug.log")
	if data, err := os.ReadFile(debugPath); err == nil {
		status.DebugLog = string(data)
	}

	target, err := os.Readlink(filepath.Join(stateDir, "tailscaled.sock"))
	if err != nil {
		return status, ""
	}
	socketPath := filepath.Clean(filepath.Join(stateDir, target))
	if info, err := os.Stat(socketPath); err != nil || info.Mode()&os.ModeSocket == 0 {
		return status, "" // daemon stopped; the link dangles
	}
	status.ID = strings.TrimSuffix(filepath.Base(socketPath), ".sock")
	status.SocketExists = true
	queryDaemon(status, socketPath)
	return status, socketPath
}

// queryDaemon fills status from the daemon listening on socketPath.
func queryDaemon(status *EndpointStatus, socketPath string) {
	tsStatus, err := getTailscaleStatus(socketPath)
	if err != nil {
		status.Error = err.Error()
		return
	}
	status.TailscaleIP = tsStatus.IP
	status.Hostname = tsStatus.Hostname
	status.Online = tsStatus.Online
	status.BackendState = tsStatus.BackendState
}

type tailscaleStatusResult struct {
	IP           string
	Hostname     string
	Online       bool
	BackendState string
}

// statusTimeout bounds a status query, so a hung daemon is reported as an
// error rather than hanging the report.
var statusTimeout = 5 * time.Second

// getTailscaleStatus queries tailscale status via the socket.
func getTailscaleStatus(socketPath string) (*tailscaleStatusResult, error) {
	// Find tailscale binary - check common locations
	tailscaleBin := findTailscaleBinary()
	if tailscaleBin == "" {
		return nil, errors.New("tailscale binary not found")
	}

	ctx, cancel := context.WithTimeout(context.Background(), statusTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, tailscaleBin, "--socket="+socketPath, "status", "--json")
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("tailscale status failed: %w", err)
	}

	var result struct {
		Self struct {
			TailscaleIPs []string `json:"TailscaleIPs"`
			HostName     string   `json:"HostName"`
			Online       bool     `json:"Online"`
		} `json:"Self"`
		BackendState string `json:"BackendState"`
	}

	if err := json.Unmarshal(output, &result); err != nil {
		return nil, fmt.Errorf("failed to parse status: %w", err)
	}

	status := &tailscaleStatusResult{
		Hostname:     result.Self.HostName,
		Online:       result.Self.Online,
		BackendState: result.BackendState,
	}

	if len(result.Self.TailscaleIPs) > 0 {
		status.IP = result.Self.TailscaleIPs[0]
	}

	return status, nil
}

// findTailscaleBinary looks for the tailscale binary in common locations.
func findTailscaleBinary() string {
	paths := []string{
		"/usr/bin/tailscale",
		"/usr/local/bin/tailscale",
	}
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}

	return ""
}

func outputResult(result *Result, w io.Writer) error {
	fmt.Fprintf(w, "=== tslink Diagnostic Report ===\n")
	fmt.Fprintf(w, "Timestamp: %s\n", result.Timestamp.Format(time.RFC3339))
	fmt.Fprintf(w, "Data Dir:  %s (exists: %v)\n\n", result.DataDir, result.DataDirExists)

	if !result.DataDirExists {
		fmt.Fprintf(w, "ERROR: Data directory does not exist\n")
		return nil
	}

	if len(result.Endpoints) == 0 && len(result.NotRunning) == 0 {
		fmt.Fprintf(w, "No endpoints found.\n")
		return nil
	}

	fmt.Fprintf(w, "=== Summary ===\n")
	fmt.Fprintf(w, "Total: %d | Online: %d | Offline: %d | Errors: %d\n\n",
		result.Summary.Total, result.Summary.Online, result.Summary.Offline, result.Summary.Errors)

	if len(result.NotRunning) > 0 {
		fmt.Fprintf(w, "=== Containers without Tailscale ===\n")
		for _, st := range result.NotRunning {
			name := st.Hostname
			if st.Stack != "" {
				name = st.Stack + "/" + st.Hostname
			}
			fmt.Fprintf(w, "  %s: %s, attempt %d, %s\n    %s\n",
				name, st.State, st.Attempts, st.Updated.Format(time.RFC3339), st.Error)
		}
		fmt.Fprintf(w, "\n")
	}

	fmt.Fprintf(w, "=== Endpoints ===\n")
	for _, ep := range result.Endpoints {
		writeEndpoint(w, ep)
	}

	return nil
}

// writeEndpoint writes the report section for one endpoint.
func writeEndpoint(w io.Writer, ep *EndpointStatus) {
	fmt.Fprintf(w, "\n--- %s ---\n", ep.ShortID)
	if ep.StateDir != "" {
		fmt.Fprintf(w, "  State dir: %s\n", ep.StateDir)
	}
	fmt.Fprintf(w, "  Socket:    %v\n", ep.SocketExists)
	fmt.Fprintf(w, "  State:     %v\n", ep.StateExists)

	if ep.TailscaleIP != "" {
		fmt.Fprintf(w, "  IP:        %s\n", ep.TailscaleIP)
	}
	if ep.Hostname != "" {
		fmt.Fprintf(w, "  Hostname:  %s\n", ep.Hostname)
	}
	if ep.BackendState != "" {
		fmt.Fprintf(w, "  Backend:   %s\n", ep.BackendState)
	}
	fmt.Fprintf(w, "  Online:    %v\n", ep.Online)

	if ep.Error != "" {
		fmt.Fprintf(w, "  ERROR:     %s\n", ep.Error)
	}

	if ep.DebugLog != "" {
		fmt.Fprintf(w, "  Debug Log:\n")
		for line := range strings.SplitSeq(ep.DebugLog, "\n") {
			if line != "" {
				fmt.Fprintf(w, "    %s\n", line)
			}
		}
	}
}
