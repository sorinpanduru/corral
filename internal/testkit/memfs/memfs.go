// Package memfs is an in-memory source.ReadFile for loader tests, with a
// per-path read counter. Synthetic content only.
package memfs

import (
	"sync"

	"github.com/sorinpanduru/corral/internal/infra/config/source"
)

type entry struct {
	content []byte
	err     error
}

// FS is a concurrency-safe in-memory file set.
type FS struct {
	mu      sync.Mutex
	files   map[string]entry
	callCnt map[string]int
}

// New returns an empty FS.
func New() *FS {
	return &FS{files: map[string]entry{}, callCnt: map[string]int{}}
}

// Put stores a copy of content at path, replacing any earlier Put or Fail.
func (f *FS) Put(path string, content []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[path] = entry{content: append([]byte{}, content...)}
}

// Fail makes reads of path return err (a source sentinel).
func (f *FS) Fail(path string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[path] = entry{err: err}
}

// Read is a source.ReadFile. Unknown paths are source.ErrUnreadable; content
// longer than maxBytes is source.ErrTooLarge. Every call is counted.
func (f *FS) Read(path string, maxBytes int64) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.callCnt[path]++
	e, ok := f.files[path]
	switch {
	case !ok:
		return nil, source.ErrUnreadable
	case e.err != nil:
		return nil, e.err
	case int64(len(e.content)) > maxBytes:
		return nil, source.ErrTooLarge
	}
	return append([]byte{}, e.content...), nil
}

// Calls returns how many times path was read.
func (f *FS) Calls(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.callCnt[path]
}
