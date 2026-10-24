package types

import (
	"strings"
	"time"
)

func (s Spec) parseList(raw string) (any, error) {
	var parts []string
	if raw == "" {
		if !s.AllowEmpty {
			return nil, ErrEmpty
		}
	} else {
		parts = strings.Split(raw, ",")
		for i, p := range parts {
			p = strings.Trim(p, " \t")
			if p == "" {
				return nil, ErrEmpty
			}
			parts[i] = p
		}
	}
	elem := s.elemSpec()
	vals := make([]any, 0, len(parts))
	for _, p := range parts {
		v, err := elem.parse(p)
		if err != nil {
			return nil, err
		}
		vals = append(vals, v)
	}
	switch s.Elem {
	case KindBool:
		return collect[bool](vals), nil
	case KindInt:
		return collect[int64](vals), nil
	case KindFloat:
		return collect[float64](vals), nil
	case KindDuration:
		return collect[time.Duration](vals), nil
	}
	return collect[string](vals), nil
}

func collect[T any](vals []any) []T {
	out := make([]T, len(vals))
	for i, v := range vals {
		out[i] = v.(T)
	}
	return out
}
