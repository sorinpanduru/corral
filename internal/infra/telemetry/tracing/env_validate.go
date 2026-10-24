package tracing

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net/url"
	"strconv"
	"strings"
)

// validEndpoint reports whether v is an absolute http/https URL with a host
// (D8). The value is parsed only; it is never retained or echoed.
func validEndpoint(v string) bool {
	u, err := url.Parse(v)
	if err != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	if u.Hostname() == "" {
		return false
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n > 65535 {
			return false
		}
	}
	return true
}

// validKeyValueList reports (with tokenKeys, keys must be RFC 7230 tokens and
// are not percent-decoded; values are always percent-decoded) whether v is a comma-separated list of key=value
// entries with non-empty keys and valid percent-encoding (D8).
func validKeyValueList(v string, tokenKeys bool) bool {
	for _, entry := range strings.Split(v, ",") {
		k, val, ok := strings.Cut(entry, "=")
		if !ok {
			return false
		}
		if !tokenKeys {
			dk, err := url.PathUnescape(strings.TrimSpace(k))
			if err != nil || dk == "" {
				return false
			}
		} else if !isToken(strings.TrimSpace(k)) {
			return false
		}
		if _, err := url.PathUnescape(strings.TrimSpace(val)); err != nil {
			return false
		}
	}
	return true
}

// isToken reports whether s is a non-empty RFC 7230 token.
func isToken(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0:
		default:
			return false
		}
	}
	return true
}

// keyPairOK reports whether the certificate and key files form a key pair.
// The bytes come from the per-call cache and are never retained.
func (r *resolver) keyPairOK(certPath, keyPath string) bool {
	c, ok := r.readPEM(certPath)
	if !ok {
		return false
	}
	k, ok := r.readPEM(keyPath)
	if !ok {
		return false
	}
	_, err := tls.X509KeyPair(c, k)
	return err == nil
}

// certFileOK reports whether path is readable and holds at least one PEM
// certificate that parses, and no unparsable certificate block.
func (r *resolver) certFileOK(path string) bool {
	b, ok := r.readPEM(path)
	if !ok {
		return false
	}
	found := false
	for {
		var blk *pem.Block
		blk, b = pem.Decode(b)
		if blk == nil {
			break
		}
		if blk.Type != "CERTIFICATE" {
			continue
		}
		if _, err := x509.ParseCertificate(blk.Bytes); err != nil {
			return false
		}
		found = true
	}
	return found
}

// keyFileOK reports whether path is readable and holds a PEM private key that
// parses (PKCS#8, PKCS#1 or SEC1).
func (r *resolver) keyFileOK(path string) bool {
	b, ok := r.readPEM(path)
	if !ok {
		return false
	}
	for {
		var blk *pem.Block
		blk, b = pem.Decode(b)
		if blk == nil {
			return false
		}
		if !strings.HasSuffix(blk.Type, "PRIVATE KEY") {
			continue
		}
		if _, err := x509.ParsePKCS8PrivateKey(blk.Bytes); err == nil {
			return true
		}
		if _, err := x509.ParsePKCS1PrivateKey(blk.Bytes); err == nil {
			return true
		}
		if _, err := x509.ParseECPrivateKey(blk.Bytes); err == nil {
			return true
		}
		return false
	}
}
