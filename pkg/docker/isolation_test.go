package docker

import (
	"errors"
	"os"
	"path/filepath"
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

// After a host reboot Docker answers no API call involving plugins until the
// plugin's socket is open, and disables a plugin that does not open it in
// time: start installs the host's tailnet isolation with the bridges Docker
// listed last, saved in the data directory, and does not wait for Docker.
// Guards: G1
func TestStartIsolatesBeforeDockerAnswers(t *testing.T) {
	fake := newFakeDocker()
	fake.hangList = true
	dir := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(dir, bridgesFile),
		[]byte("custombr0\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	d := newDriver(&core.Config{DataDir: dir, IsolateHostTailnet: true}, fake)
	var calls isolationCalls
	d.hostIsolation = calls.apply
	d.installedBridges = func() ([]string, error) { return nil, nil }
	t.Cleanup(func() {
		d.cancel()
		d.wg.Wait()
	})

	begin := time.Now()
	d.start()
	if took := time.Since(begin); took > time.Second {
		t.Errorf("start took %v, waiting for Docker", took)
	}
	if got, want := calls.last(), []string{"custombr0"}; !slices.Equal(got, want) {
		t.Errorf("bridges = %q, want the saved %q", got, want)
	}
}

// Once Docker answers, the isolation covers the bridges it lists, and the
// list is saved for the next start.
// Guards: G1
func TestStartUpdatesSavedBridges(t *testing.T) {
	fake := newFakeDocker()
	fake.networkListErr = errors.New("daemon starting")
	fake.addNetwork("custom", "bridge", map[string]string{bridgeNameOption: "newbr0"}, nil)
	dir := t.TempDir()
	d := newDriver(&core.Config{DataDir: dir, IsolateHostTailnet: true}, fake)
	var calls isolationCalls
	d.hostIsolation = calls.apply
	d.installedBridges = func() ([]string, error) { return nil, nil }
	t.Cleanup(func() {
		d.cancel()
		d.wg.Wait()
	})

	d.start()
	if got := calls.last(); len(got) != 0 {
		t.Errorf("bridges = %q before Docker answered, with none saved", got)
	}
	// A failed listing changes nothing, so it applies nothing again
	time.Sleep(1500 * time.Millisecond)
	if n := calls.count(); n != 1 {
		t.Errorf("%d isolation updates while Docker failed to list its networks, want 1", n)
	}
	fake.mu.Lock()
	fake.networkListErr = nil
	fake.mu.Unlock()

	deadline := time.Now().Add(5 * time.Second)
	for !slices.Equal(calls.last(), []string{"newbr0"}) {
		if time.Now().After(deadline) {
			t.Fatalf("bridges = %q, want newbr0 once Docker answered", calls.last())
		}
		time.Sleep(10 * time.Millisecond)
	}
	saved, err := os.ReadFile(filepath.Join(dir, bridgesFile))
	if err != nil || string(saved) != "newbr0\n" {
		t.Errorf("saved bridges = %q (%v), want newbr0", saved, err)
	}
}

// A start without the saved list, as the first after an upgrade, keeps the
// bridges the isolation already covers: the plugin's restart must not open
// the host's tailnet to containers on them until Docker answers.
// Guards: G1
func TestStartKeepsInstalledBridges(t *testing.T) {
	for _, saved := range []string{"", "custombr0\n"} {
		fake := newFakeDocker()
		fake.hangList = true
		dir := t.TempDir()
		if saved != "" {
			if err := os.WriteFile(
				filepath.Join(dir, bridgesFile),
				[]byte(saved),
				0o600,
			); err != nil {
				t.Fatal(err)
			}
		}
		d := newDriver(&core.Config{DataDir: dir, IsolateHostTailnet: true}, fake)
		var calls isolationCalls
		d.hostIsolation = calls.apply
		d.installedBridges = func() ([]string, error) { return []string{"oldbr0"}, nil }

		d.start()
		want := []string{"oldbr0"}
		if saved != "" {
			want = []string{"custombr0", "oldbr0"}
		}
		if got := calls.last(); !slices.Equal(got, want) {
			t.Errorf("saved %q: bridges = %q, want %q", saved, got, want)
		}
		d.cancel()
		d.wg.Wait()
	}
}

// A list that could not be saved is saved at the next listing, even if the
// bridges did not change meanwhile.
func TestSaveBridgesRetried(t *testing.T) {
	fake := newFakeDocker()
	fake.addNetwork("custom", "bridge", map[string]string{bridgeNameOption: "custombr0"}, nil)
	dir := t.TempDir()
	path := filepath.Join(dir, bridgesFile)
	// A directory in its place fails the save
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	d := newDriver(&core.Config{DataDir: dir, IsolateHostTailnet: true}, fake)
	d.hostIsolation = func(bool, []string) error { return nil }

	d.applyHostIsolation()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	d.applyHostIsolation()
	if saved, err := os.ReadFile(path); err != nil || string(saved) != "custombr0\n" {
		t.Errorf("saved bridges = %q (%v), want custombr0", saved, err)
	}
}
