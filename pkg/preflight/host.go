package preflight

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Env is what the preflight checks: the Docker API, a way to run host
// commands, the plugin's data directory, and the shared certificate directory
// as mounted where the preflight runs (empty if it is not).
type Env struct {
	Docker    DockerAPI
	Run       func(ctx context.Context, name string, args ...string) ([]byte, error)
	DataDir   string
	SharedDir string
}

// Check checks the environment properties it can see from this host: E1, E2,
// E4, E5, E6, E7, E8 and E9, in this order.
func Check(ctx context.Context, env Env) []Result {
	results := checkContainers(ctx, env.Docker)
	return append(results,
		checkMangle(ctx, env.Run),
		checkPluginDigest(ctx, env.Docker),
		checkSharedDir(env.SharedDir),
		Result{
			Property: "E8", Status: Unknown,
			Detail: "the credential's tag ownership is set in the tailnet policy, not on the host",
		},
		checkTailnetLock(ctx, env.Run, env.DataDir),
	)
}

// Write prints one line per result and reports whether any property is
// violated, or a check failed to run; unknown does not fail it.
func Write(w io.Writer, results []Result) (bool, error) {
	failed := false
	for _, r := range results {
		failed = failed || r.Status == Violated || r.Status == Error
		if _, err := fmt.Fprintf(w, "%-3s %-9s %s\n", r.Property, r.Status, r.Detail); err != nil {
			return failed, fmt.Errorf("failed to write the preflight: %w", err)
		}
	}
	return failed, nil
}

// checkMangle checks what it can of E5: the mangle table, where tslink keeps
// containers off the host's tailnet, works for IPv4 and IPv6.
func checkMangle(
	ctx context.Context,
	run func(context.Context, string, ...string) ([]byte, error),
) Result {
	var violations []string
	for _, cmd := range []string{"iptables", "ip6tables"} {
		if out, err := run(ctx, cmd, "-t", "mangle", "-S"); err != nil {
			violations = append(violations, fmt.Sprintf("%s -t mangle: %v: %s", cmd, err,
				strings.TrimSpace(string(out))))
		}
	}
	return outcome("E5", violations, "the iptables mangle table works for IPv4 and IPv6")
}

// checkPluginDigest checks E6: every tslink plugin was installed by digest.
func checkPluginDigest(ctx context.Context, d DockerAPI) Result {
	plugins, err := tslinkPlugins(ctx, d)
	if err != nil {
		return Result{Property: "E6", Status: Error, Detail: err.Error()}
	}
	if len(plugins) == 0 {
		return Result{Property: "E6", Status: Unknown, Detail: "no tslink plugin is enabled"}
	}
	var violations []string
	for _, p := range plugins {
		switch {
		case p.PluginReference == "":
			violations = append(violations, p.Name+": a local build")
		case !strings.Contains(p.PluginReference, "@sha256:"):
			violations = append(
				violations,
				p.Name+": installed from "+p.PluginReference+", not by digest",
			)
		}
	}
	return outcome("E6", violations, "the tslink plugin was installed by digest")
}

// checkSharedDir checks what it can of E7: the shared certificate directory
// is owned by root and closed to others.
func checkSharedDir(dir string) Result {
	if dir == "" {
		return Result{
			Property: "E7", Status: Unknown,
			Detail: "mount the shared certificate directory and pass --shared-dir to check it",
		}
	}
	st, err := os.Stat(dir)
	if err != nil {
		return Result{Property: "E7", Status: Error, Detail: err.Error()}
	}
	var violations []string
	if sys, ok := st.Sys().(*syscall.Stat_t); ok && sys.Uid != 0 {
		violations = append(
			violations,
			fmt.Sprintf("%s is owned by uid %d, not root", dir, sys.Uid),
		)
	}
	if perm := st.Mode().Perm(); perm&0o077 != 0 {
		violations = append(
			violations,
			fmt.Sprintf("%s has mode %04o, want 0700 or stricter", dir, perm),
		)
	}
	return outcome("E7", violations, dir+" is owned by root, mode 0700 or stricter")
}

// checkTailnetLock reports E9: whether Tailnet Lock is on, asked of one of
// the host's tslink nodes.
func checkTailnetLock(
	ctx context.Context,
	run func(context.Context, string, ...string) ([]byte, error),
	dataDir string,
) Result {
	sockets, err := filepath.Glob(filepath.Join(dataDir, "sock", "*.sock"))
	if err != nil || len(sockets) == 0 {
		return Result{
			Property: "E9",
			Status:   Unknown,
			Detail:   "no tslink node runs on this host to ask",
		}
	}
	// A node that does not answer leaves E9 unknown: it is a report, not a requirement
	out, err := run(ctx, "tailscale", "--socket="+sockets[0], "lock", "status", "--json")
	if err != nil {
		return Result{
			Property: "E9",
			Status:   Unknown,
			Detail:   fmt.Sprintf("tailscale lock status: %v", err),
		}
	}
	var status struct {
		Enabled bool `json:"Enabled"`
	}
	if err := json.Unmarshal(out, &status); err != nil {
		return Result{
			Property: "E9",
			Status:   Unknown,
			Detail:   fmt.Sprintf("tailscale lock status: %v", err),
		}
	}
	if !status.Enabled {
		return Result{
			Property: "E9", Status: Info,
			Detail: "Tailnet Lock is off: peer keys depend on the control server",
		}
	}
	return Result{Property: "E9", Status: OK, Detail: "Tailnet Lock is on"}
}
