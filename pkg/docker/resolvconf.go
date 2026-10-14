package docker

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"slices"
	"strings"

	dockerclient "github.com/moby/moby/client"
	"github.com/moby/moby/client/pkg/stringid"

	"github.com/matchory/tslink/pkg/core"
	"github.com/matchory/tslink/pkg/logger"
)

// resolvConfMaxBytes bounds how much of a container's resolv.conf is read.
const resolvConfMaxBytes = 64 << 10

// configuredDNS returns the DNS servers Docker configured the container with
// beyond its own: those daemon.json's "dns" gives a container without --dns.
// Docker does not show them in the container's config, only in the comments
// it writes to the container's resolv.conf (libnetwork's resolvconf package):
//
//	# ExtServers: [10.0.0.2 1.1.1.1]
//	# Overrides: [nameservers]
//
// ExtServers are the servers Docker's embedded resolver forwards to; they
// override the host's only if Overrides lists nameservers. Servers from the
// host's file, written as host(<addr>), are left to tslink's reading of the
// host's resolv.conf. It returns nil if the file holds no such servers, as
// one the container rewrote.
func configuredDNS(resolvConf []byte) []netip.Addr {
	var servers []netip.Addr
	override := false
	sc := bufio.NewScanner(bytes.NewReader(resolvConf))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if list, ok := commentList(line, "# ExtServers:"); ok {
			servers = servers[:0]
			for _, s := range list {
				if a, err := netip.ParseAddr(s); err == nil {
					servers = append(servers, a)
				}
			}
		}
		if list, ok := commentList(line, "# Overrides:"); ok {
			override = slices.Contains(list, "nameservers")
		}
	}
	if !override || len(servers) == 0 {
		return nil
	}
	return servers
}

// commentList returns the bracketed, space-separated list of a comment line
// with prefix, as "# ExtServers: [a b]".
func commentList(line, prefix string) ([]string, bool) {
	rest, ok := strings.CutPrefix(line, prefix)
	if !ok {
		return nil, false
	}
	rest = strings.TrimSpace(rest)
	rest, ok = strings.CutPrefix(rest, "[")
	if !ok {
		return nil, false
	}
	rest, ok = strings.CutSuffix(rest, "]")
	if !ok {
		return nil, false
	}
	return strings.Fields(rest), true
}

// readContainerFile reads a regular file from a container through Docker's
// archive API, up to max bytes.
func readContainerFile(
	ctx context.Context,
	cli dockerAPI,
	containerID, path string,
	maxBytes int64,
) ([]byte, error) {
	res, err := cli.CopyFromContainer(ctx, containerID, dockerclient.CopyFromContainerOptions{
		SourcePath: path,
	})
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := res.Content.Close(); err != nil {
			logger.Debugf("Closing the archive of %s: %v", path, err)
		}
	}()
	tr := tar.NewReader(res.Content)
	hdr, err := tr.Next()
	if err != nil {
		return nil, fmt.Errorf("reading the archive of %s: %w", path, err)
	}
	if hdr.Typeflag != tar.TypeReg {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	b, err := io.ReadAll(io.LimitReader(tr, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	if int64(len(b)) > maxBytes {
		return nil, errors.New(path + " is too large")
	}
	return b, nil
}

// addDaemonDNS sets the container's DNS servers from daemon.json's "dns",
// read from its resolv.conf, if it was given none of its own. Failing to
// read them leaves it without: tailscaled then uses the host's resolvers.
func addDaemonDNS(ctx context.Context, cli dockerAPI, info *core.ContainerInfo) {
	if len(info.DNS) > 0 || info.ID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, dockerCallTimeout)
	defer cancel()
	b, err := readContainerFile(ctx, cli, info.ID, "/etc/resolv.conf", resolvConfMaxBytes)
	if err != nil {
		logger.Debugf("Reading the resolv.conf of container %s: %v",
			stringid.TruncateID(info.ID), err)
		return
	}
	info.DNS = configuredDNS(b)
}
