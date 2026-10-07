package logger

import (
	"fmt"
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
}

// OpenRotating opens or creates the log file at path.
func OpenRotating(path string, maxBytes int64) (*RotatingFile, error) {
	r := &RotatingFile{path: path, maxBytes: maxBytes}
	if err := r.open(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *RotatingFile) open() error {
	f, err := os.OpenFile(r.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("failed to open %s: %w", r.path, err)
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return fmt.Errorf("failed to stat %s: %w", r.path, err)
	}
	r.file, r.size = f, st.Size()
	return nil
}

// Write appends p, rotating first if p would take the file past its limit.
func (r *RotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.size > 0 && r.size+int64(len(p)) > r.maxBytes {
		if err := r.file.Close(); err != nil {
			return 0, fmt.Errorf("failed to close %s: %w", r.path, err)
		}
		if err := os.Rename(r.path, r.path+".1"); err != nil {
			return 0, fmt.Errorf("failed to rotate %s: %w", r.path, err)
		}
		if err := r.open(); err != nil {
			return 0, err
		}
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
