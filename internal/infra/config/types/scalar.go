package types

import (
	"errors"
	"math"
	"strconv"
	"time"
)

func isControl(r rune) bool { return r <= 0x1f || r >= 0x7f && r <= 0x9f }

func parseString(raw string, allowEmpty, text bool) (any, error) {
	if raw == "" {
		if allowEmpty {
			return "", nil
		}
		return nil, ErrEmpty
	}
	for _, r := range raw {
		if !isControl(r) {
			continue
		}
		if text && (r == '\t' || r == '\n' || r == '\r') {
			continue
		}
		return nil, ErrControlChar
	}
	return raw, nil
}

func parseBool(raw string) (any, error) {
	switch {
	case asciiEqualFold(raw, "true"):
		return true, nil
	case asciiEqualFold(raw, "false"):
		return false, nil
	}
	return nil, ErrSyntax
}

func parseInt(raw string) (any, error) {
	digits := raw
	if len(digits) > 0 && digits[0] == '-' {
		digits = digits[1:]
	}
	if !isDigits(digits) {
		return nil, ErrSyntax
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return nil, ErrRange
	}
	return v, nil
}

// scanDecimal returns the length of the FR-12 decimal prefix of s
// ([0-9]+(\.[0-9]*)? or \.[0-9]+), or 0.
func scanDecimal(s string) int {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	intDigits := i
	if i < len(s) && s[i] == '.' {
		j := i + 1
		for j < len(s) && s[j] >= '0' && s[j] <= '9' {
			j++
		}
		if intDigits == 0 && j == i+1 {
			return 0
		}
		return j
	}
	return i
}

func validFloat(s string) bool {
	if len(s) > 0 && s[0] == '-' {
		s = s[1:]
	}
	n := scanDecimal(s)
	if n == 0 {
		return false
	}
	s = s[n:]
	if s == "" {
		return true
	}
	if s[0] != 'e' && s[0] != 'E' {
		return false
	}
	s = s[1:]
	if len(s) > 0 && (s[0] == '+' || s[0] == '-') {
		s = s[1:]
	}
	return isDigits(s)
}

func parseFloat(raw string) (any, error) {
	if !validFloat(raw) {
		return nil, ErrSyntax
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		if errors.Is(err, strconv.ErrRange) {
			return nil, ErrRange
		}
		return nil, ErrSyntax
	}
	if math.IsInf(v, 0) || math.IsNaN(v) {
		return nil, ErrRange
	}
	return v, nil
}

var durationUnits = []string{"ns", "us", "ms", "s", "m", "h"}

func validDuration(s string) bool {
	if s == "" {
		return false
	}
	for s != "" {
		n := scanDecimal(s)
		if n == 0 {
			return false
		}
		s = s[n:]
		matched := false
		for _, u := range durationUnits {
			if len(s) >= len(u) && s[:len(u)] == u {
				// "ms" must win over "m" followed by "s" (greedy, as in Go).
				s = s[len(u):]
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

func parseDuration(raw string, allowZero bool) (any, error) {
	if !validDuration(raw) {
		return nil, ErrSyntax
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return nil, ErrRange
	}
	if d == 0 && !allowZero {
		return nil, ErrRange
	}
	return d, nil
}

func parseEnum(raw string, values []string, caseSensitive bool) (any, error) {
	for _, v := range values {
		if raw == v || !caseSensitive && asciiEqualFold(raw, v) {
			return v, nil
		}
	}
	return nil, ErrNotInSet
}
