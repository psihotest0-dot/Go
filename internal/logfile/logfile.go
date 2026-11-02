// Package logfile holds the small log writers the backend uses next to the
// rotating backend.log.
package logfile

import (
	"io"
	"os"
	"sync"
)

// Truncating is a log file that starts over when it reaches its size limit.
// Unlike a rotating file it keeps no backup: it is meant for lines that are only
// noise once they are a few minutes old (the Prometheus scrapes), so the old
// content is simply deleted and the file never takes more than the limit.
//
// The file is opened on the first write and created if it is missing. A file
// that is already larger than the limit is emptied on the first write. It is
// safe for concurrent use.
type Truncating struct {
	path string
	max  int64

	mu   sync.Mutex
	file *os.File
	size int64
}

// NewTruncating returns a writer for path that empties the file when a write
// would take it over maxBytes. A limit below 1 is raised to 1.
func NewTruncating(path string, maxBytes int64) *Truncating {
	if maxBytes < 1 {
		maxBytes = 1
	}
	return &Truncating{path: path, max: maxBytes}
}

// Write appends p to the file, emptying it first when p would not fit. A single
// write larger than the limit is still written whole: a log line is never cut.
func (t *Truncating) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.file == nil {
		// No O_APPEND: on Windows it opens the file without the right to truncate
		// it. The size is tracked here instead.
		f, err := os.OpenFile(t.path, os.O_WRONLY|os.O_CREATE, 0o644)
		if err != nil {
			return 0, err
		}
		size, err := f.Seek(0, io.SeekEnd)
		if err != nil {
			f.Close()
			return 0, err
		}
		t.file, t.size = f, size
	}

	if t.size > 0 && t.size+int64(len(p)) > t.max {
		if err := t.file.Truncate(0); err != nil {
			return 0, err
		}
		if _, err := t.file.Seek(0, io.SeekStart); err != nil {
			return 0, err
		}
		t.size = 0
	}

	n, err := t.file.Write(p)
	t.size += int64(n)
	return n, err
}

// Close closes the file. A later Write opens it again.
func (t *Truncating) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.file == nil {
		return nil
	}
	err := t.file.Close()
	t.file = nil
	return err
}
