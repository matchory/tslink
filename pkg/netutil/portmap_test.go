package netutil

import (
	"maps"
	"net/netip"
	"slices"
	"testing"
)

func TestParsePortRange(t *testing.T) {
	for in, want := range map[string]PortRange{
		"61000-65535":   {61000, 65535},
		" 1024-1024 ":   {1024, 1024},
		"50000 - 50009": {50000, 50009},
	} {
		got, err := ParsePortRange(in)
		if err != nil || got != want {
			t.Errorf("ParsePortRange(%q) = %v, %v, want %v", in, got, err, want)
		}
	}
	for _, in := range []string{
		"", "61000", "65535-61000", "0-10", "1023-2000", "61000-65536", "a-b", "1-2-3", "-5",
	} {
		if got, err := ParsePortRange(in); err == nil {
			t.Errorf("ParsePortRange(%q) = %v, want an error", in, got)
		}
	}
}

func TestPortRangeContains(t *testing.T) {
	r := PortRange{100, 200}
	for port, want := range map[int]bool{99: false, 100: true, 150: true, 200: true, 201: false} {
		if got := r.Contains(port); got != want {
			t.Errorf("%v.Contains(%d) = %v, want %v", r, port, got, want)
		}
	}
	if s := r.String(); s != "100-200" {
		t.Errorf("String() = %q", s)
	}
}

// Each source gets the port it already has, or one no other source has,
// from its preferred port on, wrapping around within the range.
func TestPickPort(t *testing.T) {
	r := PortRange{1000, 1003}
	a, b := netip.MustParseAddr("10.200.0.2"), netip.MustParseAddr("10.200.0.6")

	// Deterministic: the same source prefers the same port
	first, err := pickPort(a, r, nil)
	if err != nil || !r.Contains(first) {
		t.Fatalf("pickPort = %d, %v", first, err)
	}
	if again, _ := pickPort(a, r, nil); again != first {
		t.Errorf("pickPort is not deterministic: %d, then %d", first, again)
	}
	if p := preferredPort(a, r); p != first {
		t.Errorf("free range: got %d, want the preferred %d", first, p)
	}

	// Recovery: an existing mapping is kept
	mapped := map[netip.Addr]int{a: 1002}
	if got, err := pickPort(a, r, mapped); err != nil || got != 1002 {
		t.Errorf("existing mapping: got %d, %v, want 1002", got, err)
	}

	// Ports in use are skipped, wrapping around to the range's start
	mapped = map[netip.Addr]int{}
	for i, p := 0, preferredPort(b, r); i < 3; i++ {
		mapped[netip.AddrFrom4([4]byte{10, 200, 1, byte(i)})] = p
		p = r.First + (p-r.First+1)%r.size()
	}
	got, err := pickPort(b, r, mapped)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range mapped {
		if got == p {
			t.Errorf("pickPort = %d, which another source uses (%v)", got, mapped)
		}
	}
	if want := r.First + (preferredPort(b, r)-r.First+3)%r.size(); got != want {
		t.Errorf("pickPort = %d, want %d, the next free port after wrapping around", got, want)
	}

	// A full range gives an error
	mapped[netip.MustParseAddr("10.200.2.2")] = got
	if p, err := pickPort(b, r, mapped); err == nil {
		t.Errorf("full range: pickPort = %d, want an error", p)
	}
}

// Ports of many sources are unique.
func TestPickPortUnique(t *testing.T) {
	r := PortRange{61000, 61099}
	mapped := map[netip.Addr]int{}
	used := map[int]bool{}
	for i := range 100 {
		src := netip.AddrFrom4([4]byte{10, 200, byte(i / 64), byte(i%64*4 + 2)})
		p, err := pickPort(src, r, mapped)
		if err != nil {
			t.Fatalf("source %d: %v", i, err)
		}
		if used[p] || !r.Contains(p) {
			t.Fatalf("source %d got port %d, used %v or outside %v", i, p, used[p], r)
		}
		used[p] = true
		mapped[src] = p
	}
}

// parseMappings reads the source and port of each rule back, as iptables
// lists them.
func TestParseMappings(t *testing.T) {
	listed := []string{
		"-N " + portMapChain,
		"-A " + portMapChain + " -s 10.200.0.2/32 -p udp -m udp --sport 41641" +
			" -j MASQUERADE --to-ports 61234",
		"-A " + portMapChain + " -s 10.200.0.6/32 -p udp -m udp --sport 41641" +
			" -j MASQUERADE --to-ports 62000-62000",
		"-A " + portMapChain + " -s 10.200.0.10/32 -j RETURN",
		"-A " + portMapChain + " -s garbage/32 -p udp -j MASQUERADE --to-ports 1",
	}
	want := map[netip.Addr]int{
		netip.MustParseAddr("10.200.0.2"): 61234,
		netip.MustParseAddr("10.200.0.6"): 62000,
	}
	if got := parseMappings(listed); !maps.Equal(got, want) {
		t.Errorf("parseMappings = %v, want %v", got, want)
	}
}

// The rule matches the container's tailscaled only.
func TestMappingRule(t *testing.T) {
	m := portMapping{sport: 41641, ports: DefaultPortRange}
	got := m.rule(netip.MustParseAddr("10.200.0.2"), 61234)
	want := []string{
		"-s", "10.200.0.2/32", "-p", "udp", "-m", "udp", "--sport", "41641",
		"-j", "MASQUERADE", "--to-ports", "61234",
	}
	if !slices.Equal(got, want) {
		t.Errorf("rule = %q, want %q", got, want)
	}
}
