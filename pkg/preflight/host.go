package preflight

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
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
		checkTagScope(ctx, env.Docker),
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

// The chains tslink jumps to first from a built-in chain, and the built-in
// chains themselves (pkg/netutil's isolate.go).
const (
	vethFwdChain    = "TSLINK-VETH-FWD"
	isolateFwdChain = "TSLINK-ISOLATE-FWD"
	isolateInChain  = "TSLINK-ISOLATE-IN"
	forwardBuiltin  = "FORWARD"
	inputBuiltin    = "INPUT"
	iptablesCmd     = "iptables"
	ip6tablesCmd    = "ip6tables"
)

// tslinkChains are the chains tslink jumps to first from a built-in chain;
// its jumps lead their chain in any order.
var tslinkChains = []string{vethFwdChain, isolateFwdChain, isolateInChain}

// isolationJump is a jump the isolation needs at the head of a built-in chain.
type isolationJump struct {
	cmd, from, to string
	always        bool // the veth isolation does not depend on the setting
}

var isolationJumps = []isolationJump{
	{iptablesCmd, forwardBuiltin, vethFwdChain, true},
	{iptablesCmd, forwardBuiltin, isolateFwdChain, false},
	{iptablesCmd, inputBuiltin, isolateInChain, false},
	{ip6tablesCmd, forwardBuiltin, isolateFwdChain, false},
	{ip6tablesCmd, inputBuiltin, isolateInChain, false},
}

// leadingJumps returns the chains of the leading run of tslink jumps in the
// listing of a built-in chain (iptables -S).
func leadingJumps(listing, from string) []string {
	var jumps []string
	for line := range strings.SplitSeq(strings.TrimSpace(listing), "\n") {
		if strings.HasPrefix(line, "-P ") {
			continue
		}
		target, ok := strings.CutPrefix(strings.TrimSpace(line), "-A "+from+" -j ")
		if !ok || !slices.Contains(tslinkChains, target) {
			break
		}
		jumps = append(jumps, target)
	}
	return jumps
}

// missingJumps returns the isolation's jumps that are not at the head of
// their chains; with off, only those that do not depend on the setting. A
// chain whose listing fails is reported once, not once per jump.
func missingJumps(
	ctx context.Context,
	run func(context.Context, string, ...string) ([]byte, error),
	off bool,
) []string {
	var missing []string
	listings := make(map[string][]string)
	failed := make(map[string]bool)
	for _, j := range isolationJumps {
		if off && !j.always {
			continue
		}
		key := j.cmd + " " + j.from
		if failed[key] {
			continue
		}
		jumps, listed := listings[key]
		if !listed {
			out, err := run(ctx, j.cmd, "-t", "mangle", "-S", j.from)
			if err != nil {
				missing = append(missing, fmt.Sprintf("%s -t mangle -S %s: %v: %s",
					j.cmd, j.from, err, strings.TrimSpace(string(out))))
				failed[key] = true
				continue
			}
			jumps = leadingJumps(string(out), j.from)
			listings[key] = jumps
		}
		if !slices.Contains(jumps, j.to) {
			missing = append(missing, fmt.Sprintf("%s: %s does not jump to %s first",
				j.cmd, j.from, j.to))
		}
	}
	return missing
}

// checkIsolation checks what it can of E5: no tslink plugin turns the host's
// tailnet isolation off, and tslink's isolation chains lead the mangle
// table's FORWARD and INPUT chains for IPv4 and IPv6.
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
	off := false
	for _, p := range plugins {
		if isolationOff(p.Settings.Env) {
			violations = append(violations, p.Name+": "+core.IsolateHostTailnetSetting+"=false")
			off = true
		}
	}
	violations = append(violations, missingJumps(ctx, run, off)...)
	return outcome("E5", violations, "tslink's isolation chains lead the mangle table's "+
		"FORWARD and INPUT chains for IPv4 and IPv6 on this host, and "+
		core.IsolateHostTailnetSetting+" is on")
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

// checkTagScope checks what it can of E8: no tslink plugin widens a
// cluster-credential stack's tags beyond tag:<stack>. The tailnet policy,
// which decides which tags the credential owns, is not visible from the host.
func checkTagScope(ctx context.Context, d DockerAPI) Result {
	plugins, err := tslinkPlugins(ctx, d)
	if err != nil {
		return Result{Property: "E8", Status: Error, Detail: err.Error()}
	}
	var violations []string
	for _, p := range plugins {
		for _, kv := range p.Settings.Env {
			k, v, _ := strings.Cut(kv, "=")
			if k == core.TagScopeSetting && !core.TagScopeStrict(v) {
				violations = append(violations, p.Name+": "+kv)
			}
		}
	}
	if len(violations) > 0 {
		return outcome("E8", violations, "")
	}
	return Result{
		Property: "E8", Status: Unknown,
		Detail: "the credential's tag ownership is set in the tailnet policy, not on the host",
	}
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
