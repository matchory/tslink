package main

import (
	"strings"
	"testing"
)

const testPluginID = "e74bc95c58656b8e0d1601564f3b152304ad54ec1062293382811ba347bad0cc"

// pluginMountinfo is a plugin's /proc/self/mountinfo, as Docker 29 on a
// btrfs data root set it up, with tslink's mounts of the host's /run, its
// network namespaces under it, and the data directory: Docker binds the
// plugin's directory in its exec root, /run/docker/plugins/<ID>, over the
// host's /run at /run/docker/plugins.
const pluginMountinfo = `607 394 0:38 /docker/plugins/` + testPluginID + `/rootfs / rw,noatime - btrfs /dev/vdb1 rw,ssd,subvolid=5,subvol=/
609 607 0:70 / /proc rw,nosuid,nodev,noexec,relatime - proc proc rw
610 607 0:71 / /dev rw,nosuid - tmpfs tmpfs rw,size=65536k,mode=755
612 607 0:55 / /sys ro,nosuid,nodev,noexec,relatime - sysfs sysfs rw
613 607 0:38 /var/lib/docker-plugins/tailscale /data rw,noatime - btrfs /dev/vdb1 rw,ssd,subvolid=5,subvol=/
614 607 0:58 / /run rw,nosuid,nodev,relatime master:134 - tmpfs none rw,mode=755
615 614 0:4 net:[4026532711] /run/docker/netns/1d2f0c3a9b8e rw shared:300 master:301 - nsfs nsfs rw
616 607 0:58 /docker/plugins/` + testPluginID + ` /run/docker/plugins rw,nosuid,nodev,relatime shared:254 master:134 - tmpfs none rw,mode=755
617 607 0:20 /opt/docker-rootfs/etc/hosts /etc/hosts ro,noatime master:26 - overlay orbstack rw,lowerdir=/
`

// hostMountinfo is the host's: Docker's exec root is in /run, with
// container network namespaces, and no mount at /run/docker/plugins.
const hostMountinfo = `22 1 259:1 / / rw,relatime shared:1 - ext4 /dev/nvme0n1p1 rw
23 22 0:21 / /proc rw,nosuid,nodev,noexec,relatime shared:12 - proc proc rw
25 22 0:24 / /run rw,nosuid,nodev,noexec,relatime shared:5 - tmpfs tmpfs rw,size=401060k,mode=755
310 25 0:4 net:[4026532711] /run/docker/netns/1d2f0c3a9b8e rw shared:300 - nsfs nsfs rw
311 22 0:47 / /var/lib/docker/overlay2/0f3c/merged rw,relatime - overlay overlay rw,lowerdir=/var/lib/docker/overlay2/l/A
`

func TestInDockerPlugin(t *testing.T) {
	line := func(root, mountPoint string) string {
		return "616 607 0:58 " + root + " " + mountPoint +
			" rw,nosuid,nodev,relatime shared:254 master:134 - tmpfs none rw,mode=755\n"
	}
	tests := []struct {
		name      string
		mountinfo string
		wantID    string
	}{
		{name: "plugin", mountinfo: pluginMountinfo, wantID: testPluginID},
		{name: "host", mountinfo: hostMountinfo},
		{name: "empty"},
		{
			// A container that bind-mounts the host's plugin sockets
			name:      "container with the host's socket directory",
			mountinfo: hostMountinfo + line("/docker/plugins", "/run/docker/plugins"),
		},
		{
			name:      "plugin directory of an exec root that is a mount itself",
			mountinfo: line("/plugins/"+testPluginID, "/run/docker/plugins"),
			wantID:    testPluginID,
		},
		{
			name:      "escaped space in the root",
			mountinfo: line(`/my\040docker/plugins/`+testPluginID, "/run/docker/plugins"),
			wantID:    testPluginID,
		},
		{
			name:      "escaped mount point",
			mountinfo: line("/docker/plugins/"+testPluginID, `/run/docker/plugins\040x`),
		},
		{
			name:      "mount point below the socket directory",
			mountinfo: line("/docker/plugins/"+testPluginID, "/run/docker/plugins/x"),
		},
		{
			name: "upper-case ID",
			mountinfo: line(
				"/docker/plugins/"+strings.ToUpper(testPluginID),
				"/run/docker/plugins",
			),
		},
		{
			name:      "short ID",
			mountinfo: line("/docker/plugins/"+testPluginID[:63], "/run/docker/plugins"),
		},
		{
			name:      "path below the plugin directory",
			mountinfo: line("/docker/plugins/"+testPluginID+"/x", "/run/docker/plugins"),
		},
		{
			name:      "ID not in a plugins directory",
			mountinfo: line("/docker/"+testPluginID, "/run/docker/plugins"),
		},
		{
			name:      "malformed lines before the plugin's",
			mountinfo: "garbage\n\n616 607 0:58 /docker/plugins\n" + pluginMountinfo,
			wantID:    testPluginID,
		},
		{
			name:      "truncated escape",
			mountinfo: line(`/docker/plugins/`+testPluginID+`\04`, "/run/docker/plugins"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ok, id := inDockerPlugin(strings.NewReader(tt.mountinfo))
			if ok != (tt.wantID != "") || id != tt.wantID {
				t.Errorf("inDockerPlugin = %v, %q; want %v, %q", ok, id, tt.wantID != "", tt.wantID)
			}
		})
	}
}

func TestUnescapeMountinfo(t *testing.T) {
	tests := map[string]string{
		`/plain`:            "/plain",
		`/a\040b`:           "/a b",
		`/tab\011nl\012`:    "/tab\tnl\n",
		`/back\134slash`:    `/back\slash`,
		`/not\08x`:          `/not\08x`,
		`/end\04`:           `/end\04`,
		`/over\777`:         `/over\777`,
		`\040\040`:          "  ",
		`/trailing\`:        `/trailing\`,
		`/mixed\040and\134`: `/mixed and\`,
	}
	for in, want := range tests {
		if got := unescapeMountinfo(in); got != want {
			t.Errorf("unescapeMountinfo(%q) = %q, want %q", in, got, want)
		}
	}
}
