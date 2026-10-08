// Command tslink is a Docker network plugin that puts each container on a
// tailnet as its own Tailscale node.
package main

import (
	"context"
	"flag"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
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
	if err := logger.Init(filepath.Join(core.DataDir(), "plugin.log")); err != nil {
		log.Printf("Warning: failed to initialize file logger: %v", err)
	}

	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "diag", "diagnose", "status":
			runDiag(os.Args[2:])
			return
		case "help", "-h", "--help":
			printHelp()
			return
		}
	}

	runPlugin()
}

func runPlugin() {
	logger.Infof("Starting Tailscale Docker network plugin")

	driver, err := docker.NewDriver()
	if err != nil {
		logger.Errorf("Failed to create driver: %v", err)
		os.Exit(1)
	}

	// Set up signal handling for graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	go func() {
		sig := <-sigCh
		logger.Infof("Received signal %v, initiating graceful shutdown", sig)

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		if err := driver.Shutdown(ctx); err != nil {
			logger.Errorf("Error during shutdown: %v", err)
		}

		logger.Infof("Shutdown complete, exiting")
		os.Exit(0)
	}()

	handler := network.NewHandler(driver)

	logger.Infof("Listening on %s", socketAddress)
	if err := handler.ServeUnix(socketAddress, 0); err != nil {
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
	docker, err := dockerclient.New(dockerclient.FromEnv)
	if err != nil {
		log.Fatalf("Preflight failed: %v", err)
	}
	env := preflight.Env{
		Docker: docker,
		Run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, name, args...).CombinedOutput()
		},
		DataDir:   core.DataDir(),
		SharedDir: *sharedDir,
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	code := runPreflight(ctx, env, os.Stdout)
	cancel()
	if err := docker.Close(); err != nil {
		log.Printf("Warning: failed to close the Docker client: %v", err)
	}
	os.Exit(code)
}

// runPreflight checks the environment properties, prints the results and
// returns the exit code: 1 if a property is violated or could not be checked.
func runPreflight(ctx context.Context, env preflight.Env, w io.Writer) int {
	failed, err := preflight.Write(w, preflight.Check(ctx, env))
	if err != nil || failed {
		return 1
	}
	return 0
}

func printHelp() {
	os.Stdout.WriteString(`tslink - Tailscale Container Network Plugin

Usage:
  tslink          Start the plugin server
  tslink diag     Run diagnostics on all endpoints
  tslink diag --preflight [--shared-dir DIR]   Check the environment properties in SECURITY.md
  tslink help     Show this help message

Diagnostics:
  The 'diag' command shows the health status of all Tailscale endpoints.
  Run it from the host using:

    docker run --rm -v /var/lib/docker-plugins/tailscale:/data \
      --entrypoint /tslink \
      ghcr.io/matchory/tslink:latest diag

  Or use the helper script from a checkout of the repository:

    ./scripts/tslink-diag.sh

Preflight:
  'diag --preflight' checks the environment properties tslink's guarantees
  rely on (SECURITY.md). It needs the Docker API and the host's network:

    docker run --rm --network host --cap-add NET_ADMIN \
      -v /var/run/docker.sock:/var/run/docker.sock \
      -v /var/lib/docker-plugins/tailscale:/data \
      --entrypoint /tslink ghcr.io/matchory/tslink:latest diag --preflight

  Add -v <shared dir>:/shared:ro --shared-dir /shared to check the shared
  certificate directory. It exits non-zero if a property is violated or
  could not be checked.

`)
}
