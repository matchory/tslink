package tailscale

import (
	"testing"

	"github.com/matchory/tslink/internal/leakcheck"
)

func TestMain(m *testing.M) {
	leakcheck.Main(m)
}
