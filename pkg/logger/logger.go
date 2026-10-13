// Package logger provides the plugin's structured logging and rotating log
// files.
package logger

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync/atomic"
)

// maxLogBytes caps the plugin log; one rotated copy is kept.
const maxLogBytes = 50 << 20

// current is the logger in use: stdout until Init adds the log file.
var current atomic.Pointer[slog.Logger]

// level is the lowest level logged; info until SetLevel.
var level slog.LevelVar

func init() {
	SetOutput(os.Stdout)
}

// Init logs to the file at logPath as well as to stdout. If the file cannot
// be opened, logging stays on stdout and Init returns the error.
func Init(logPath string) error {
	file, err := OpenRotating(logPath, maxLogBytes)
	if err != nil {
		return err
	}
	// Stdout first: MultiWriter stops at a writer that fails, as the file
	// does on a full disk, and the Docker daemon's log keeps the lines
	SetOutput(io.MultiWriter(os.Stdout, file))
	return nil
}

// Get returns the logger in use.
func Get() *slog.Logger {
	return current.Load()
}

// SetOutput sends the log to w instead.
func SetOutput(w io.Writer) {
	current.Store(slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: &level})))
}

// SetLevel sets the lowest level logged.
func SetLevel(l slog.Level) {
	level.Set(l)
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
