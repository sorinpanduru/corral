package loader

import (
	"errors"

	"github.com/sorinpanduru/corral/internal/infra/config/registry"
	"github.com/sorinpanduru/corral/internal/infra/config/source"
)

// readFileRule reads the winning path of a File setting through the injected
// reader and runs the rule's Check. Errors are constant sentinels.
func readFileRule(s registry.Setting, path any, read source.ReadFile) (registry.FileValue, error) {
	p, _ := path.(string)
	limit := s.File.MaxBytes
	if limit <= 0 {
		limit = registry.DefaultFileMaxBytes
	}
	b, err := read(p, limit)
	switch {
	case err == nil:
	case errors.Is(err, source.ErrNotRegular):
		return registry.FileValue{}, ErrFileNotRegular
	case errors.Is(err, source.ErrTooLarge):
		return registry.FileValue{}, ErrFileTooLarge
	default:
		return registry.FileValue{}, ErrFileUnreadable
	}
	content := append([]byte(nil), b...)
	if s.File.Check != nil && s.File.Check(content) != nil {
		return registry.FileValue{}, ErrInvalidValue
	}
	return registry.FileValue{Path: p, Content: content}, nil
}
