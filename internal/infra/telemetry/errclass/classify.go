// Package errclass classifies errors into the FR-29 class and detail enums
// and provides the only helpers that put errors into logs and spans.
//
// Compliance (FR-24, FR-26, GDPR data minimisation): nothing in this package
// ever reads err.Error(), an errmsg, or a recovered panic value. Output is
// built only from enum constants, a numeric server code and a server code
// name supplied by the typed server error.
package errclass

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"os"
	"syscall"
)

// Class is the FR-29 error class.
type Class string

// Detail is the FR-29 error detail.
type Detail string

// Error classes.
const (
	ClassTimeout  Class = "timeout"
	ClassNetwork  Class = "network"
	ClassProtocol Class = "protocol"
	ClassServer   Class = "server"
	ClassConfig   Class = "config"
	ClassInternal Class = "internal"
)

// Error details.
const (
	DetailECONNREFUSED     Detail = "ECONNREFUSED"
	DetailECONNRESET       Detail = "ECONNRESET"
	DetailECONNABORTED     Detail = "ECONNABORTED"
	DetailETIMEDOUT        Detail = "ETIMEDOUT"
	DetailEPIPE            Detail = "EPIPE"
	DetailEHOSTUNREACH     Detail = "EHOSTUNREACH"
	DetailENETUNREACH      Detail = "ENETUNREACH"
	DetailEADDRINUSE       Detail = "EADDRINUSE"
	DetailDeadlineExceeded Detail = "deadline_exceeded"
	DetailCanceled         Detail = "canceled"
	DetailEOF              Detail = "eof"
	DetailTLSHandshake     Detail = "tls_handshake"
	DetailDNSResolution    Detail = "dns_resolution"
	DetailOther            Detail = "other"
)

// Classes returns the closed set of classes.
func Classes() []Class {
	return []Class{ClassTimeout, ClassNetwork, ClassProtocol, ClassServer, ClassConfig, ClassInternal}
}

// Details returns the closed set of details.
func Details() []Detail {
	return []Detail{
		DetailECONNREFUSED, DetailECONNRESET, DetailECONNABORTED, DetailETIMEDOUT,
		DetailEPIPE, DetailEHOSTUNREACH, DetailENETUNREACH, DetailEADDRINUSE,
		DetailDeadlineExceeded, DetailCanceled, DetailEOF, DetailTLSHandshake,
		DetailDNSResolution, DetailOther,
	}
}

// ServerError is satisfied by a MongoDB server-reply error.
type ServerError interface {
	error
	ServerCode() int32
	ServerCodeName() string
}

// ConfigError is satisfied by an error wrapped as a configuration error.
type ConfigError interface {
	error
	ConfigSetting() string
}

// ProtocolError is satisfied by wire-parse failures and OTLP collector
// rejections.
type ProtocolError interface {
	error
	ProtocolError()
}

var errnoDetails = []struct {
	errno  syscall.Errno
	detail Detail
}{
	{syscall.ECONNREFUSED, DetailECONNREFUSED},
	{syscall.ECONNRESET, DetailECONNRESET},
	{syscall.ECONNABORTED, DetailECONNABORTED},
	{syscall.ETIMEDOUT, DetailETIMEDOUT},
	{syscall.EPIPE, DetailEPIPE},
	{syscall.EHOSTUNREACH, DetailEHOSTUNREACH},
	{syscall.ENETUNREACH, DetailENETUNREACH},
	{syscall.EADDRINUSE, DetailEADDRINUSE},
}

const (
	maxWalkDepth = 64  // deepest Unwrap level visited
	maxWalkNodes = 256 // total errors visited, bounds join fan-out
)

// walk flattens the Unwrap tree of err (breadth first) into at most
// maxWalkNodes errors, no deeper than maxWalkDepth. It is used instead of
// errors.Is/As so that a cyclic Unwrap chain terminates.
func walk(err error) []error {
	type item struct {
		err   error
		depth int
	}
	var out []error
	queue := []item{{err, 0}}
	for len(queue) > 0 && len(out) < maxWalkNodes {
		it := queue[0]
		queue = queue[1:]
		if it.err == nil {
			continue
		}
		out = append(out, it.err)
		if it.depth >= maxWalkDepth {
			continue
		}
		switch u := it.err.(type) {
		case interface{ Unwrap() error }:
			queue = append(queue, item{u.Unwrap(), it.depth + 1})
		case interface{ Unwrap() []error }:
			for _, e := range u.Unwrap() {
				queue = append(queue, item{e, it.depth + 1})
			}
		}
	}
	return out
}

// anyNode reports whether pred holds for any node.
func anyNode(nodes []error, pred func(error) bool) bool {
	for _, n := range nodes {
		if pred(n) {
			return true
		}
	}
	return false
}

// isTarget reports whether n is target or says it is via an Is method.
func isTarget(n, target error) bool {
	if n == target {
		return true
	}
	if x, ok := n.(interface{ Is(error) bool }); ok {
		return x.Is(target)
	}
	return false
}

func hasTarget(nodes []error, target error) bool {
	return anyNode(nodes, func(n error) bool { return isTarget(n, target) })
}

// Classify maps err to its FR-29 class and detail (plan decision D3). It
// never reads the error text.
//
// Precedence over the whole chain: recovered panic > config > server >
// protocol > errno/sentinels. For the last group the detail is chosen in
// this order: deadline_exceeded, canceled, errno values, eof, dns_resolution,
// tls_handshake, then any net.Error-style Timeout() (ETIMEDOUT). EOF is
// checked before DNS and TLS because a peer closing the stream mid-handshake
// or mid-lookup surfaces as EOF wrapped by the higher-level error, and the
// connection close is the actual cause.
//
// The chain is walked with a bounded walk (depth 64, 256 nodes), so a cyclic
// Unwrap chain terminates and, having no match, yields internal/other. A nil
// error also yields internal/other.
func Classify(err error) (Class, Detail) {
	return classify(walk(err))
}

func classify(nodes []error) (Class, Detail) {
	switch {
	case anyNode(nodes, func(n error) bool { _, ok := n.(*panicError); return ok }):
		return ClassInternal, DetailOther
	case anyNode(nodes, func(n error) bool { _, ok := n.(ConfigError); return ok }):
		return ClassConfig, detailOf(nodes)
	case anyNode(nodes, func(n error) bool { _, ok := n.(ServerError); return ok }):
		return ClassServer, DetailOther
	case anyNode(nodes, func(n error) bool { _, ok := n.(ProtocolError); return ok }):
		return ClassProtocol, DetailOther
	}
	d := detailOf(nodes)
	return classOfDetail(d), d
}

func classOfDetail(d Detail) Class {
	switch d {
	case DetailETIMEDOUT, DetailDeadlineExceeded:
		return ClassTimeout
	case DetailCanceled, DetailOther:
		return ClassInternal
	default:
		return ClassNetwork
	}
}

// detailOf derives the detail from the walked chain only.
func detailOf(nodes []error) Detail {
	switch {
	case hasTarget(nodes, context.DeadlineExceeded), hasTarget(nodes, os.ErrDeadlineExceeded):
		return DetailDeadlineExceeded
	case hasTarget(nodes, context.Canceled):
		return DetailCanceled
	}
	for _, e := range errnoDetails {
		if hasTarget(nodes, e.errno) {
			return e.detail
		}
	}
	if hasTarget(nodes, io.EOF) || hasTarget(nodes, io.ErrUnexpectedEOF) {
		return DetailEOF
	}
	if anyNode(nodes, func(n error) bool {
		switch n.(type) {
		case *net.DNSError:
			return true
		}
		return false
	}) {
		return DetailDNSResolution
	}
	if anyNode(nodes, isTLSNode) {
		return DetailTLSHandshake
	}
	if anyNode(nodes, func(n error) bool {
		t, ok := n.(interface{ Timeout() bool })
		return ok && t.Timeout()
	}) {
		return DetailETIMEDOUT
	}
	return DetailOther
}

func isTLSNode(n error) bool {
	switch n.(type) {
	case tls.RecordHeaderError, *tls.RecordHeaderError,
		*tls.CertificateVerificationError, tls.AlertError,
		x509.UnknownAuthorityError, *x509.UnknownAuthorityError,
		x509.HostnameError, *x509.HostnameError,
		x509.CertificateInvalidError, *x509.CertificateInvalidError,
		x509.SystemRootsError, *x509.SystemRootsError,
		x509.ConstraintViolationError, *x509.ConstraintViolationError,
		x509.InsecureAlgorithmError, x509.UnhandledCriticalExtension:
		return true
	}
	return false
}
