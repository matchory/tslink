//go:build linux

package netutil

import (
	"maps"
	"net/netip"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/coreos/go-iptables/iptables"
	"github.com/vishvananda/netlink"
)

// natRules lists the iptables rules SetupNAT and the cleanup functions
// manage, in the namespace at nsPath.
type natRules struct {
	ipForward   string
	forward     []string
	chain       []string // nil when the chain does not exist
	postrouting []string
	portMap     map[netip.Addr]int // the mappings in portMapChain
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
		if r.postrouting, err = ipt.List("nat", "POSTROUTING"); err != nil {
			return err
		}
		r.portMap, err = listMappings(ipt)
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

	// The host ends of three veths, with their addresses
	addrs := map[string]string{
		"vethtslinka": "10.200.0.1/30",
		"vethtslinkb": "10.200.0.5/30",
		"vethtslinkc": "10.200.0.9/30",
	}
	inNS("add veths", func() error { return addLinks(addrs) })
	ctrA, ctrB, ctrC := netip.MustParseAddr("10.200.0.2"), netip.MustParseAddr("10.200.0.6"),
		netip.MustParseAddr("10.200.0.10")

	inNS("SetupNAT", func() error {
		if err := SetupNAT("vethtslinka"); err != nil {
			return err
		}
		return SetupNAT("vethtslinkb")
	})

	jump := "-A FORWARD -j " + chainName
	drop := "-A FORWARD -i dummy0 -j DROP"
	masquerade := "-A POSTROUTING -s 10.200.0.0/16 -j MASQUERADE"
	portJump := "-A POSTROUTING -j " + portMapChain
	postrouting := []string{"-P POSTROUTING ACCEPT", portJump, masquerade}
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
	assertRules(t, "POSTROUTING", r.postrouting, postrouting)
	mapped := r.portMap
	if len(mapped) != 2 || mapped[ctrA] == mapped[ctrB] ||
		!DefaultPortRange.Contains(mapped[ctrA]) || !DefaultPortRange.Contains(mapped[ctrB]) {
		t.Fatalf("port mappings = %v, want two ports in %s", mapped, DefaultPortRange)
	}

	// Setting a veth up again keeps its port
	inNS("SetupNAT again", func() error { return SetupNAT("vethtslinka") })
	if got := listNATRules(t, nsPath).portMap; !maps.Equal(got, mapped) {
		t.Errorf("port mappings after SetupNAT again = %v, want %v", got, mapped)
	}

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
	assertRules(t, "POSTROUTING after restart", r.postrouting, postrouting)
	if r.portMap[ctrA] != mapped[ctrA] || r.portMap[ctrB] != mapped[ctrB] || len(r.portMap) != 3 {
		t.Errorf(
			"port mappings after restart = %v, want those of %v and one more",
			r.portMap,
			mapped,
		)
	}
	mapped = r.portMap

	// SyncPortMappings maps a veth an older version set up and removes the
	// mapping of an address no veth has
	inNS("simulate an old veth and a stale mapping", func() error {
		ipt, err := iptables.New()
		if err != nil {
			return err
		}
		if err := ipt.Delete("nat", portMapChain, portMap.rule(ctrB, mapped[ctrB])...); err != nil {
			return err
		}
		return ipt.Append(
			"nat",
			portMapChain,
			portMap.rule(netip.MustParseAddr("10.200.9.2"), 1)...)
	})
	inNS("SyncPortMappings", SyncPortMappings)
	r = listNATRules(t, nsPath)
	if len(r.portMap) != 3 || r.portMap[ctrA] != mapped[ctrA] || r.portMap[ctrC] != mapped[ctrC] ||
		r.portMap[ctrB] == 0 || r.portMap[ctrB] == mapped[ctrA] || r.portMap[ctrB] == mapped[ctrC] {
		t.Errorf("port mappings after SyncPortMappings = %v, want a, b and c mapped", r.portMap)
	}
	assertRules(t, "POSTROUTING after SyncPortMappings", r.postrouting, postrouting)

	// CleanupNAT removes one veth's rules, and tolerates their absence.
	for range 2 {
		inNS("CleanupNAT", func() error { return CleanupNAT("vethtslinka") })
	}

	r = listNATRules(t, nsPath)
	assertRules(t, "FORWARD after CleanupNAT", r.forward, []string{"-P FORWARD ACCEPT", jump, drop})
	assertRules(t, chainName+" after CleanupNAT", r.chain,
		slices.Concat([]string{"-N " + chainName}, accept("vethtslinkb"), accept("vethtslinkc")))
	assertRules(t, "POSTROUTING after CleanupNAT", r.postrouting, postrouting)
	if _, ok := r.portMap[ctrA]; ok || len(r.portMap) != 2 {
		t.Errorf("port mappings after CleanupNAT = %v, want b's and c's", r.portMap)
	}

	// The mapping of a veth that is gone already goes too
	inNS("delete veth c", func() error { return DeleteVeth("vethtslinkc") })
	inNS("CleanupNAT of a deleted veth", func() error { return CleanupNAT("vethtslinkc") })
	if got := listNATRules(t, nsPath).portMap; len(got) != 1 || got[ctrB] == 0 {
		t.Errorf("port mappings after CleanupNAT of a deleted veth = %v, want b's", got)
	}

	// CleanupAllNAT removes the chains, the jumps and MASQUERADE, and
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
	if len(r.portMap) != 0 {
		t.Errorf("port mappings after CleanupAllNAT = %v", r.portMap)
	}
}

// addLinks adds a dummy link for each name, with its address, in the
// current network namespace.
func addLinks(addrs map[string]string) error {
	for name, addr := range addrs {
		link := &netlink.Dummy{Name: name}
		if err := netlink.LinkAdd(link); err != nil {
			return err
		}
		a, err := netlink.ParseAddr(addr)
		if err != nil {
			return err
		}
		if err := netlink.AddrAdd(link, a); err != nil {
			return err
		}
	}
	return nil
}

// UsedVethSubnets lists the /30s in tslink's range on any link of the
// namespace, and VethIPv4 reads a link's address back.
func TestUsedVethSubnets(t *testing.T) {
	nsPath := newTestNetNS(t)
	if err := inNetNS(nsPath, func() error {
		for name, addr := range map[string]string{
			"dummy1": "10.200.1.5/30", "dummy2": "10.200.2.9/30", "dummy3": "192.168.0.1/24",
		} {
			link := &netlink.Dummy{Name: name}
			if err := netlink.LinkAdd(link); err != nil {
				return err
			}
			a, _ := netlink.ParseAddr(addr)
			if err := netlink.AddrAdd(link, a); err != nil {
				return err
			}
		}
		used, err := UsedVethSubnets()
		if err != nil {
			return err
		}
		want := map[netip.Prefix]bool{
			netip.MustParsePrefix("10.200.1.4/30"): true,
			netip.MustParsePrefix("10.200.2.8/30"): true,
		}
		if !maps.Equal(used, want) {
			t.Errorf("used = %v, want %v", used, want)
		}
		ip, err := VethIPv4("dummy2")
		if err != nil || ip != "10.200.2.9" {
			t.Errorf("VethIPv4 = %q, %v", ip, err)
		}
		// The address is the host side's: a second link may not get it
		if err := SetupHostRouting("dummy3", "10.200.1.5"); err != nil {
			t.Errorf("SetupHostRouting on a free link: %v", err)
		}
		if err := SetupHostRouting("dummy3", "10.200.1.5"); err == nil {
			t.Error("SetupHostRouting did not fail adding an address the link has")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
