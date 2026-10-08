package docker

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/docker/go-plugins-helpers/network"

	"github.com/matchory/tslink/pkg/core"
	"github.com/matchory/tslink/pkg/logger"
)

// lockedBuffer is a bytes.Buffer that goroutines may write concurrently.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestCredentialsStayOutOfTheLog creates networks with valid and invalid
// options around an auth key, and checks that neither the plugin's log nor the
// errors returned to Docker contain it.
//
// Guards: G5
func TestCredentialsStayOutOfTheLog(t *testing.T) {
	const canary = "tskey-auth-kCANARY0CNTRL-0123456789abcdef"
	var log lockedBuffer
	logger.SetOutput(&log)
	t.Cleanup(func() { logger.SetOutput(os.Stdout) })

	td := newTestDriver(t, newFakeDocker())
	td.config.AuthKey = canary + "plugin"
	for i, opts := range []map[string]any{
		{"tslink.authkey": canary},
		{"tslink.authkey": canary, "tslink.tags": "tag:a"},
		{"tslink.authkey": canary, core.LoginServerOption: "ftp://x"},
		{"tslink.authkey": canary, core.EphemeralOption: "maybe"},
		{"tslink.authkey": canary + "?ephemeral=maybe"},
		{"tslink.authkey": canary + "?preauthorized=true"},
		{},
	} {
		err := td.CreateNetwork(&network.CreateNetworkRequest{
			NetworkID: fakeID(fmt.Sprint("credentials", i)),
			Options:   opts,
		})
		if err != nil && strings.Contains(err.Error(), "CANARY") {
			t.Errorf("CreateNetwork(%d) error contains the auth key: %v", i, err)
		}
	}
	if strings.Contains(log.String(), "CANARY") {
		t.Errorf("the plugin log contains the auth key:\n%s", log.String())
	}
}
