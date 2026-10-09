package netutil

import (
	"errors"
	"fmt"
	"hash/fnv"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/coreos/go-iptables/iptables"

	"github.com/matchory/tslink/pkg/logger"
)

// Port mapping gives each container's tailscaled an external port of its own.
// Every tailscaled uses the same UDP port, and the global MASQUERADE keeps a
// source port only while no other flow from the host uses it towards the same
// destination: the host's own tailscaled or another container's. So towards
// STUN servers a container leaves on a remapped port, which it advertises,
// and towards a new peer on its own: peers send to a port the host does not
// map back, and hole punching between nodes on different hosts fails. A rule
// for each container maps its tailscaled's port to one no other container
// on the host has, whatever the destination.
//
// The rules live in portMapChain in nat POSTROUTING, jumped to ahead of the
// global MASQUERADE. They only rewrite the source of outgoing traffic: replies
// come back as any other masqueraded traffic does, and nothing new reaches a
// container.
const portMapChain = "TSLINK-WG-SNAT"

// PortRange is an inclusive range of ports.
type PortRange struct{ First, Last int }

// DefaultPortRange holds the external ports of tailscaled's WireGuard
// traffic. It lies above Linux's default ephemeral range (32768-60999), so
// the host's own connections do not use them.
var DefaultPortRange = PortRange{61000, 65535}

// ParsePortRange parses "first-last", ports from 1024 to 65535.
func ParsePortRange(s string) (PortRange, error) {
	first, last, ok := strings.Cut(s, "-")
	if !ok {
		return PortRange{}, fmt.Errorf("%q is no range first-last", s)
	}
	var r PortRange
	var err1, err2 error
	r.First, err1 = strconv.Atoi(strings.TrimSpace(first))
	r.Last, err2 = strconv.Atoi(strings.TrimSpace(last))
	if err := errors.Join(err1, err2); err != nil {
		return PortRange{}, fmt.Errorf("%q is no range first-last: %w", s, err)
	}
	if r.First < 1024 || r.Last > 65535 || r.First > r.Last {
		return PortRange{}, fmt.Errorf("%q is no range of ports from 1024 to 65535", s)
	}
	return r, nil
}

// Contains reports whether port is in the range.
func (r PortRange) Contains(port int) bool { return r.First <= port && port <= r.Last }

func (r PortRange) String() string { return fmt.Sprintf("%d-%d", r.First, r.Last) }

func (r PortRange) size() int { return r.Last - r.First + 1 }

// portMapping is what the rules need: tailscaled's port in the containers,
// and the external ports they are mapped to.
type portMapping struct {
	sport int
	ports PortRange
}

// udp is the protocol of tailscaled's WireGuard traffic.
const udp = "udp"

// rule returns the rule that maps src's tailscaled to port.
func (m portMapping) rule(src netip.Addr, port int) []string {
	return []string{
		"-s", netip.PrefixFrom(src, 32).String(), "-p", udp, "-m", udp,
		"--sport", strconv.Itoa(m.sport), "-j", "MASQUERADE", "--to-ports", strconv.Itoa(port),
	}
}

var (
	// portMapMu makes choosing a port and adding its rule one step.
	portMapMu sync.Mutex
	// portMap is the mapping SetupNAT installs.
	portMap = portMapping{sport: 41641, ports: DefaultPortRange}
)

// ConfigurePortMapping sets tailscaled's UDP port in the containers and the
// range of external ports SetupNAT maps it to. Rules in place keep their
// ports.
func ConfigurePortMapping(sport int, ports PortRange) {
	portMapMu.Lock()
	defer portMapMu.Unlock()
	portMap = portMapping{sport: sport, ports: ports}
}

// preferredPort is the port src's search starts at.
func preferredPort(src netip.Addr, r PortRange) int {
	h := fnv.New32a()
	b := src.As4()
	h.Write(b[:])                                    // A hash's Write never fails
	return r.First + int(h.Sum32()%uint32(r.size())) //nolint:gosec // size is at most 65535
}

// pickPort returns the port mapped holds for src, or else the first port in r,
// from src's preferred one on, that mapped holds for no source.
func pickPort(src netip.Addr, r PortRange, mapped map[netip.Addr]int) (int, error) {
	if p, ok := mapped[src]; ok {
		return p, nil
	}
	used := make(map[int]bool, len(mapped))
	for _, p := range mapped {
		used[p] = true
	}
	start := preferredPort(src, r)
	for i := range r.size() {
		if p := r.First + (start-r.First+i)%r.size(); !used[p] {
			return p, nil
		}
	}
	return 0, fmt.Errorf("no free port in %s", r)
}

// mappingRule matches a rule of portMapChain as iptables lists it.
var mappingRule = regexp.MustCompile(
	`^-A ` + portMapChain + ` -s (\S+)/32 .*-j MASQUERADE --to-ports (\d+)(?:-\d+)?$`,
)

// parseMappings returns the source and port of each mapping among the rules
// iptables lists.
func parseMappings(listed []string) map[netip.Addr]int {
	mapped := map[netip.Addr]int{}
	for _, rule := range listed {
		m := mappingRule.FindStringSubmatch(rule)
		if m == nil {
			continue
		}
		src, err := netip.ParseAddr(m[1])
		port, perr := strconv.Atoi(m[2])
		if err == nil && perr == nil {
			mapped[src] = port
		}
	}
	return mapped
}

// listMappings returns the mappings in portMapChain, none if it does not exist.
func listMappings(ipt *iptables.IPTables) (map[netip.Addr]int, error) {
	exists, err := ipt.ChainExists("nat", portMapChain)
	if err != nil || !exists {
		return map[netip.Addr]int{}, err
	}
	listed, err := ipt.List("nat", portMapChain)
	if err != nil {
		return nil, fmt.Errorf("failed to list chain %s: %w", portMapChain, err)
	}
	return parseMappings(listed), nil
}

// ensurePortMapChain creates portMapChain unless it exists, and jumps to it
// from the start of nat POSTROUTING, ahead of the global MASQUERADE.
func ensurePortMapChain(ipt *iptables.IPTables) error {
	exists, err := ipt.ChainExists("nat", portMapChain)
	if err != nil {
		return fmt.Errorf("failed to check chain %s: %w", portMapChain, err)
	}
	if !exists {
		if err := ipt.NewChain("nat", portMapChain); err != nil {
			return fmt.Errorf("failed to create chain %s: %w", portMapChain, err)
		}
	}
	if err := ipt.InsertUnique("nat", "POSTROUTING", 1, "-j", portMapChain); err != nil {
		return fmt.Errorf("failed to add jump to %s: %w", portMapChain, err)
	}
	return nil
}

// containerAddr returns the container's address on the veth whose host end
// is vethHost: the one after the host's in their /30.
func containerAddr(vethHost string) (netip.Addr, error) {
	host, err := VethIPv4(vethHost)
	if err != nil {
		return netip.Addr{}, err
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("failed to parse the address of %s: %w", vethHost, err)
	}
	return ip.Next(), nil
}

// setupPortMapping maps the tailscaled behind vethHost to a port of its own,
// or keeps the port it has.
func setupPortMapping(ipt *iptables.IPTables, vethHost string) error {
	src, err := containerAddr(vethHost)
	if err != nil {
		return err
	}
	return mapContainers(ipt, []netip.Addr{src})
}

// SyncPortMappings maps the tailscaled behind each of the host's veths to a
// port of its own, unless it has one, and removes the mappings of addresses
// no veth has. It changes nothing that is in place, so it can run
// periodically: it maps containers whose veth an older version set up, and
// removes the mappings of veths that went away with their container.
func SyncPortMappings() error {
	ipt, err := iptables.NewWithProtocol(iptables.ProtocolIPv4)
	if err != nil {
		return fmt.Errorf("failed to initialize iptables: %w", err)
	}
	used, err := UsedVethSubnets()
	if err != nil {
		return err
	}
	srcs := make([]netip.Addr, 0, len(used))
	for subnet := range used {
		// The host's address is the first in the /30, the container's the second
		srcs = append(srcs, subnet.Addr().Next().Next())
	}
	return mapContainers(ipt, srcs)
}

// mapContainers maps the tailscaled at each address in srcs to a port of its
// own, unless it has one. It first removes the mappings of addresses no veth
// has any more, whose ports are free again.
func mapContainers(ipt *iptables.IPTables, srcs []netip.Addr) error {
	portMapMu.Lock()
	defer portMapMu.Unlock()
	if err := ensurePortMapChain(ipt); err != nil {
		return err
	}
	mapped, err := pruneMappings(ipt)
	if err != nil {
		return err
	}
	var errs []error
	for _, src := range srcs {
		if _, ok := mapped[src]; !ok {
			errs = append(errs, mapContainer(ipt, src, mapped))
		}
	}
	return errors.Join(errs...)
}

// mapContainer maps the tailscaled at src, which mapped has no port for, to a
// free one, and records it in mapped. The flows it has already, as one an
// older version set up or a restarted tailscaled on the same port, keep their
// NAT until their conntrack entries go, which tailscaled's keepalives never
// let happen: mapContainer deletes them, so their next packets leave on the
// new port.
func mapContainer(ipt *iptables.IPTables, src netip.Addr, mapped map[netip.Addr]int) error {
	port, err := pickPort(src, portMap.ports, mapped)
	if err != nil {
		return fmt.Errorf("failed to map %s: %w", src, err)
	}
	if err := ipt.Append("nat", portMapChain, portMap.rule(src, port)...); err != nil {
		return fmt.Errorf("failed to map %s to port %d: %w", src, port, err)
	}
	mapped[src] = port
	logger.Infof("Mapped the WireGuard port of %s to %d", src, port)
	n, err := forgetUDPFlows(src, portMap.sport)
	if err != nil {
		return err
	}
	if n > 0 {
		logger.Infof("Reset %d flows of %s to its new WireGuard port", n, src)
	}
	return nil
}

// pruneMappings removes the mappings of addresses outside the /30s of the
// host's veths, and returns the others. The caller holds portMapMu.
func pruneMappings(ipt *iptables.IPTables) (map[netip.Addr]int, error) {
	mapped, err := listMappings(ipt)
	if err != nil {
		return nil, err
	}
	used, err := UsedVethSubnets()
	if err != nil {
		return nil, err
	}
	var errs []error
	for src, port := range mapped {
		if used[netip.PrefixFrom(src, 30).Masked()] {
			continue
		}
		logger.Infof("Removing the WireGuard port mapping of %s: no veth has its address", src)
		if err := deleteMapping(ipt, src, port); err != nil {
			errs = append(errs, err)
		}
		delete(mapped, src)
	}
	return mapped, errors.Join(errs...)
}

// deleteMapping removes the rule that maps src to port, whatever port its
// tailscaled has.
func deleteMapping(ipt *iptables.IPTables, src netip.Addr, port int) error {
	listed, err := ipt.List("nat", portMapChain)
	if err != nil {
		return fmt.Errorf("failed to list chain %s: %w", portMapChain, err)
	}
	for _, rule := range listed {
		m := mappingRule.FindStringSubmatch(rule)
		if m == nil || m[1] != src.String() || m[2] != strconv.Itoa(port) {
			continue
		}
		spec := strings.Fields(strings.TrimPrefix(rule, "-A "+portMapChain+" "))
		if err := ipt.Delete("nat", portMapChain, spec...); err != nil {
			return fmt.Errorf("failed to remove the mapping of %s: %w", src, err)
		}
	}
	return nil
}

// removePortMapping removes the mapping of the tailscaled behind vethHost, and
// those of addresses no veth has any more, as when vethHost is gone already.
func removePortMapping(ipt *iptables.IPTables, vethHost string) error {
	portMapMu.Lock()
	defer portMapMu.Unlock()
	mapped, err := listMappings(ipt)
	if err != nil || len(mapped) == 0 {
		return err
	}
	var errs []error
	if src, err := containerAddr(vethHost); err == nil {
		if port, ok := mapped[src]; ok {
			errs = append(errs, deleteMapping(ipt, src, port))
		}
	}
	_, err = pruneMappings(ipt)
	return errors.Join(append(errs, err)...)
}
