//go:build linux

package netutil

import (
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/coreos/go-iptables/iptables"
)

// countIptables puts a wrapper for every iptables binary tslink may run on
// PATH, which logs its name and runs the real one. The returned function
// returns the names logged since its last call.
func countIptables(t *testing.T) func() []string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "log")
	for _, base := range []string{"iptables", "ip6tables"} {
		for _, backend := range []string{"", "-nft", "-legacy"} {
			for _, tool := range []string{"", "-save", "-restore"} {
				name := base + backend + tool
				bin, err := exec.LookPath(name)
				if err != nil {
					continue
				}
				script := fmt.Sprintf("#!/bin/sh\necho %s >> %s\nexec %s \"$@\"\n", name, log, bin)
				if err := os.WriteFile(
					filepath.Join(dir, name),
					[]byte(script),
					0o700,
				); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func() []string {
		b, err := os.ReadFile(log) // #nosec G304 -- the test's own file
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if err := os.Remove(log); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		return strings.Fields(string(b))
	}
}

// watchdogRun does what the watchdog does with s's rules on every run.
func (s *hostState) watchdogRun(bridges []string) error {
	if err := s.setupHostIsolation(bridges); err != nil {
		return err
	}
	return s.setupVethIsolation(WireGuardTestPort)
}

// A watchdog run that finds everything in place, and setting up a new
// container, start few iptables processes, and the host endpoints none: each
// runs for every container on the host, and the watchdog and the host
// endpoints periodically. Before, they took 66, 12 and 3.
func TestIptablesProcessCount(t *testing.T) {
	nsPath := newTestNetNS(t)
	ran := countIptables(t)
	s := newHostState()
	inNS := func(name string, fn func() error) {
		t.Helper()
		if err := inNetNS(nsPath, fn); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	inNS("add a veth", func() error {
		return addLinks(map[string]string{"vethtslinka": "10.200.0.1/30"})
	})
	inNS("SetupNAT", func() error { return s.setupNAT("vethtslinka") })
	inNS("first watchdog run", func() error { return s.watchdogRun(nil) })
	ran()

	inNS("steady watchdog run", func() error { return s.watchdogRun(nil) })
	if got := ran(); len(got) > 6 {
		t.Errorf("steady watchdog run: %d processes, want at most 6: %q", len(got), got)
	} else {
		t.Logf("steady watchdog run: %d processes: %q", len(got), got)
	}

	inNS("add another veth", func() error {
		return addLinks(map[string]string{"vethtslinkb": "10.200.0.5/30"})
	})
	inNS("SetupNAT of a new container", func() error { return s.setupNAT("vethtslinkb") })
	if got := ran(); len(got) > 6 {
		t.Errorf("new container: %d processes, want at most 6: %q", len(got), got)
	} else {
		t.Logf("new container: %d processes: %q", len(got), got)
	}

	s.hostEndpoints = HostEndpointsConfig{Addrs: []netip.Addr{netip.MustParseAddr("192.0.2.1")}}
	inNS("HostEndpoints", func() error {
		eps, err := s.hostEndpointsOf("vethtslinkb")
		if err == nil && len(eps) != 1 {
			err = fmt.Errorf("HostEndpoints = %v, want one", eps)
		}
		return err
	})
	if got := ran(); len(got) > 0 {
		t.Errorf("HostEndpoints: %d processes, want none: %q", len(got), got)
	}
}

// The containers one sync maps get their flows reset with one pass through
// the conntrack table, made after portMapMu is released, and a failure to
// reset them does not fail their mapping.
func TestNewMappingsForgetFlowsOnce(t *testing.T) {
	nsPath := newTestNetNS(t)
	s := newHostState()
	var calls [][]netip.Addr
	var fail error
	s.forgetFlows = func(srcs []netip.Addr, sport int) (uint, error) {
		if !s.portMapMu.TryLock() {
			t.Error("conntrack flushed with portMapMu held")
		} else {
			s.portMapMu.Unlock()
		}
		if sport != 41641 {
			t.Errorf("sport = %d, want 41641", sport)
		}
		calls = append(calls, slices.Clone(srcs))
		return 0, fail
	}
	sync := func() error {
		f, err := s.family(iptables.ProtocolIPv4)
		if err != nil {
			return err
		}
		return s.syncPortMappings(f)
	}
	if err := inNetNS(nsPath, func() error {
		if err := addLinks(map[string]string{
			"vethtslinka": "10.200.0.1/30",
			"vethtslinkb": "10.200.0.5/30",
			"vethtslinkc": "10.200.0.9/30",
		}); err != nil {
			return err
		}
		if err := sync(); err != nil {
			return err
		}
		// Nothing new: no flush
		if err := sync(); err != nil {
			return err
		}
		// A flush that fails is a warning
		fail = errors.New("conntrack failed")
		if err := addLinks(map[string]string{"vethtslinkd": "10.200.0.13/30"}); err != nil {
			return err
		}
		return sync()
	}); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || len(calls[0]) != 3 || len(calls[1]) != 1 {
		t.Fatalf("conntrack calls = %v, want one with three addresses, then one with one", calls)
	}
	for _, src := range []string{"10.200.0.2", "10.200.0.6", "10.200.0.10"} {
		if !slices.Contains(calls[0], netip.MustParseAddr(src)) {
			t.Errorf("first conntrack call %v lacks %s", calls[0], src)
		}
	}
	if len(s.ports) != 4 {
		t.Errorf("mappings = %v, want four", s.ports)
	}
}

// A container whose mapping is in the chain but not in the cache, as when
// the sync at start failed, gets its port into the cache when SetupNAT finds
// the mapping, rather than only with the next watchdog run.
func TestSetupNATCachesExistingMapping(t *testing.T) {
	nsPath := newTestNetNS(t)
	if err := inNetNS(nsPath, func() error {
		if err := addLinks(map[string]string{"vethtslinka": "10.200.0.1/30"}); err != nil {
			return err
		}
		if err := newHostState().setupNAT("vethtslinka"); err != nil {
			return err
		}
		s := newHostState()
		s.hostEndpoints = HostEndpointsConfig{Addrs: []netip.Addr{netip.MustParseAddr("192.0.2.1")}}
		if err := s.setupNAT("vethtslinka"); err != nil {
			return err
		}
		eps, err := s.hostEndpointsOf("vethtslinka")
		if err == nil && len(eps) != 1 {
			err = fmt.Errorf("HostEndpoints = %v, want one", eps)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// Once the watchdog remembered the chains as it found them, another
// program's changes are still found and repaired: a rule added, a rule
// removed, a rule replaced by another, and a rule put ahead of the jumps.
//
// Guards: G1, G2
func TestRememberedChainsStillChecked(t *testing.T) {
	nsPath := newTestNetNS(t)
	s := newHostState()
	run := func(what string) {
		t.Helper()
		if err := inNetNS(nsPath, func() error { return s.watchdogRun(nil) }); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	list := func() map[iptables.Protocol]tableRules {
		t.Helper()
		out := map[iptables.Protocol]tableRules{}
		if err := inNetNS(nsPath, func() error {
			for _, fam := range isolationFamilies {
				f, err := s.family(fam.proto)
				if err != nil {
					return err
				}
				if out[fam.proto], err = f.list("mangle"); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}
	tamper := func(proto iptables.Protocol, fn func(*iptables.IPTables) error) {
		t.Helper()
		if err := inNetNS(nsPath, func() error {
			ipt, err := iptables.NewWithProtocol(proto)
			if err != nil {
				return err
			}
			return fn(ipt)
		}); err != nil {
			t.Fatal(err)
		}
	}
	run("first run")
	run("second run")
	if len(s.checked) == 0 {
		t.Fatal("no chain remembered")
	}
	want := list()
	assertRepaired := func(what string) {
		t.Helper()
		run(what)
		got := list()
		for proto, w := range want {
			for _, chain := range tslinkChains {
				assertRules(t, fmt.Sprintf("%s: %v %s", what, proto, chain),
					got[proto].rules[chain], w.rules[chain])
			}
			for _, from := range []string{forwardChain, inputChain} {
				for _, chain := range tslinkChains {
					if first, jumps := jumpPosition(
						got[proto].rules[from], from, chain,
					); jumps && !first {
						t.Errorf("%s: %v %s does not jump to %s first: %q",
							what, proto, from, chain, got[proto].rules[from])
					}
				}
			}
		}
	}
	junk := []string{"-d", "192.0.2.1/32", "-j", returnTarget}
	v4 := isolationFamilies[0]

	tamper(iptables.ProtocolIPv4, func(i *iptables.IPTables) error {
		return i.Append("mangle", VethIsolateChain, junk...)
	})
	assertRepaired("a rule added")

	for _, fam := range isolationFamilies {
		tamper(fam.proto, func(i *iptables.IPTables) error {
			return i.Delete("mangle", IsolateForwardChain,
				fam.rules(IsolateForwardChain, containerInterfaces)[0]...)
		})
	}
	assertRepaired("a rule removed")

	tamper(iptables.ProtocolIPv4, func(i *iptables.IPTables) error {
		return errors.Join(
			i.Delete("mangle", IsolateInputChain,
				v4.rules(IsolateInputChain, containerInterfaces)[0]...),
			i.Append("mangle", IsolateInputChain, junk...),
		)
	})
	assertRepaired("a rule replaced")

	for _, fam := range isolationFamilies {
		tamper(fam.proto, func(i *iptables.IPTables) error {
			return errors.Join(
				i.Insert("mangle", forwardChain, 1, "-i", "dummy0", "-j", "ACCEPT"),
				i.Insert("mangle", inputChain, 1, "-i", "dummy0", "-j", "ACCEPT"),
			)
		})
	}
	assertRepaired("jumps pushed down")
}
