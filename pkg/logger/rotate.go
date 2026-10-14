package logger

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sync"
)

// RotatingFile is an append-only log file that is renamed to <path>.1 once it
// would grow past maxBytes, so a log never takes more than twice that on disk.
type RotatingFile struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	file     *os.File
	size     int64
	failing  bool // A rotation failed, and was reported
}

// OpenRotating opens or creates the log file at path.
func OpenRotating(path string, maxBytes int64) (*RotatingFile, error) {
	r := &RotatingFile{path: path, maxBytes: maxBytes}
	if err := r.open(); err != nil {
		return nil, err
	}
	return r, nil
}

// Write appends p, rotating first if p would take the file past its limit.
func (r *RotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.size > 0 && r.size+int64(len(p)) > r.maxBytes {
		err := r.rotate()
		if err != nil && !r.failing {
			// The log cannot log its own failure. Lines go on to the
			// current file, and the next write tries again
			fmt.Fprintf(os.Stderr, "failed to rotate %s: %v\n", r.path, err)
		}
		r.failing = err != nil
	}
	n, err := r.file.Write(p)
	r.size += int64(n)
	return n, err
}

// Close closes the file.
func (r *RotatingFile) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.file.Close()
}

// rotate renames the file to <path>.1 and opens a new one. The old file is
// closed only once the new one is open, so there is always one to write to.
// If the file is gone, as when a rotation renamed it but could not open the
// new one, it only opens the new one.
func (r *RotatingFile) rotate() error {
	if err := os.Rename(r.path, r.path+".1"); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	old := r.file
	if err := r.open(); err != nil {
		return err
	}
	if err := old.Close(); err != nil {
		return fmt.Errorf("failed to close the rotated %s: %w", r.path, err)
	}
	return nil
}

func (r *RotatingFile) open() error {
	f, err := os.OpenFile(r.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("failed to open %s: %w", r.path, err)
	}
	st, err := f.Stat()
	if err != nil {
		if closeErr := f.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
		return fmt.Errorf("failed to stat %s: %w", r.path, err)
	}
	r.file, r.size = f, st.Size()
	return nil
}
