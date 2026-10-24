package tracing

import (
	"crypto/x509"
	"encoding/pem"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// maxPEMFileBytes bounds how much of a certificate or key file is read.
const maxPEMFileBytes = 1 << 20

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
		if err != nil || n < 0 || n > 65535 {
			return false
		}
	}
	return true
}

// validKeyValueList reports whether v is a comma-separated list of key=value
// entries with non-empty keys and valid percent-encoding (D8).
func validKeyValueList(v string) bool {
	for _, entry := range strings.Split(v, ",") {
		k, val, ok := strings.Cut(entry, "=")
		if !ok {
			return false
		}
		dk, err := url.PathUnescape(strings.TrimSpace(k))
		if err != nil || dk == "" {
			return false
		}
		if _, err := url.PathUnescape(strings.TrimSpace(val)); err != nil {
			return false
		}
	}
	return true
}

// readPEMFile reads at most maxPEMFileBytes from path. Contents are returned to
// the caller for parsing only and never retained or logged (FR-24).
func readPEMFile(path string) ([]byte, bool) {
	f, err := os.Open(path) // #nosec G304 -- operator-supplied path, validated not echoed
	if err != nil {
		return nil, false
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxPEMFileBytes+1))
	if err != nil || len(b) == 0 || len(b) > maxPEMFileBytes {
		return nil, false
	}
	return b, true
}

// certFileOK reports whether path is readable and holds at least one PEM
// certificate that parses, and no unparsable certificate block.
func certFileOK(path string) bool {
	b, ok := readPEMFile(path)
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
func keyFileOK(path string) bool {
	b, ok := readPEMFile(path)
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
