package netutil

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
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
// lock included, and one iptables-save run.
const restoreTimeout = 30 * time.Second

// apply applies w with the iptables-restore of f's family and backend,
// without flushing anything else.
func (f *ipFamily) apply(w chainWrite) error {
	script, err := w.script()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), restoreTimeout)
	defer cancel()
	cmd := exec.CommandContext( // #nosec G204 -- a fixed binary
		ctx, f.restore, "--noflush", "--wait",
	)
	cmd.Stdin = strings.NewReader(script)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w: %s", f.restore, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}
