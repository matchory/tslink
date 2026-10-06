package docker

import (
	"testing"

	"github.com/aaomidi/tslink/pkg/core"
)

func TestParseContainerInfoStack(t *testing.T) {
	info := parseContainerInfo("billing_api.1.abc", map[string]string{core.StackLabel: "billing"})
	if info.Stack != "billing" {
		t.Errorf("Stack = %q, want billing", info.Stack)
	}
	if info := parseContainerInfo("web", nil); info.Stack != "" {
		t.Errorf("Stack = %q, want empty", info.Stack)
	}
}
