package netutil

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/coreos/go-iptables/iptables"
)

// chainWrite is a change to a chain that iptables-restore applies at once, so
// no packet sees it half done: the chain's rules, and a jump to it as the
// first rule of a built-in chain.
type chainWrite struct {
	table, chain string
	rules        [][]string // the chain's rules, if setRules
	setRules     bool
	from         string // the built-in chain whose first rule jumps to chain; empty for none
	jumpExists   bool   // from jumps to chain already, further down
}

// script returns the iptables-restore input of w. Declaring a chain creates
// it, or empties it under --noflush, in both iptables backends; the explicit
// flush only says so.
func (w chainWrite) script() (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "*%s\n", w.table)
	lines := [][]string{}
	if w.setRules {
		fmt.Fprintf(&b, ":%s - [0:0]\n", w.chain)
		lines = append(lines, []string{"-F", w.chain})
		for _, rule := range w.rules {
			lines = append(lines, append([]string{"-A", w.chain}, rule...))
		}
	}
	if w.from != "" {
		if w.jumpExists {
			lines = append(lines, []string{"-D", w.from, "-j", w.chain})
		}
		lines = append(lines, []string{"-I", w.from, "1", "-j", w.chain})
	}
	for _, line := range lines {
		for _, arg := range line {
			if arg == "" || strings.ContainsAny(arg, " \t\n\"'#") {
				return "", fmt.Errorf("argument %q cannot be written for iptables-restore", arg)
			}
		}
		b.WriteString(strings.Join(line, " "))
		b.WriteByte('\n')
	}
	b.WriteString("COMMIT\n")
	return b.String(), nil
}

// restoreTimeout bounds one iptables-restore run, waiting for the xtables
// lock included.
const restoreTimeout = 30 * time.Second

// applyChainWrite applies w with the iptables-restore of ipt's family and
// backend, without flushing anything else.
func applyChainWrite(ipt *iptables.IPTables, w chainWrite) error {
	script, err := w.script()
	if err != nil {
		return err
	}
	bin, err := restoreCommand(ipt.Proto())
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), restoreTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "--noflush", "--wait") // #nosec G204 -- a fixed binary
	cmd.Stdin = strings.NewReader(script)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w: %s", bin, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// restoreCommand returns the iptables-restore that writes to the same backend
// as iptables, which go-iptables runs: the plugin image links iptables to
// iptables-nft, but not iptables-restore.
func restoreCommand(proto iptables.Protocol) (string, error) {
	name := "iptables"
	if proto == iptables.ProtocolIPv6 {
		name = "ip6tables"
	}
	out, err := exec.CommandContext( // #nosec G204 -- a fixed binary
		context.Background(), name, "--version",
	).Output()
	if err != nil {
		return "", fmt.Errorf("failed to get the version of %s: %w", name, err)
	}
	return restoreFor(name, string(out)), nil
}

// restoreFor returns the iptables-restore matching the backend name's version
// output names, as in "iptables v1.8.10 (nf_tables)", if it is installed.
func restoreFor(name, version string) string {
	backend := ""
	switch {
	case strings.Contains(version, "(nf_tables)"):
		backend = "-nft"
	case strings.Contains(version, "(legacy)"):
		backend = "-legacy"
	}
	if backend != "" {
		if path, err := exec.LookPath(name + backend + "-restore"); err == nil {
			return path
		}
	}
	return name + "-restore"
}
