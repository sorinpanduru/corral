package types

import (
	"net/netip"
	"strings"
)

// parseHostPort validates "host:port" (ADR-0011: returned as the validated
// text). Hosts are an IPv4 literal, a bracketed IPv6 literal without a zone,
// or a DNS hostname without a trailing dot (SPEC-003 Amendment 1).
func parseHostPort(raw string) (any, error) {
	if raw == "" {
		return nil, ErrEmpty
	}
	for _, r := range raw {
		if isControl(r) {
			return nil, ErrControlChar
		}
	}
	var host, port string
	if raw[0] == '[' {
		end := strings.IndexByte(raw, ']')
		if end < 0 || end+1 >= len(raw) || raw[end+1] != ':' {
			return nil, ErrSyntax
		}
		host, port = raw[1:end], raw[end+2:]
		if a, err := netip.ParseAddr(host); err != nil || !a.Is6() || a.Zone() != "" {
			return nil, ErrSyntax
		}
	} else {
		i := strings.LastIndexByte(raw, ':')
		if i < 0 {
			return nil, ErrSyntax
		}
		host, port = raw[:i], raw[i+1:]
		if !validHost(host) {
			return nil, ErrSyntax
		}
	}
	if err := validPort(port); err != nil {
		return nil, err
	}
	return raw, nil
}

func validPort(p string) error {
	if !isDigits(p) {
		return ErrSyntax
	}
	if len(p) > 1 && p[0] == '0' {
		return ErrSyntax
	}
	if len(p) > 5 {
		return ErrRange
	}
	n := 0
	for i := 0; i < len(p); i++ {
		n = n*10 + int(p[i]-'0')
	}
	if n > 65535 {
		return ErrRange
	}
	return nil
}

func validHost(h string) bool {
	if h == "" {
		return false
	}
	if a, err := netip.ParseAddr(h); err == nil {
		return a.Is4()
	}
	return validHostname(h)
}

func validHostname(h string) bool {
	if h == "" || len(h) > 253 {
		return false
	}
	labels := strings.Split(h, ".")
	for _, l := range labels {
		if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return false
		}
		for i := 0; i < len(l); i++ {
			c := l[i]
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	last := labels[len(labels)-1]
	if isDigits(last) {
		return false
	}
	if len(last) >= 2 && last[0] == '0' && (last[1] == 'x' || last[1] == 'X') {
		hex := true
		for i := 2; i < len(last); i++ {
			c := last[i]
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
				hex = false
			}
		}
		if hex {
			return false
		}
	}
	return true
}
