// Package osfile is the real source.ReadFile (SPEC-003 FR-15, FR-25). It is
// one of the two places allowed to touch the filesystem for configuration
// (AC-15).
//
// Compliance (SPEC-003 FR-11): no OS error is returned or wrapped, because an
// *fs.PathError carries the path. Errors are the constant source sentinels.
package osfile

import (
	"io"
	"math"
	"os"

	"github.com/sorinpanduru/corral/internal/infra/config/source"
)

// Read returns the content of the regular file at path (symlinks followed),
// at most maxBytes bytes. It never blocks on a FIFO or device: the path must
// stat as a regular file, the file is opened non-blocking, and the handle is
// checked again. At most maxBytes+1 bytes are read.
func Read(path string, maxBytes int64) ([]byte, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, source.ErrUnreadable
	}
	if !fi.Mode().IsRegular() {
		return nil, source.ErrNotRegular
	}
	f, err := os.OpenFile(path, os.O_RDONLY|openFlags, 0)
	if err != nil {
		return nil, source.ErrUnreadable
	}
	defer f.Close()
	hi, err := f.Stat()
	if err != nil {
		return nil, source.ErrUnreadable
	}
	if !hi.Mode().IsRegular() {
		return nil, source.ErrNotRegular
	}
	if maxBytes < 0 {
		maxBytes = 0
	}
	limit := maxBytes
	if limit < math.MaxInt64 {
		limit++
	}
	data, err := io.ReadAll(io.LimitReader(f, limit))
	if err != nil {
		return nil, source.ErrUnreadable
	}
	if int64(len(data)) > maxBytes {
		return nil, source.ErrTooLarge
	}
	return data, nil
}
