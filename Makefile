# The development plugin is created under the name tslink, the alias the
# documentation installs releases under, so networks use "--driver tslink:latest".
PLUGIN_NAME ?= tslink
PLUGIN_TAG ?= latest
GOARCH ?= $(shell go env GOARCH)
# The version the binary reports. Empty outside a repository, which leaves the
# Go toolchain's stamp
VERSION ?= $(shell scripts/version.sh 2>/dev/null)
LDFLAGS = -X main.version=$(VERSION)

.PHONY: all build clean create enable disable test reinstall

# The default target only builds; reinstall replaces the local plugin.
all: build

build:
	@echo "Building plugin binary for $(GOARCH)..."
	CGO_ENABLED=0 GOOS=linux GOARCH=$(GOARCH) go build -ldflags "$(LDFLAGS)" -o docker/rootfs/tslink ./cmd/tslink

# The rootfs is assembled in a container, never on the host: on a filesystem
# that ignores case, as macOS's default volume, libxt_MARK.so and libxt_mark.so
# overwrite each other, and the plugin's iptables cannot read its own rules.
DOCKER_CLI ?= docker:29.3.1-cli@sha256:18f5ab0fab739ea822819b342357947dfba235cdef438cce345ebc0c143c5b34

create:
	@echo "Building Docker image for rootfs..."
	docker build --build-arg TSLINK_VERSION=$(VERSION) -t $(PLUGIN_NAME):rootfs -f docker/Dockerfile .
	@echo "Creating plugin..."
	docker plugin rm -f $(PLUGIN_NAME):$(PLUGIN_TAG) 2>/dev/null || true
	docker run --rm -v /var/run/docker.sock:/var/run/docker.sock \
		-v "$(CURDIR)/docker/config.json:/plugin/config.json:ro" $(DOCKER_CLI) sh -euc '\
		mkdir /plugin/rootfs; \
		c=$$(docker create $(PLUGIN_NAME):rootfs); \
		docker export "$$c" | tar -x -C /plugin/rootfs; \
		docker rm "$$c" >/dev/null; \
		docker plugin create $(PLUGIN_NAME):$(PLUGIN_TAG) /plugin'

enable:
	@echo "Enabling plugin..."
	docker plugin enable $(PLUGIN_NAME):$(PLUGIN_TAG)

disable:
	@echo "Disabling plugin..."
	docker plugin disable $(PLUGIN_NAME):$(PLUGIN_TAG)

clean:
	@echo "Cleaning up..."
	rm -rf docker/rootfs
	docker plugin rm -f $(PLUGIN_NAME):$(PLUGIN_TAG) 2>/dev/null || true

test:
	@echo "Running tests..."
	go test -v ./...

# Development helpers
dev-build:
	go build -ldflags "$(LDFLAGS)" -o tslink ./cmd/tslink

# Full reinstall cycle for development
reinstall: clean create enable
	@echo "Plugin reinstalled successfully"

# View plugin logs (requires jq)
logs:
	@docker plugin inspect $(PLUGIN_NAME):$(PLUGIN_TAG) --format '{{.ID}}' | xargs -I {} docker logs {}

# Load .env file if it exists
-include .env
export

# Create a test network (requires TS_AUTHKEY env var or .env file)
test-network:
ifndef TS_AUTHKEY
	$(error TS_AUTHKEY is not set. Create .env file with TS_AUTHKEY=xxx or export TS_AUTHKEY=xxx)
endif
	docker network rm tsnet-test 2>/dev/null || true
	docker network create --driver $(PLUGIN_NAME):$(PLUGIN_TAG) --opt tslink.authkey=$(TS_AUTHKEY) tsnet-test

# Remove test network
test-network-rm:
	docker network rm tsnet-test 2>/dev/null || true

# Run a test container
test-container:
	docker run --rm --network tsnet-test alpine ip addr

# Full test cycle: reinstall plugin, create network, run container
test-full: reinstall test-network test-container test-network-rm
	@echo "Full test cycle complete"
