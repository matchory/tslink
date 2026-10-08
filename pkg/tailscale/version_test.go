package tailscale

import (
	"os"
	"regexp"
	"runtime/debug"
	"testing"
)

// The LocalAPI client is only known to work with a tailscaled of its own
// version: the plugin image's Tailscale and the tailscale.com module must
// match. Dependabot updates them in separate pull requests; this fails each
// until both agree.
func TestLocalAPIClientMatchesBundledTailscale(t *testing.T) {
	dockerfile, err := os.ReadFile("../../docker/Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^FROM tailscale/tailscale:(v[0-9][^@\s]*)`).
		FindSubmatch(dockerfile)
	if m == nil {
		t.Fatal("docker/Dockerfile has no tailscale/tailscale stage")
	}
	bundled := string(m[1])

	info, ok := debug.ReadBuildInfo()
	if !ok {
		t.Fatal("no build info")
	}
	for _, dep := range info.Deps {
		if dep.Path == "tailscale.com" {
			if dep.Version != bundled {
				t.Errorf(
					"tailscale.com is %s, the plugin image bundles Tailscale %s",
					dep.Version,
					bundled,
				)
			}
			return
		}
	}
	t.Error("tailscale.com is not a dependency")
}
