package docker

import (
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/matchory/tslink/pkg/core"
)

// isolationCalls records the bridge names the driver installs the host's
// tailnet isolation with.
type isolationCalls struct {
	mu    sync.Mutex
	calls [][]string
}

func (c *isolationCalls) apply(on bool, bridges []string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if on {
		c.calls = append(c.calls, slices.Clone(bridges))
	}
	return nil
}

func (c *isolationCalls) last() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.calls) == 0 {
		return nil
	}
	return c.calls[len(c.calls)-1]
}

func (c *isolationCalls) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.calls)
}

// The host's tailnet isolation covers the bridges Docker's bridge networks
// name: a stack file names one with com.docker.network.bridge.name. Bridges
// Docker names itself (br-<ID>) need no name, and other drivers' options
// are not bridges.
// Guards: G1
func TestHostIsolationCoversDockerBridges(t *testing.T) {
	fake := newFakeDocker()
	fake.addNetwork("bridge", "bridge", map[string]string{bridgeNameOption: "docker0"}, nil)
	fake.addNetwork("custom", "bridge", map[string]string{bridgeNameOption: "custombr0"}, nil)
	fake.addNetwork("plain", "bridge", nil, nil)
	fake.addNetwork("over", "overlay", map[string]string{bridgeNameOption: "notabridge"}, nil)
	d := newDriver(&core.Config{DataDir: t.TempDir(), IsolateHostTailnet: true}, fake)
	var calls isolationCalls
	d.hostIsolation = calls.apply

	d.applyHostIsolation()
	if got, want := calls.last(), []string{"custombr0", "docker0"}; !slices.Equal(got, want) {
		t.Errorf("bridges = %q, want %q", got, want)
	}

	// Docker failing to list its networks narrows nothing: the last list holds
	fake.mu.Lock()
	fake.networkListErr = errors.New("daemon busy")
	fake.mu.Unlock()
	d.applyHostIsolation()
	if got, want := calls.last(), []string{"custombr0", "docker0"}; !slices.Equal(got, want) {
		t.Errorf("after a failed list: bridges = %q, want %q", got, want)
	}
}

// A new bridge network is covered at once, not only at the next watchdog
// run: containers may join it right away.
// Guards: G1
func TestHostIsolationFollowsNewBridges(t *testing.T) {
	fake := newFakeDocker()
	td := newTestDriver(t, fake)
	td.config.IsolateHostTailnet = true
	var calls isolationCalls
	td.hostIsolation = calls.apply
	td.watch(nil, nil)

	id := fake.addNetwork("later", "bridge", map[string]string{bridgeNameOption: "laterbr0"}, nil)
	fake.send(t, createEvent(id, "later", "bridge"))
	deadline := time.Now().Add(3 * time.Second)
	for !slices.Equal(calls.last(), []string{"laterbr0"}) {
		if time.Now().After(deadline) {
			t.Fatalf("bridges = %q, want laterbr0 after its network was created", calls.last())
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Other drivers' networks change nothing: the overlay's event comes
	// first, so had it updated the isolation, there would be two updates
	n := calls.count()
	fake.send(t, createEvent("ovid", "over", "overlay"))
	third := fake.addNetwork(
		"third",
		"bridge",
		map[string]string{bridgeNameOption: "thirdbr0"},
		nil,
	)
	fake.send(t, createEvent(third, "third", "bridge"))
	deadline = time.Now().Add(3 * time.Second)
	for !slices.Contains(calls.last(), "thirdbr0") {
		if time.Now().After(deadline) {
			t.Fatal("the second bridge network was not covered")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := calls.count(); got != n+1 {
		t.Errorf("%d isolation updates for one bridge and one overlay network, want 1", got-n)
	}
}
