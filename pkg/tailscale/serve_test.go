package tailscale

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestServeOptionArgs(t *testing.T) {
	if got := serveOptionArgs(ServeEndpoint{Proto: "tcp", ProxyProtocol: "2"}); !slices.Equal(got, []string{"--proxy-protocol=2"}) {
		t.Errorf("with proxy protocol: %q", got)
	}
	if got := serveOptionArgs(ServeEndpoint{Proto: "http", AcceptAppCaps: "example.com/cap/a,example.com/cap/b"}); !slices.Equal(got, []string{"--accept-app-caps=example.com/cap/a,example.com/cap/b"}) {
		t.Errorf("with app capabilities: %q", got)
	}
	if got := serveOptionArgs(ServeEndpoint{Proto: "tcp"}); len(got) != 0 {
		t.Errorf("without options: %q", got)
	}
}

// fakeCLI records the tailscale CLI calls of a daemon and answers them with
// out and err.
type fakeCLI struct {
	calls [][]string
	out   string
	err   error
}

func (f *fakeCLI) run(_ context.Context, _ string, args ...string) (string, error) {
	f.calls = append(f.calls, args)
	return f.out, f.err
}

const testSocket = "/run/test.sock"

func newTestDaemon(t *testing.T, cli *fakeCLI, service string) *Daemon {
	t.Helper()
	return &Daemon{
		config:     DaemonConfig{EndpointID: "test", StateDir: t.TempDir(), Service: service},
		socketPath: testSocket,
		runCLI:     cli.run,
	}
}

// serveCall returns the arguments after --socket of the "tailscale serve"
// command configuring ep for service, or for direct machine serve if service
// is empty; nil if no command ran.
func serveCall(t *testing.T, ep ServeEndpoint, service string) ([]string, error) {
	t.Helper()
	cli := &fakeCLI{}
	d := newTestDaemon(t, cli, service)
	var err error
	if service == "" {
		err = d.configureDirectServeEndpoint(ep)
	} else {
		err = d.configureServeEndpoint(ep)
	}
	switch len(cli.calls) {
	case 0:
		return nil, err
	case 1:
		args := cli.calls[0]
		if len(args) == 0 || args[0] != "--socket="+testSocket {
			t.Fatalf("command does not start with the socket: %q", args)
		}
		return args[1:], err
	default:
		t.Fatalf("ran %d commands, want at most one: %q", len(cli.calls), cli.calls)
		return nil, nil
	}
}

func TestServeArgs(t *testing.T) {
	const svc = "svc:web"
	tests := []struct {
		name    string
		service string
		ep      ServeEndpoint
		want    []string
		wantErr string
	}{
		// Direct machine serve
		{
			name: "direct http",
			ep:   ServeEndpoint{Proto: "http", Port: "80", Target: "8080"},
			want: []string{"serve", "--bg", "--http=80", "http://127.0.0.1:8080"},
		},
		{
			name: "direct https",
			ep:   ServeEndpoint{Proto: "https", Port: "443", Target: "8080"},
			want: []string{"serve", "--bg", "--https=443", "http://127.0.0.1:8080"},
		},
		{
			name: "direct https with path",
			ep:   ServeEndpoint{Proto: "https", Port: "443", Target: "8080", Path: "/api"},
			want: []string{"serve", "--bg", "--https=443", "--set-path=/api", "http://127.0.0.1:8080"},
		},
		{
			name: "direct https with path and options",
			ep: ServeEndpoint{Proto: "https", Port: "443", Target: "8080", Path: "/api",
				ProxyProtocol: "1", AcceptAppCaps: "example.com/cap/a,example.com/cap/b"},
			want: []string{"serve", "--bg", "--https=443", "--set-path=/api", "--proxy-protocol=1",
				"--accept-app-caps=example.com/cap/a,example.com/cap/b", "http://127.0.0.1:8080"},
		},
		{
			name: "direct tcp",
			ep:   ServeEndpoint{Proto: "tcp", Port: "5432", Target: "5432"},
			want: []string{"serve", "--bg", "--tcp=5432", "tcp://127.0.0.1:5432"},
		},
		{
			name: "direct tcp ignores path",
			ep:   ServeEndpoint{Proto: "tcp", Port: "5432", Target: "5432", Path: "/api"},
			want: []string{"serve", "--bg", "--tcp=5432", "tcp://127.0.0.1:5432"},
		},
		{
			name: "direct tcp with proxy protocol",
			ep:   ServeEndpoint{Proto: "tcp", Port: "5432", Target: "5432", ProxyProtocol: "2"},
			want: []string{"serve", "--bg", "--tcp=5432", "--proxy-protocol=2", "tcp://127.0.0.1:5432"},
		},
		{
			name: "direct tls-terminated-tcp",
			ep:   ServeEndpoint{Proto: "tls-terminated-tcp", Port: "443", Target: "8080"},
			want: []string{"serve", "--bg", "--tls-terminated-tcp=443", "tcp://127.0.0.1:8080"},
		},
		{
			name: "direct tls-terminated-tcp with proxy protocol",
			ep:   ServeEndpoint{Proto: "tls-terminated-tcp", Port: "443", Target: "8080", ProxyProtocol: "1"},
			want: []string{"serve", "--bg", "--tls-terminated-tcp=443", "--proxy-protocol=1", "tcp://127.0.0.1:8080"},
		},
		{
			name: "direct tun is skipped",
			ep:   ServeEndpoint{Proto: "tun", Port: "0", Target: "0"},
			want: nil,
		},

		// Tailscale Service backend
		{
			name:    "service http",
			service: svc,
			ep:      ServeEndpoint{Proto: "http", Port: "80", Target: "8080"},
			want:    []string{"serve", "--service=svc:web", "--http=80", "127.0.0.1:8080"},
		},
		{
			name:    "service https",
			service: svc,
			ep:      ServeEndpoint{Proto: "https", Port: "443", Target: "8080"},
			want:    []string{"serve", "--service=svc:web", "--https=443", "127.0.0.1:8080"},
		},
		{
			name:    "service https with path and options",
			service: svc,
			ep: ServeEndpoint{Proto: "https", Port: "443", Target: "8080", Path: "/api",
				ProxyProtocol: "2", AcceptAppCaps: "example.com/cap/a"},
			want: []string{"serve", "--service=svc:web", "--https=443", "--set-path=/api", "--proxy-protocol=2",
				"--accept-app-caps=example.com/cap/a", "127.0.0.1:8080"},
		},
		{
			name:    "service tcp",
			service: svc,
			ep:      ServeEndpoint{Proto: "tcp", Port: "5432", Target: "5432"},
			want:    []string{"serve", "--service=svc:web", "--tcp=5432", "tcp://127.0.0.1:5432"},
		},
		{
			name:    "service tcp with proxy protocol",
			service: svc,
			ep:      ServeEndpoint{Proto: "tcp", Port: "5432", Target: "5432", ProxyProtocol: "1"},
			want:    []string{"serve", "--service=svc:web", "--tcp=5432", "--proxy-protocol=1", "tcp://127.0.0.1:5432"},
		},
		{
			name:    "service tls-terminated-tcp",
			service: svc,
			ep:      ServeEndpoint{Proto: "tls-terminated-tcp", Port: "443", Target: "8080", ProxyProtocol: "2"},
			want: []string{"serve", "--service=svc:web", "--tls-terminated-tcp=443", "--proxy-protocol=2",
				"tcp://127.0.0.1:8080"},
		},
		{
			name:    "service tun ignores port, path and options",
			service: svc,
			ep: ServeEndpoint{Proto: "tun", Port: "1", Target: "2", Path: "/api",
				ProxyProtocol: "2", AcceptAppCaps: "example.com/cap/a"},
			want: []string{"serve", "--service=svc:web", "--tun"},
		},

		// Invalid input
		{
			name:    "direct non-numeric port",
			ep:      ServeEndpoint{Proto: "http", Port: "http", Target: "8080"},
			wantErr: `invalid external port "http": must be numeric`,
		},
		{
			name:    "direct missing target",
			ep:      ServeEndpoint{Proto: "tcp", Port: "5432"},
			wantErr: `invalid target port "": must be numeric`,
		},
		{
			name:    "direct tun with invalid port",
			ep:      ServeEndpoint{Proto: "tun", Port: "x", Target: "0"},
			wantErr: `invalid external port "x": must be numeric`,
		},
		{
			name:    "direct unsupported protocol",
			ep:      ServeEndpoint{Proto: "udp", Port: "53", Target: "53"},
			wantErr: "unsupported protocol: udp",
		},
		{
			name:    "service non-numeric target",
			service: svc,
			ep:      ServeEndpoint{Proto: "https", Port: "443", Target: "web:8080"},
			wantErr: `invalid target port "web:8080": must be numeric`,
		},
		{
			name:    "service tun with missing ports",
			service: svc,
			ep:      ServeEndpoint{Proto: "tun"},
			wantErr: `invalid external port "": must be numeric`,
		},
		{
			name:    "service unsupported protocol",
			service: svc,
			ep:      ServeEndpoint{Proto: "HTTP", Port: "80", Target: "8080"},
			wantErr: "unsupported protocol: HTTP",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The builder, and the command the daemon runs with it
			args, argsErr := serveArgs(tt.ep, tt.service)
			got, err := serveCall(t, tt.ep, tt.service)
			if !slices.Equal(args, got) || (argsErr == nil) != (err == nil) {
				t.Fatalf("serveArgs = %q, %v; daemon ran %q, %v", args, argsErr, got, err)
			}
			if tt.wantErr != "" {
				if err == nil || err.Error() != tt.wantErr {
					t.Fatalf("error = %v, want %q", err, tt.wantErr)
				}
				if got != nil {
					t.Fatalf("ran %q despite the error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("args =\n  %q\nwant\n  %q", got, tt.want)
			}
		})
	}
}

func TestConfigureServiceAdvertises(t *testing.T) {
	cli := &fakeCLI{}
	d := newTestDaemon(t, cli, "svc:web")
	d.config.Endpoints = []ServeEndpoint{
		{Proto: "https", Port: "443", Target: "8080"},
		{Proto: "tcp", Port: "5432", Target: "5432"},
	}
	if err := d.configureService(); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"--socket=" + testSocket, "serve", "--service=svc:web", "--https=443", "127.0.0.1:8080"},
		{"--socket=" + testSocket, "serve", "--service=svc:web", "--tcp=5432", "tcp://127.0.0.1:5432"},
		{"--socket=" + testSocket, "serve", "advertise", "svc:web"},
	}
	if !slices.EqualFunc(cli.calls, want, slices.Equal) {
		t.Errorf("calls =\n  %q\nwant\n  %q", cli.calls, want)
	}
}

func TestConfigureServiceSkips(t *testing.T) {
	cli := &fakeCLI{}
	d := newTestDaemon(t, cli, "")
	d.config.Endpoints = []ServeEndpoint{{Proto: "https", Port: "443", Target: "8080"}}
	if err := d.configureService(); err != nil {
		t.Fatal(err)
	}
	d = newTestDaemon(t, cli, "svc:web")
	if err := d.configureService(); err != nil {
		t.Fatal(err)
	}
	if len(cli.calls) != 0 {
		t.Errorf("ran %q without a service or endpoints", cli.calls)
	}
}

func TestConfigureServiceErrors(t *testing.T) {
	tests := []struct {
		name string
		out  string
		want string
	}{
		{"unknown service", "error: service not found", "service svc:web not found: create it in Tailscale admin console first"},
		{"untagged node", "service hosts must be tagged nodes", "requires tagged auth key"},
		{"other", "boom", "tailscale serve failed: exit status 1 (output: boom)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cli := &fakeCLI{out: tt.out, err: errors.New("exit status 1")}
			d := newTestDaemon(t, cli, "svc:web")
			d.config.Endpoints = []ServeEndpoint{{Proto: "https", Port: "443", Target: "8080"}}
			err := d.configureService()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tt.want)
			}
			if len(cli.calls) != 1 {
				t.Errorf("ran %d commands, want only the failing one", len(cli.calls))
			}
		})
	}
}

func TestConfigureServiceAdvertiseError(t *testing.T) {
	cli := &fakeCLI{}
	d := newTestDaemon(t, cli, "svc:web")
	d.config.Endpoints = []ServeEndpoint{{Proto: "tcp", Port: "22", Target: "22"}}
	d.runCLI = func(ctx context.Context, prefix string, args ...string) (string, error) {
		if slices.Contains(args, "advertise") {
			return " not allowed\n", errors.New("exit status 1")
		}
		return cli.run(ctx, prefix, args...)
	}
	err := d.configureService()
	if err == nil || err.Error() != "tailscale serve advertise failed: exit status 1 (output: not allowed)" {
		t.Errorf("error = %v", err)
	}
}

func TestDrain(t *testing.T) {
	cli := &fakeCLI{}
	d := newTestDaemon(t, cli, "")
	if err := d.Drain("svc:web"); err != nil {
		t.Fatal(err)
	}
	want := []string{"--socket=" + testSocket, "serve", "drain", "svc:web"}
	if len(cli.calls) != 1 || !slices.Equal(cli.calls[0], want) {
		t.Errorf("calls = %q, want %q", cli.calls, want)
	}

	cli.out, cli.err = "no such service\n", errors.New("exit status 1")
	err := d.Drain("svc:web")
	if err == nil || err.Error() != "tailscale serve drain failed: exit status 1 (output: no such service)" {
		t.Errorf("error = %v", err)
	}
}

func TestConfigureServeEndpoints(t *testing.T) {
	cli := &fakeCLI{}
	d := newTestDaemon(t, cli, "")
	endpoints := []ServeEndpoint{
		{Proto: "https", Port: "443", Target: "8080"},
		{Proto: "tun", Port: "0", Target: "0"},
	}
	if err := d.ConfigureServeEndpoints("svc:web", endpoints, []string{"tag:a", "tag:b"}, true); err != nil {
		t.Fatal(err)
	}
	s := "--socket=" + testSocket
	// Direct serve skips the tun endpoint
	want := [][]string{
		{s, "set", "--advertise-tags=tag:a,tag:b"},
		{s, "serve", "--bg", "--https=443", "http://127.0.0.1:8080"},
		{s, "serve", "--service=svc:web", "--https=443", "127.0.0.1:8080"},
		{s, "serve", "--service=svc:web", "--tun"},
		{s, "serve", "advertise", "svc:web"},
	}
	if !slices.EqualFunc(cli.calls, want, slices.Equal) {
		t.Errorf("calls =\n  %q\nwant\n  %q", cli.calls, want)
	}
}
