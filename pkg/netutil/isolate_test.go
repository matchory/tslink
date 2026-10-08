//go:build linux

package netutil

import (
	"slices"
	"testing"

	"github.com/coreos/go-iptables/iptables"
)

// isolationState lists, for one address family in the namespace at nsPath,
// the mangle rules SetupHostIsolation and RemoveHostIsolation manage.
type isolationState struct {
	forward, input []string // the built-in chains
	fwdChain       []string // nil when the chain does not exist
	inChain        []string
}

func listIsolation(t *testing.T, nsPath string, proto iptables.Protocol) isolationState {
	t.Helper()
	var s isolationState
	if err := inNetNS(nsPath, func() error {
		ipt, err := iptables.NewWithProtocol(proto)
		if err != nil {
			return err
		}
		if s.forward, err = ipt.List("mangle", "FORWARD"); err != nil {
			return err
		}
		if s.input, err = ipt.List("mangle", "INPUT"); err != nil {
			return err
		}
		for chain, dst := range map[string]*[]string{
			isolateForwardChain: &s.fwdChain,
			isolateInputChain:   &s.inChain,
		} {
			exists, err := ipt.ChainExists("mangle", chain)
			if err != nil {
				return err
			}
			if exists {
				if *dst, err = ipt.List("mangle", chain); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("list rules: %v", err)
	}
	return s
}

// assertIsolationChain checks that chain holds exactly the family's rules for
// it, in order. iptables lists rules in its own normal form, so each is
// checked with -C and its position by the rule count.
func assertIsolationChain(
	t *testing.T,
	what, nsPath string,
	f isolationFamily,
	chain string,
	listed []string,
) {
	t.Helper()
	want := f.rules(chain)
	if len(listed) != len(want)+1 {
		t.Errorf("%s: got %d rules, want %d:\n %q", what, len(listed)-1, len(want), listed)
		return
	}
	if err := inNetNS(nsPath, func() error {
		ipt, err := iptables.NewWithProtocol(f.proto)
		if err != nil {
			return err
		}
		for _, rule := range want {
			ok, err := ipt.Exists("mangle", chain, rule...)
			if err != nil {
				return err
			}
			if !ok {
				t.Errorf("%s: missing rule %q", what, rule)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("check rules: %v", err)
	}
}

// Guards: G1
func TestHostIsolationRules(t *testing.T) {
	nsPath := newTestNetNS(t)
	for _, f := range isolationFamilies {
		t.Run(f.name, func(t *testing.T) {
			ipt := func(fn func(*iptables.IPTables) error) {
				t.Helper()
				if err := inNetNS(nsPath, func() error {
					i, err := iptables.NewWithProtocol(f.proto)
					if err != nil {
						return err
					}
					return fn(i)
				}); err != nil {
					t.Fatal(err)
				}
			}
			setup := func() {
				t.Helper()
				if err := inNetNS(nsPath, SetupHostIsolation); err != nil {
					t.Fatalf("SetupHostIsolation: %v", err)
				}
			}
			jumpForward := "-A FORWARD -j " + isolateForwardChain
			jumpInput := "-A INPUT -j " + isolateInputChain
			other := "-A FORWARD -i dummy0 -j ACCEPT"

			// A rule already in FORWARD, so the jump's position shows
			ipt(func(i *iptables.IPTables) error {
				return i.Append("mangle", "FORWARD", "-i", "dummy0", "-j", "ACCEPT")
			})

			// Twice: the watchdog repeats it, and it must not duplicate rules
			setup()
			setup()
			s := listIsolation(t, nsPath, f.proto)
			assertRules(t, "FORWARD", s.forward, []string{"-P FORWARD ACCEPT", jumpForward, other})
			assertRules(t, "INPUT", s.input, []string{"-P INPUT ACCEPT", jumpInput})
			assertIsolationChain(t, isolateForwardChain, nsPath, f, isolateForwardChain, s.fwdChain)
			assertIsolationChain(t, isolateInputChain, nsPath, f, isolateInputChain, s.inChain)

			// Another program inserts a rule ahead of the jump, and a rule of
			// the chain goes missing: setup restores both
			ipt(func(i *iptables.IPTables) error {
				if err := i.Insert(
					"mangle",
					"FORWARD",
					1,
					"-i",
					"dummy0",
					"-j",
					"ACCEPT",
				); err != nil {
					return err
				}
				return i.Delete("mangle", isolateForwardChain, f.rules(isolateForwardChain)[0]...)
			})
			setup()
			s = listIsolation(t, nsPath, f.proto)
			assertRules(t, "FORWARD after tampering", s.forward,
				[]string{"-P FORWARD ACCEPT", jumpForward, other, other})
			assertIsolationChain(t, isolateForwardChain+" after tampering", nsPath, f,
				isolateForwardChain, s.fwdChain)

			// Removal takes the jumps and chains, and tolerates their absence
			for range 2 {
				if err := inNetNS(nsPath, RemoveHostIsolation); err != nil {
					t.Fatalf("RemoveHostIsolation: %v", err)
				}
			}
			s = listIsolation(t, nsPath, f.proto)
			assertRules(t, "FORWARD after removal", s.forward,
				[]string{"-P FORWARD ACCEPT", other, other})
			assertRules(t, "INPUT after removal", s.input, []string{"-P INPUT ACCEPT"})
			if s.fwdChain != nil || s.inChain != nil {
				t.Errorf("chains still exist: %q %q", s.fwdChain, s.inChain)
			}
		})
	}
}

// Guards: G1
func TestIsolationRulesCoverEveryContainerInterface(t *testing.T) {
	for _, f := range isolationFamilies {
		for _, chain := range []string{isolateForwardChain, isolateInputChain} {
			var drops []string
			for _, rule := range f.rules(chain) {
				if rule[len(rule)-1] == "DROP" {
					drops = append(drops, rule[1])
				}
			}
			if !slices.Equal(drops, containerInterfaces) {
				t.Errorf(
					"%s %s drops traffic from %q, want %q",
					f.name,
					chain,
					drops,
					containerInterfaces,
				)
			}
		}
	}
}

// Veth isolation keeps containers from reaching each other through tslink's
// veths: from any container interface to 10.200.0.0/16, only tailscaled's
// WireGuard port passes, so colocated nodes keep their direct path.
func TestVethIsolationRules(t *testing.T) {
	nsPath := newTestNetNS(t)
	const port = 41641
	list := func(chain string) []string {
		t.Helper()
		var rules []string
		if err := inNetNS(nsPath, func() error {
			ipt, err := iptables.New()
			if err != nil {
				return err
			}
			exists, err := ipt.ChainExists("mangle", chain)
			if err != nil || !exists {
				return err
			}
			rules, err = ipt.List("mangle", chain)
			return err
		}); err != nil {
			t.Fatalf("list %s: %v", chain, err)
		}
		return rules
	}
	setupVeth := func() {
		t.Helper()
		if err := inNetNS(nsPath, func() error { return SetupVethIsolation(port) }); err != nil {
			t.Fatalf("SetupVethIsolation: %v", err)
		}
	}
	setupHost := func() {
		t.Helper()
		if err := inNetNS(nsPath, SetupHostIsolation); err != nil {
			t.Fatalf("SetupHostIsolation: %v", err)
		}
	}

	setupVeth()
	setupVeth()
	want := []string{
		"-A " + vethIsolateChain +
			" -d 10.200.0.0/16 -m conntrack --ctstate RELATED,ESTABLISHED -j RETURN",
		"-A " + vethIsolateChain + " -d 10.200.0.0/16 -i veth+ -p udp -m udp --dport 41641 -j RETURN",
		"-A " + vethIsolateChain + " -d 10.200.0.0/16 -j DROP",
	}
	assertRules(t, vethIsolateChain, list(vethIsolateChain),
		append([]string{"-N " + vethIsolateChain}, want...))

	// With the host's tailnet isolation too, both jumps lead FORWARD, and
	// setting either up again moves neither: the watchdog repeats both
	jumpVeth := "-A FORWARD -j " + vethIsolateChain
	jumpHost := "-A FORWARD -j " + isolateForwardChain
	setupHost()
	first := list("FORWARD")
	if len(first) < 3 || !slices.Contains(first[1:3], jumpVeth) ||
		!slices.Contains(first[1:3], jumpHost) {
		t.Fatalf("FORWARD does not start with both jumps: %q", first)
	}
	for range 3 {
		setupVeth()
		setupHost()
	}
	assertRules(t, "FORWARD after repeated setups", list("FORWARD"), first)

	// A rule another program puts ahead of the jumps is moved behind them
	if err := inNetNS(nsPath, func() error {
		ipt, err := iptables.New()
		if err != nil {
			return err
		}
		return ipt.Insert("mangle", "FORWARD", 1, "-i", "dummy0", "-j", "ACCEPT")
	}); err != nil {
		t.Fatal(err)
	}
	setupVeth()
	setupHost()
	got := list("FORWARD")
	if len(got) < 4 || !slices.Contains(got[1:3], jumpVeth) ||
		!slices.Contains(got[1:3], jumpHost) {
		t.Errorf("jumps not restored ahead of the other rule: %q", got)
	}

	// Removing the host's isolation leaves the veth isolation in place
	if err := inNetNS(nsPath, RemoveHostIsolation); err != nil {
		t.Fatal(err)
	}
	if got := list("FORWARD"); !slices.Contains(got, jumpVeth) || slices.Contains(got, jumpHost) {
		t.Errorf("after RemoveHostIsolation: %q", got)
	}
}
