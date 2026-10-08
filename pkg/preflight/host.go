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

	"github.com/matchory/tslink/pkg/core"
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
		checkIsolation(ctx, env.Docker, env.Run),
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

// isolationChains are the mangle table chains in which the plugin keeps
// containers off the host's tailnet (pkg/netutil's isolate.go).
var isolationChains = []string{"TSLINK-ISOLATE-FWD", "TSLINK-ISOLATE-IN"}

// checkIsolation checks what it can of E5: no tslink plugin turns the host's
// tailnet isolation off, and the plugin's isolation chains are in the mangle
// table for IPv4 and IPv6.
func checkIsolation(
	ctx context.Context,
	d DockerAPI,
	run func(context.Context, string, ...string) ([]byte, error),
) Result {
	plugins, err := tslinkPlugins(ctx, d)
	if err != nil {
		return Result{Property: "E5", Status: Error, Detail: err.Error()}
	}
	if len(plugins) == 0 {
		return noPlugin("E5")
	}
	var violations []string
	for _, p := range plugins {
		if isolationOff(p.Settings.Env) {
			violations = append(violations, p.Name+": "+core.IsolateHostTailnetSetting+"=false")
		}
	}
	for _, cmd := range []string{"iptables", "ip6tables"} {
		for _, chain := range isolationChains {
			if out, err := run(ctx, cmd, "-t", "mangle", "-S", chain); err != nil {
				violations = append(violations, fmt.Sprintf("%s: chain %s is not in the mangle "+
					"table: %v: %s", cmd, chain, err, strings.TrimSpace(string(out))))
			}
		}
	}
	return outcome("E5", violations, "the plugin's isolation chains are in the mangle table "+
		"for IPv4 and IPv6 on this host, and "+core.IsolateHostTailnetSetting+" is on")
}

// isolationOff reports whether a plugin's settings turn the host's tailnet
// isolation off. As in pkg/core's isolateHostTailnet, only "false" does.
func isolationOff(env []string) bool {
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		if k == core.IsolateHostTailnetSetting && strings.EqualFold(v, "false") {
			return true
		}
	}
	return false
}

// checkPluginDigest checks E6: every tslink plugin was installed by digest.
func checkPluginDigest(ctx context.Context, d DockerAPI) Result {
	plugins, err := tslinkPlugins(ctx, d)
	if err != nil {
		return Result{Property: "E6", Status: Error, Detail: err.Error()}
	}
	if len(plugins) == 0 {
		return noPlugin("E6")
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
	return outcome("E6", violations, "the tslink plugin on this host was installed by digest")
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
	return outcome("E7", violations, dir+" is owned by root, mode 0700 or stricter, "+
		"as mounted on this host")
}

// checkTailnetLock reports E9: whether Tailnet Lock is on, asked of every
// tslink node on this host. A node that does not answer is left out: E9 is a
// report, not a requirement.
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
	answered, off := 0, 0
	var lastErr error
	for _, sock := range sockets {
		enabled, err := lockEnabled(ctx, run, sock)
		if err != nil {
			lastErr = err
			continue
		}
		answered++
		if !enabled {
			off++
		}
	}
	switch {
	case answered == 0:
		return Result{
			Property: "E9", Status: Unknown,
			Detail: fmt.Sprintf("no tslink node on this host answered: %v", lastErr),
		}
	case off > 0:
		return Result{Property: "E9", Status: Info, Detail: fmt.Sprintf(
			"Tailnet Lock is off for %d of %d answering tslink nodes on this host: "+
				"their peer keys depend on the control server", off, answered)}
	}
	return Result{Property: "E9", Status: OK, Detail: fmt.Sprintf(
		"Tailnet Lock is on for every answering tslink node on this host (%d of %d)",
		answered, len(sockets))}
}

// lockEnabled asks the tailscaled at sock whether Tailnet Lock is on.
func lockEnabled(
	ctx context.Context,
	run func(context.Context, string, ...string) ([]byte, error),
	sock string,
) (bool, error) {
	out, err := run(ctx, "tailscale", "--socket="+sock, "lock", "status", "--json")
	if err != nil {
		return false, fmt.Errorf("tailscale lock status: %w", err)
	}
	var status struct {
		Enabled bool `json:"Enabled"`
	}
	if err := json.Unmarshal(out, &status); err != nil {
		return false, fmt.Errorf("tailscale lock status: %w", err)
	}
	return status.Enabled, nil
}
