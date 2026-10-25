package settings

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"strconv"
	"strings"

	"github.com/sorinpanduru/corral/internal/infra/config/registry"
)

// Rule errors are constants: the loader reports only that a rule failed, and
// never carries a value, a path or file content (SPEC-003 FR-11).
var (
	errPort        = errors.New("settings: port out of range")
	errRange       = errors.New("settings: value out of range")
	errDuplicate   = errors.New("settings: duplicate entry")
	errSourceChars = errors.New("settings: authentication database name has a forbidden character")
	errNoCert      = errors.New("settings: no usable PEM certificate")
	errBadKey      = errors.New("settings: not a usable PEM private key")
	errRuleType    = errors.New("settings: unexpected value type")
)

func intIn(lo, hi int64) func(any) error {
	return func(v any) error {
		n, ok := v.(int64)
		if !ok {
			return errRuleType
		}
		if n < lo || n > hi {
			return errRange
		}
		return nil
	}
}

func portOf(hostport string) (int, bool) {
	i := strings.LastIndexByte(hostport, ':')
	if i < 0 {
		return 0, false
	}
	n, err := strconv.Atoi(hostport[i+1:])
	return n, err == nil
}

// listenRule: port 1 to 65535 (the host is already non-empty by type).
func listenRule(v any) error {
	s, ok := v.(string)
	if !ok {
		return errRuleType
	}
	if n, ok := portOf(s); !ok || n < 1 || n > 65535 {
		return errPort
	}
	return nil
}

// hostsRule: non-empty, ports 1 to 65535, no duplicates.
func hostsRule(v any) error {
	hs, ok := v.([]string)
	if !ok {
		return errRuleType
	}
	if len(hs) == 0 {
		return errRange
	}
	seen := map[string]bool{}
	for _, h := range hs {
		if n, ok := portOf(h); !ok || n < 1 || n > 65535 {
			return errPort
		}
		if seen[h] {
			return errDuplicate
		}
		seen[h] = true
	}
	return nil
}

// sourceRule: none of / \ . " $ or space.
func sourceRule(v any) error {
	s, ok := v.(string)
	if !ok {
		return errRuleType
	}
	if strings.ContainsAny(s, "/\\.\"$ ") {
		return errSourceChars
	}
	return nil
}

// pemCertificates is the PEM rule: at least one CERTIFICATE block that parses.
// It returns constant errors only.
func pemCertificates(content []byte) error {
	rest := content
	for {
		var b *pem.Block
		b, rest = pem.Decode(rest)
		if b == nil {
			return errNoCert
		}
		if b.Type == "CERTIFICATE" {
			if _, err := x509.ParseCertificate(b.Bytes); err == nil {
				return nil
			}
		}
	}
}

// pemPrivateKey is the key rule: a PEM private key (PKCS#1, PKCS#8 or SEC 1
// EC) that parses.
func pemPrivateKey(v any) error {
	s, ok := v.(string)
	if !ok {
		return errRuleType
	}
	b, _ := pem.Decode([]byte(s))
	if b == nil {
		return errBadKey
	}
	var err error
	switch b.Type {
	case "PRIVATE KEY":
		_, err = x509.ParsePKCS8PrivateKey(b.Bytes)
	case "RSA PRIVATE KEY":
		_, err = x509.ParsePKCS1PrivateKey(b.Bytes)
	case "EC PRIVATE KEY":
		_, err = x509.ParseECPrivateKey(b.Bytes)
	default:
		return errBadKey
	}
	if err != nil {
		return errBadKey
	}
	return nil
}

type getter = func(name string) (any, bool)

func boolOf(get getter, name string) bool {
	v, ok := get(name)
	b, _ := v.(bool)
	return ok && b
}

func isSet(get getter, name string) bool { _, ok := get(name); return ok }

func intOf(get getter, name string) int64 {
	v, _ := get(name)
	n, _ := v.(int64)
	return n
}

func isNone(get getter) bool {
	v, _ := get("upstream.auth.mechanism")
	s, _ := v.(string)
	return strings.EqualFold(s, "none")
}

func proxyCrossRules() []registry.CrossRule {
	return []registry.CrossRule{
		{ID: "proxy-tls-material", Involves: []string{"proxy.tls.enabled", "proxy.tls.cert_path", "proxy.tls.key"},
			Primary: "proxy.tls.enabled", AppliesToUnset: true,
			Check: func(get getter) error {
				if boolOf(get, "proxy.tls.enabled") && !(isSet(get, "proxy.tls.cert_path") && isSet(get, "proxy.tls.key")) {
					return errRange
				}
				return nil
			}},
		{ID: "proxy-tls-keypair", Involves: []string{"proxy.tls.cert_path", "proxy.tls.key"}, Primary: "proxy.tls.key",
			Check: func(get getter) error {
				c, _ := get("proxy.tls.cert_path")
				k, _ := get("proxy.tls.key")
				fv, ok1 := c.(registry.FileValue)
				ks, ok2 := k.(string)
				if !ok1 || !ok2 {
					return errRuleType
				}
				if _, err := tls.X509KeyPair(fv.Content, []byte(ks)); err != nil {
					return errBadKey
				}
				return nil
			}},
		{ID: "proxy-tls-mtls", Involves: []string{"proxy.tls.client_ca_path", "proxy.tls.enabled"}, Primary: "proxy.tls.client_ca_path",
			Check: func(get getter) error {
				if !boolOf(get, "proxy.tls.enabled") {
					return errRange
				}
				return nil
			}},
		{ID: "pool-min-le-max", Involves: []string{"pool.min_size", "pool.max_size"}, Primary: "pool.min_size",
			Check: func(get getter) error {
				if intOf(get, "pool.min_size") > intOf(get, "pool.max_size") {
					return errRange
				}
				return nil
			}},
		{ID: "upstream-single-host", Involves: []string{"upstream.replica_set", "upstream.hosts"}, Primary: "upstream.hosts", AppliesToUnset: true,
			Check: func(get getter) error {
				if isSet(get, "upstream.replica_set") {
					return nil
				}
				v, _ := get("upstream.hosts")
				hs, _ := v.([]string)
				if len(hs) != 1 {
					return errRange
				}
				return nil
			}},
		{ID: "upstream-auth-credentials", Involves: []string{"upstream.auth.mechanism", "upstream.auth.username", "upstream.auth.password"},
			Primary: "upstream.auth.mechanism", AppliesToUnset: true,
			Check: func(get getter) error {
				u, p := isSet(get, "upstream.auth.username"), isSet(get, "upstream.auth.password")
				if isNone(get) {
					if u || p {
						return errRange
					}
					return nil
				}
				if !(u && p) {
					return errRange
				}
				return nil
			}},
		{ID: "upstream-tls-ca", Involves: []string{"upstream.tls.ca_path", "upstream.tls.enabled"}, Primary: "upstream.tls.ca_path",
			Check: func(get getter) error {
				if !boolOf(get, "upstream.tls.enabled") {
					return errRange
				}
				return nil
			}},
	}
}
