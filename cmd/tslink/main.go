// Command tslink is a Docker network plugin that puts each container on a
// tailnet as its own Tailscale node.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/docker/go-plugins-helpers/network"
	dockerclient "github.com/moby/moby/client"

	"github.com/matchory/tslink/pkg/core"
	"github.com/matchory/tslink/pkg/diag"
	"github.com/matchory/tslink/pkg/docker"
	"github.com/matchory/tslink/pkg/logger"
	"github.com/matchory/tslink/pkg/preflight"
)

const (
	socketAddress = "/run/docker/plugins/tailscale.sock"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "diag", "diagnose", "status":
			initLogger()
			runDiag(os.Args[2:])
			return
		case "help", "-h", "--help":
			printHelp(os.Stdout)
			return
		case "version", "-v", "--version":
			printVersion(os.Stdout)
			return
		default:
			// Anything else would start a plugin instance, on the host if run there
			fmt.Fprintf(os.Stderr, "tslink: unknown command %q\n\n", os.Args[1])
			printHelp(os.Stderr)
			os.Exit(2)
		}
	}

	// Anywhere else, as on a host, a plugin instance would install isolation
	// chains the host's settings may turn off
	id, ok := pluginID()
	if !ok {
		fmt.Fprintf(os.Stderr, "tslink: not running as a Docker plugin (no plugin socket mount "+
			"at %s); install it with docker plugin install, see "+
			"docs/guides/install-and-upgrade.md\n\n", pluginSocketDir)
		printHelp(os.Stderr)
		os.Exit(2)
	}

	initLogger()
	runPlugin(id)
}

func initLogger() {
	if err := logger.Init(filepath.Join(core.DataDir(), "plugin.log")); err != nil {
		log.Printf("Warning: failed to initialize file logger: %v", err)
	}
}

func runPlugin(id string) {
	info, _ := debug.ReadBuildInfo()
	logger.Infof(
		"Starting Tailscale Docker network plugin, tslink %s, plugin %s",
		tslinkVersion(version, info),
		id,
	)

	driver, err := docker.NewDriver()
	if err != nil {
		logger.Errorf("Failed to create driver: %v", err)
		os.Exit(1)
	}

	// Set up signal handling for graceful shutdown
	// A second signal is ignored, as the shutdown is under way
	stopped, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)

	// Set before stop when the server failed: that is no signal, and the
	// process exits rather than shut down
	var serveFailed atomic.Bool
	go func() {
		<-stopped.Done()
		if serveFailed.Load() {
			return
		}
		logger.Infof("Shutting down gracefully: %v", context.Cause(stopped))

		ctx, cancel := context.WithTimeout(context.WithoutCancel(stopped), 30*time.Second)
		defer cancel()

		if err := driver.Shutdown(ctx); err != nil {
			logger.Errorf("Error during shutdown: %v", err)
		}

		logger.Infof("Shutdown complete, exiting")
		os.Exit(0)
	}()

	handler := network.NewHandler(driver)

	logger.Infof("Listening on %s", socketAddress)
	err = handler.ServeUnix(socketAddress, 0)
	serveFailed.Store(true)
	stop()
	if err != nil {
		logger.Errorf("Failed to serve: %v", err)
		os.Exit(1)
	}
}

func runDiag(args []string) {
	flags := flag.NewFlagSet("diag", flag.ExitOnError)
	check := flags.Bool("preflight", false, "check the environment properties of SECURITY.md")
	sharedDir := flags.String("shared-dir", "", "the shared certificate directory, as mounted here")
	if err := flags.Parse(args); err != nil {
		log.Fatalf("Diagnostic failed: %v", err)
	}
	if !*check {
		if err := diag.Run(core.DataDir(), os.Stdout); err != nil {
			log.Fatalf("Diagnostic failed: %v", err)
		}
		return
	}
	client, err := dockerclient.New(dockerclient.FromEnv)
	if err != nil {
		log.Fatalf("Preflight failed: %v", err)
	}
	env := preflight.Env{
		Docker: client,
		Run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, name, args...).CombinedOutput()
		},
		DataDir:   core.DataDir(),
		SharedDir: *sharedDir,
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	code := runPreflight(ctx, env, os.Stdout)
	cancel()
	if err := client.Close(); err != nil {
		log.Printf("Warning: failed to close the Docker client: %v", err)
	}
	os.Exit(code)
}

// runPreflight checks the environment properties, prints the results and
// returns the exit code: 1 if a property is violated, or a check failed to
// run; unknown does not fail it.
func runPreflight(ctx context.Context, env preflight.Env, w io.Writer) int {
	failed, err := preflight.Write(w, preflight.Check(ctx, env))
	if err != nil || failed {
		return 1
	}
	return 0
}

// version is tslink's version, set at build time with
// -ldflags "-X main.version=...". Empty, it is the module version the Go
// toolchain stamps, (devel) if it found no version control.
var version string

// printVersion prints tslink's version and that of the Tailscale it is built
// with, which is the bundled one (TestLocalAPIClientMatchesBundledTailscale).
func printVersion(w io.Writer) {
	info, _ := debug.ReadBuildInfo()
	fmt.Fprint(w, versionText(version, info))
}

// tslinkVersion returns stamped, else the module version of info.
func tslinkVersion(stamped string, info *debug.BuildInfo) string {
	switch {
	case stamped != "":
		return stamped
	case info != nil && info.Main.Version != "":
		return info.Main.Version
	}
	return "(devel)"
}

func versionText(stamped string, info *debug.BuildInfo) string {
	text := "tslink " + tslinkVersion(stamped, info) + "\n"
	if info == nil {
		return text
	}
	for _, dep := range info.Deps {
		if dep.Path == "tailscale.com" {
			return text + "tailscale " + dep.Version + "\n"
		}
	}
	return text
}

func printHelp(w io.Writer) {
	fmt.Fprint(w, `tslink - Tailscale Container Network Plugin

Usage:
  tslink          Serve as the Docker plugin; it refuses to run anywhere else
  tslink diag     Run diagnostics on all endpoints
  tslink diag --preflight [--shared-dir DIR]   Check the environment properties in SECURITY.md
  tslink help     Show this help message
  tslink version  Show the versions of tslink and its bundled Tailscale

Diagnostics:
  The 'diag' command shows the health status of all Tailscale endpoints.
  ghcr.io/matchory/tslink:latest is a Docker plugin tag, not an image
  docker run can pull; build the image from a checkout of the repository
  and run it from the host:

    docker build -t tslink-diag -f docker/Dockerfile .
    docker run --rm -v /var/lib/docker-plugins/tailscale:/data \
      --entrypoint /tslink tslink-diag diag

  Or use the helper script from a checkout of the repository:

    ./scripts/tslink-diag.sh

Preflight:
  'diag --preflight' checks the environment properties tslink's guarantees
  rely on (SECURITY.md), on the host it runs on: run it on every node of a
  Swarm. Build the image as for 'diag':

    docker build -t tslink-diag -f docker/Dockerfile .
    docker run --rm --network host --cap-add NET_ADMIN \
      -v /var/run/docker.sock:/var/run/docker.sock \
      -v /var/lib/docker-plugins/tailscale:/data \
      --entrypoint /tslink tslink-diag diag --preflight

  Add -v <shared dir>:/shared:ro --shared-dir /shared to check the owner
  and mode of the shared certificate directory (E7). It exits non-zero if a property is violated, or a
  check failed to run; unknown does not fail it.

`)
}
