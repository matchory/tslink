package tailscale

import (
	"errors"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/moby/moby/client/pkg/stringid"

	"github.com/matchory/tslink/pkg/logger"
)

// WireGuardPort is the UDP port every tslink tailscaled listens on, each in
// its container's namespace. The host lets only this port through between
// tslink's veths (netutil.SetupVethIsolation), so colocated nodes keep a
// direct path while containers cannot reach each other's other ports.
const WireGuardPort = 41641

// tailscaledArgs returns tailscaled's arguments. It uses a real tun device
// (tailscale0), so containers use Tailscale networking directly.
func (d *daemon) tailscaledArgs() []string {
	return []string{
		"--state=" + filepath.Join(d.config.StateDir, "tailscaled.state"),
		"--socket=" + d.socketPath,
		"--tun=tailscale0",
		"--statedir=" + d.config.StateDir,
		"--port=" + strconv.Itoa(WireGuardPort),
	}
}

// pretendpointEnv returns the environment that has tailscaled advertise eps
// as explicitly configured endpoints. tailscaled reads it once, at start.
func pretendpointEnv(eps []netip.AddrPort) []string {
	if len(eps) == 0 {
		return nil
	}
	s := make([]string, len(eps))
	for i, ep := range eps {
		s[i] = ep.String()
	}
	return []string{"TS_DEBUG_PRETENDPOINT=" + strings.Join(s, ",")}
}

// tailscaledCommand returns the command running tailscaled with args in the
// container's network namespace.
//
// tailscaled reads /etc/resolv.conf for its own lookups, of the ACME server for
// one, and as the upstreams of its DNS forwarder. The plugin's is the host's,
// whose resolver may not exist in that namespace, such as systemd-resolved's
// 127.0.0.53; and Docker's embedded resolver loops back to tailscaled when the
// container uses Tailscale's resolver. So tailscaled gets a resolv.conf of its
// own (see tailscaledResolvConf), in a mount namespace of its own. It gets its
// own backup of it too: tailscaled replaces /etc/resolv.conf and then reads its
// upstreams from the backup, which the endpoints would otherwise share. Without
// the mounts, tailscaled keeps the plugin's files.
func (d *daemon) tailscaledCommand(args []string) *exec.Cmd {
	host, err := os.ReadFile(hostResolvConfPath)
	if err != nil {
		logger.Warnf("Failed to read %s: %v", hostResolvConfPath, err)
	}
	resolved, err := os.ReadFile(resolvedResolvConfPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		logger.Warnf("Failed to read %s: %v", resolvedResolvConfPath, err)
	}
	conf, warning := tailscaledResolvConf(host, resolved, d.config.ContainerDNS)
	if warning != "" {
		logger.Warnf(
			"Endpoint %s: %s",
			stringid.TruncateID(d.config.EndpointID),
			warning,
		)
	}
	if d.config.Warn != nil {
		d.config.Warn(dnsUpstreamsWarning, warning)
	}
	resolvConf := filepath.Join(d.config.StateDir, "resolv.conf")
	if err := os.WriteFile(resolvConf, conf, 0o644); err != nil { // #nosec G306 -- not secret
		logger.Warnf("Failed to write %s: %v", resolvConf, err)
	}
	backup := filepath.Join(d.config.StateDir, "resolv.pre-tailscale-backup.conf")
	if err := os.WriteFile(backup, nil, 0o644); err != nil { // #nosec G306 -- not secret
		logger.Warnf("Failed to write %s: %v", backup, err)
	}
	// Taildrop writes received files to <state dir>/files on the host, where
	// no container sees them: drop any received before, and give tailscaled
	// an empty mount point to cover
	files := filepath.Join(d.config.StateDir, "files")
	if err := os.RemoveAll(files); err != nil {
		logger.Warnf("Failed to remove Taildrop files %s: %v", files, err)
	}
	if err := os.Mkdir(files, 0o700); err != nil {
		logger.Warnf("Failed to create %s: %v", files, err)
	}
	// unshare, sh and nsenter each exec the next, so the process is tailscaled.
	// The plugin's resolv.conf is a bind mount of the host's, and the kernel
	// refuses to mount over it once the host replaced that file (move_mount:
	// ENOENT), as systemd-resolved does when the host's DNS changes. The
	// namespace is tailscaled's own, so it drops that mount and tries again.
	// tailscaled must not reach the host's system bus, in the host's /run:
	// with it, it would configure the host's systemd-resolved with the
	// control server's DNS settings. It does not start if the bus stays.
	// Nor if it can store Taildrop files: a peer the control server lets
	// send files, or an untagged node's own container, could fill the host's
	// disk with them, and they serve no container.
	unshareArgs := []string{
		"--mount",
		"--propagation",
		"private",
		"--",
		"sh",
		"-c",
		`mount --bind "$0" /etc/resolv.conf 2>/dev/null ||
	{ umount /etc/resolv.conf && mount --bind "$0" /etc/resolv.conf; } ||
	echo "tslink: tailscaled keeps the plugin's resolv.conf" >&2
b=/etc/resolv.pre-tailscale-backup.conf
{ [ -e $b ] || touch $b; } && mount --bind "$1" $b || echo "tslink: tailscaled shares the plugin's resolv.conf backup" >&2
for d in /run/dbus /var/run/dbus; do
	[ ! -d $d ] || mount -t tmpfs -o size=4k,mode=0755 tmpfs $d ||
		{ echo "tslink: cannot hide the host's system bus from tailscaled" >&2; exit 1; }
done
unset DBUS_SYSTEM_BUS_ADDRESS
mount -t tmpfs -o ro,size=4k,mode=0500 tmpfs "$3" ||
	{ echo "tslink: cannot keep Taildrop files off the host" >&2; exit 1; }
netns=$2; shift 3; exec nsenter --net="$netns" -- "$@"`,
		resolvConf,
		backup,
		d.config.NetNSPath,
		files,
		d.config.TailscaledBin,
	}
	unshareArgs = append(unshareArgs, args...)
	logger.Debugf("Running: unshare %v", unshareArgs)
	cmd := exec.CommandContext(d.ctx, "unshare", unshareArgs...)
	// tailscaled does not need the plugin's auth key, and its peerapi serves
	// its environment to peers the control server lets debug it; and the
	// endpoints on the host it advertises (netutil.HostEndpoints), last:
	// exec.Cmd.Start dedups cmd.Env, keeping the last value for a duplicated
	// key, so they win over any operator setting
	pretendpoint := pretendpointEnv(d.hostEndpoints)
	if len(pretendpoint) > 0 && os.Getenv("TS_DEBUG_PRETENDPOINT") != "" {
		logger.Warnf(
			"Endpoint %s: TS_DEBUG_PRETENDPOINT is overridden by the host endpoints",
			stringid.TruncateID(d.config.EndpointID),
		)
	}
	cmd.Env = append(withoutAuthKey(os.Environ()), pretendpoint...)
	return cmd
}
