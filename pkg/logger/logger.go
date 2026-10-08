// Package logger provides the plugin's structured logging and rotating log
// files.
package logger

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
)

// maxLogBytes caps the plugin log; one rotated copy is kept.
const maxLogBytes = 50 << 20

var (
	current atomic.Pointer[slog.Logger]
	once    sync.Once
)

// Init initializes the global logger with file and stdout output.
// Call this once at startup. If not called, Get() will initialize with defaults.
func Init(logPath string) error {
	var initErr error
	once.Do(func() {
		initErr = initLogger(logPath)
	})
	return initErr
}

func initLogger(logPath string) error {
	file, err := OpenRotating(logPath, maxLogBytes)
	if err != nil {
		// Fall back to stdout only
		current.Store(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
			Level: slog.LevelDebug,
		})))
		return err
	}

	// Create multi-writer for both file and stdout
	multiWriter := io.MultiWriter(file, os.Stdout)

	current.Store(slog.New(slog.NewTextHandler(multiWriter, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})))

	return nil
}

// Get returns the global logger, initializing with defaults if needed.
func Get() *slog.Logger {
	once.Do(func() {
		// Default initialization to /data/plugin.log
		_ = initLogger("/data/plugin.log")
	})
	return current.Load()
}

// SetOutput sends the log to w instead, for tests.
func SetOutput(w io.Writer) {
	once.Do(func() {})
	current.Store(slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug})))
}

// Debugf logs at debug level with printf-style formatting.
func Debugf(format string, args ...any) {
	Get().Debug(fmt.Sprintf(format, args...))
}

// Infof logs at info level with printf-style formatting.
func Infof(format string, args ...any) {
	Get().Info(fmt.Sprintf(format, args...))
}

// Warnf logs at warn level with printf-style formatting.
func Warnf(format string, args ...any) {
	Get().Warn(fmt.Sprintf(format, args...))
}

// Errorf logs at error level with printf-style formatting.
func Errorf(format string, args ...any) {
	Get().Error(fmt.Sprintf(format, args...))
}

// With returns a logger with additional attributes.
func With(args ...any) *slog.Logger {
	return Get().With(args...)
}
