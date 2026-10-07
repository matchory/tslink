//go:build linux

package netutil

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/coreos/go-iptables/iptables"
)

// natRules lists the iptables rules SetupNAT and the cleanup functions
// manage, in the namespace at nsPath.
type natRules struct {
	ipForward   string
	forward     []string
	chain       []string // nil when the chain does not exist
	postrouting []string
}

func listNATRules(t *testing.T, nsPath string) natRules {
	t.Helper()
	var r natRules
	if err := inNetNS(nsPath, func() error {
		b, err := os.ReadFile("/proc/sys/net/ipv4/ip_forward")
		if err != nil {
			return err
		}
		r.ipForward = strings.TrimSpace(string(b))

		ipt, err := iptables.New()
		if err != nil {
			return err
		}
		if r.forward, err = ipt.List("filter", "FORWARD"); err != nil {
			return err
		}
		exists, err := ipt.ChainExists("filter", chainName)
		if err != nil {
			return err
		}
		if exists {
			if r.chain, err = ipt.List("filter", chainName); err != nil {
				return err
			}
		}
		r.postrouting, err = ipt.List("nat", "POSTROUTING")
		return err
	}); err != nil {
		t.Fatalf("list rules: %v", err)
	}
	return r
}

func assertRules(t *testing.T, what string, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("%s:\n got  %q\n want %q", what, got, want)
	}
}

// resetChainInitialized makes SetupNAT set up the chain again, as after a
// plugin restart.
func resetChainInitialized() {
	chainMu.Lock()
	chainInitialized = false
	chainMu.Unlock()
}

func TestNATRules(t *testing.T) {
	nsPath := newTestNetNS(t)
	resetChainInitialized()
	t.Cleanup(resetChainInitialized)

	inNS := func(name string, fn func() error) {
		t.Helper()
		if err := inNetNS(nsPath, fn); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}

	// A rule already in FORWARD, so the jump's position shows.
	inNS("add existing FORWARD rule", func() error {
		ipt, err := iptables.New()
		if err != nil {
			return err
		}
		return ipt.Append("filter", "FORWARD", "-i", "dummy0", "-j", "DROP")
	})

	inNS("SetupNAT", func() error {
		if err := SetupNAT("vethtslinka"); err != nil {
			return err
		}
		return SetupNAT("vethtslinkb")
	})

	jump := "-A FORWARD -j " + chainName
	drop := "-A FORWARD -i dummy0 -j DROP"
	masquerade := "-A POSTROUTING -s 10.200.0.0/16 -j MASQUERADE"
	accept := func(veth string) []string {
		return []string{
			"-A " + chainName + " -i " + veth + " -j ACCEPT",
			"-A " + chainName + " -o " + veth + " -j ACCEPT",
		}
	}

	r := listNATRules(t, nsPath)
	if r.ipForward != "1" {
		t.Errorf("ip_forward = %q, want 1", r.ipForward)
	}
	assertRules(t, "FORWARD", r.forward, []string{"-P FORWARD ACCEPT", jump, drop})
	assertRules(t, chainName, r.chain,
		slices.Concat([]string{"-N " + chainName}, accept("vethtslinka"), accept("vethtslinkb")))
	assertRules(t, "POSTROUTING", r.postrouting, []string{"-P POSTROUTING ACCEPT", masquerade})

	// After a plugin restart the chain and global rules exist already; they
	// must not be duplicated.
	resetChainInitialized()
	inNS("SetupNAT after restart", func() error { return SetupNAT("vethtslinkc") })

	r = listNATRules(t, nsPath)
	assertRules(t, "FORWARD after restart", r.forward, []string{"-P FORWARD ACCEPT", jump, drop})
	assertRules(
		t,
		chainName+" after restart",
		r.chain,
		slices.Concat(
			[]string{"-N " + chainName},
			accept("vethtslinka"),
			accept("vethtslinkb"),
			accept("vethtslinkc"),
		),
	)
	assertRules(
		t,
		"POSTROUTING after restart",
		r.postrouting,
		[]string{"-P POSTROUTING ACCEPT", masquerade},
	)

	// CleanupNAT removes one veth's rules, and tolerates their absence.
	for range 2 {
		inNS("CleanupNAT", func() error { return CleanupNAT("vethtslinka") })
	}

	r = listNATRules(t, nsPath)
	assertRules(t, "FORWARD after CleanupNAT", r.forward, []string{"-P FORWARD ACCEPT", jump, drop})
	assertRules(t, chainName+" after CleanupNAT", r.chain,
		slices.Concat([]string{"-N " + chainName}, accept("vethtslinkb"), accept("vethtslinkc")))
	assertRules(
		t,
		"POSTROUTING after CleanupNAT",
		r.postrouting,
		[]string{"-P POSTROUTING ACCEPT", masquerade},
	)

	// CleanupAllNAT removes the chain, the jump and MASQUERADE, and
	// tolerates their absence.
	for range 2 {
		inNS("CleanupAllNAT", CleanupAllNAT)
	}

	r = listNATRules(t, nsPath)
	assertRules(t, "FORWARD after CleanupAllNAT", r.forward, []string{"-P FORWARD ACCEPT", drop})
	if r.chain != nil {
		t.Errorf("chain %s still exists: %q", chainName, r.chain)
	}
	assertRules(
		t,
		"POSTROUTING after CleanupAllNAT",
		r.postrouting,
		[]string{"-P POSTROUTING ACCEPT"},
	)
}
