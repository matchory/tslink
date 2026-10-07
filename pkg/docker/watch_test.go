package docker

import (
	"errors"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/docker/go-plugins-helpers/network"

	"github.com/matchory/tslink/pkg/core"
)

// eventSetup is a driver watching events, with a tslink network the driver
// created and a container on it.
type eventSetup struct {
	td                       *testDriver
	netID, epID, containerID string
	handled, stopped         chan string
}

func newEventSetup(t *testing.T, join bool) *eventSetup {
	t.Helper()
	fake := newFakeDocker()
	s := &eventSetup{
		epID:    fakeID("ep"),
		handled: make(chan string, 8),
		stopped: make(chan string, 8),
	}
	s.netID = fake.addNetwork("billing_net", pluginName, map[string]string{"tslink.authkey": "k"},
		map[string]string{core.StackLabel: "billing"})
	s.containerID = fake.addContainer("billing_web.1.xyz",
		map[string]string{core.StackLabel: "billing", "tslink.tags": "tag:web"},
		attachment{"billing_net", s.netID, s.epID})
	s.td = newTestDriver(t, fake)
	s.td.createNetwork(t, s.netID, map[string]string{"tslink.authkey": "k"})
	if join {
		s.td.join(t, s.netID, s.epID, sandbox)
	} else if _, err := s.td.CreateEndpoint(&network.CreateEndpointRequest{NetworkID: s.netID, EndpointID: s.epID}); err != nil {
		t.Fatal(err)
	}
	s.td.watch(s.handled, s.stopped)
	return s
}

// The connect event carries the container's name, which Join does not have:
// it starts Tailscale with the hostname derived from it.
func TestEventStartsTailscaleWithContainerName(t *testing.T) {
	s := newEventSetup(t, true)
	dns := netip.MustParseAddr("100.100.100.100")
	s.td.fake.setDNS(s.containerID, dns)
	s.td.noRun(t)

	s.td.fake.send(t, connectEvent(s.netID, "billing_net", s.containerID))
	if id := receive(t, s.handled, "the connect event"); id != s.epID {
		t.Fatalf("handled endpoint %s, want %s", id[:12], s.epID[:12])
	}
	r := s.td.nextRun(t)
	if r.endpointID != s.epID || r.info.Name != "billing_web.1.xyz" ||
		r.info.Hostname != "billing-web-1-xyz" ||
		r.info.Stack != "billing" ||
		r.info.NetworkStack != "billing" ||
		!slices.Equal(r.info.Tags, []string{"tag:web"}) ||
		!slices.Equal(r.info.DNS, []netip.Addr{dns}) {
		t.Errorf("started Tailscale with %+v", r.info)
	}
	if info, ok := s.td.cache.GetByEndpoint(s.epID); !ok || info != r.info {
		t.Error("container info not cached")
	}
}

// The event can arrive before Join: Join then starts Tailscale with its info.
func TestEventBeforeJoin(t *testing.T) {
	s := newEventSetup(t, false)
	s.td.fake.send(t, connectEvent(s.netID, "billing_net", s.containerID))
	receive(t, s.handled, "the connect event")
	s.td.noRun(t)

	if _, err := s.td.Join(
		&network.JoinRequest{NetworkID: s.netID, EndpointID: s.epID, SandboxKey: sandbox},
	); err != nil {
		t.Fatal(err)
	}
	if r := s.td.nextRun(t); r.endpointID != s.epID || r.info.Hostname != "billing-web-1-xyz" {
		t.Errorf("started %+v", r.info)
	}
}

func TestEventsIgnored(t *testing.T) {
	s := newEventSetup(t, true)
	fake := s.td.fake
	bridgeID := fake.addNetwork("bridge", "bridge", nil, nil)
	other := fake.addContainer("other", nil, attachment{"bridge", bridgeID, fakeID("bridge-ep")})

	fake.send(t, connectEvent(bridgeID, "bridge", other))              // another driver's network
	fake.send(t, connectEvent(s.netID, "billing_net", ""))             // no container
	fake.send(t, connectEvent(s.netID, "billing_net", fakeID("gone"))) // container already gone
	fake.send(
		t,
		connectEvent(s.netID, "other_net", s.containerID),
	) // container not on that network
	fake.send(t, connectEvent(s.netID, "billing_net", s.containerID))
	// Events are handled in order: the first one handled is the last one sent
	if id := receive(t, s.handled, "the connect event"); id != s.epID {
		t.Errorf("handled endpoint %s", id[:12])
	}
	if r := s.td.nextRun(t); r.endpointID != s.epID {
		t.Errorf("started endpoint %s", r.endpointID[:12])
	}
}

// A kill event with the container's stop signal reports that it is stopping,
// so its Service backends drain; other signals, such as SIGHUP, do not.
func TestKillEvents(t *testing.T) {
	s := newEventSetup(t, true)
	fake := s.td.fake
	quitter := fake.addContainer("quitter", nil, attachment{"billing_net", s.netID, fakeID("ep2")})
	fake.mu.Lock()
	fake.containers[len(fake.containers)-1].Config.StopSignal = "SIGQUIT"
	fake.mu.Unlock()
	marker := fakeID("marker")

	tests := []struct {
		container, signal string
		stops             bool
	}{
		{s.containerID, "1", false},  // SIGHUP: reload
		{s.containerID, "15", true},  // docker stop
		{s.containerID, "9", true},   // stop timeout
		{quitter, "15", false},       // not its stop signal
		{quitter, "3", true},         // SIGQUIT
		{fakeID("gone"), "15", true}, // container gone: default stop signal
	}
	var want []string
	for _, tt := range tests {
		fake.send(t, killEvent(tt.container, tt.signal))
		if tt.stops {
			want = append(want, tt.container)
		}
	}
	// When the marker is reported, every event before it was handled
	fake.send(t, killEvent(marker, "9"))

	var got []string
	for id := receive(t, s.stopped, "a stop"); id != marker; id = receive(t, s.stopped, "a stop") {
		got = append(got, id)
	}
	if !slices.Equal(got, want) {
		t.Errorf("stops reported for %d containers, want %d", len(got), len(want))
	}
}

// waitReconnect waits until the watcher opened its second event stream.
func waitReconnect(t *testing.T, fake *fakeDocker) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for fake.eventStreams() < 2 {
		if time.Now().After(deadline) {
			t.Fatal("watcher did not reconnect")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// After an error the watcher reconnects and handles events again. Without an
// event seen, it asks for those since it started watching.
func TestEventsReconnect(t *testing.T) {
	before := time.Now()
	s := newEventSetup(t, true)
	fake := s.td.fake
	fake.errs <- errors.New("connection reset")

	waitReconnect(t, fake)
	opts := fake.eventsOptions()
	if opts[0].Since != "" {
		t.Errorf("first stream since %q, want the events from now on", opts[0].Since)
	}
	if since := parseSince(t, opts[1].Since); since.Before(before) || since.After(time.Now()) {
		t.Errorf("reconnected since %v, want the time watching started", since)
	}
	fake.send(t, connectEvent(s.netID, "billing_net", s.containerID))
	receive(t, s.handled, "the connect event after reconnecting")
}

// A closed error channel ends the stream too: the watcher reconnects, instead
// of receiving nil errors from it in a busy loop.
func TestEventsReconnectAfterClosedStream(t *testing.T) {
	s := newEventSetup(t, true)
	fake := s.td.fake
	fake.mu.Lock()
	close(fake.errs)
	fake.mu.Unlock()

	waitReconnect(t, fake)
}

// Reconnecting, the watcher asks for the events after the last one it saw, so
// none sent while the stream was down are lost.
func TestEventsReconnectSinceLastEvent(t *testing.T) {
	s := newEventSetup(t, true)
	fake := s.td.fake
	last := time.Date(2026, 10, 7, 12, 0, 0, 999_999_999, time.UTC)
	msg := connectEvent(s.netID, "billing_net", s.containerID)
	msg.TimeNano = last.UnixNano()
	fake.send(t, msg)
	receive(t, s.handled, "the connect event")
	fake.errs <- errors.New("connection reset")

	waitReconnect(t, fake)
	if got, want := fake.eventsOptions()[1].Since, "1791374401.000000000"; got != want {
		t.Errorf("reconnected since %q, want %q, after the last event", got, want)
	}
}

// parseSince parses the Since option of an event stream.
func parseSince(t *testing.T, since string) time.Time {
	t.Helper()
	sec, nsec, ok := strings.Cut(since, ".")
	s, err1 := strconv.ParseInt(sec, 10, 64)
	ns, err2 := strconv.ParseInt(nsec, 10, 64)
	if !ok || len(nsec) != 9 || err1 != nil || err2 != nil {
		t.Fatalf("since %q is not seconds.nanoseconds", since)
	}
	return time.Unix(s, ns)
}
