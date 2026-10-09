package tailscale

import (
	"slices"
	"testing"
)

// The status comes from the LocalAPI without peers: a node in a large tailnet
// would otherwise list them all on every poll.
func TestGetStatus(t *testing.T) {
	api := newFakeLocalAPI(t)
	api.set(statusPath,
		`{"Self":{"TailscaleIPs":["100.64.0.1","fd7a::1"],"HostName":"web","Online":true}}`)
	d := newTestDaemon(t, &fakeCLI{}, "")
	d.lc = api.client()
	st, err := d.getStatus(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if *st != (Status{IP: "100.64.0.1", Hostname: "web", Online: true}) {
		t.Errorf("status = %+v", *st)
	}
	if want := []string{"GET " + statusPath + "?peers=false"}; !slices.Equal(api.paths(), want) {
		t.Errorf("requests = %q, want %q", api.paths(), want)
	}

	api.set(statusPath, "")
	if _, err := d.getStatus(t.Context()); err == nil {
		t.Error("no error from a failing tailscaled")
	}
}

func TestWaitBackendState(t *testing.T) {
	api := newFakeLocalAPI(t)
	api.set(statusPath, `{"BackendState":"NeedsLogin"}`)
	d := newTestDaemon(t, &fakeCLI{}, "")
	d.lc = api.client()
	if st := d.waitBackendState(); st != "NeedsLogin" {
		t.Errorf("backend state = %q, want NeedsLogin", st)
	}
	if want := []string{"GET " + statusPath + "?peers=false"}; !slices.Equal(api.paths(), want) {
		t.Errorf("requests = %q, want %q", api.paths(), want)
	}
}

func TestBackendState(t *testing.T) {
	api := newFakeLocalAPI(t)
	api.set(statusPath, `{"BackendState":"Running"}`)
	d := newTestDaemon(t, &fakeCLI{}, "")
	d.lc = api.client()
	if st, err := d.BackendState(); err != nil || st != "Running" {
		t.Errorf("BackendState() = %q, %v, want Running", st, err)
	}
	api.set(statusPath, "")
	if _, err := d.BackendState(); err == nil {
		t.Error("no error from a failing tailscaled")
	}
}
