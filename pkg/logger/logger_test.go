package logger

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestSetOutput(t *testing.T) {
	var buf bytes.Buffer
	SetOutput(&buf)
	t.Cleanup(func() { SetOutput(os.Stdout) })
	Infof("hello %d", 1)
	if !strings.Contains(buf.String(), "hello 1") {
		t.Errorf("log %q does not contain the message", buf.String())
	}
}
