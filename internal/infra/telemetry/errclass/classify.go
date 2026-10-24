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
	"errors"
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

// Classify maps err to its FR-29 class and detail (plan decision D3). It
// never reads the error text.
func Classify(err error) (Class, Detail) {
	if err == nil {
		return ClassInternal, DetailOther
	}
	var pe *panicError
	if errors.As(err, &pe) {
		return ClassInternal, DetailOther
	}
	var se ServerError
	if errors.As(err, &se) {
		return ClassServer, DetailOther
	}
	var ce ConfigError
	if errors.As(err, &ce) {
		return ClassConfig, detailOf(err)
	}
	var pr ProtocolError
	if errors.As(err, &pr) {
		return ClassProtocol, DetailOther
	}
	d := detailOf(err)
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

// detailOf derives the detail from the error chain only.
func detailOf(err error) Detail {
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, os.ErrDeadlineExceeded):
		return DetailDeadlineExceeded
	case errors.Is(err, context.Canceled):
		return DetailCanceled
	}
	for _, e := range errnoDetails {
		if errors.Is(err, e.errno) {
			return e.detail
		}
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return DetailEOF
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return DetailDNSResolution
	}
	if isTLS(err) {
		return DetailTLSHandshake
	}
	if hasTimeout(err) {
		return DetailETIMEDOUT
	}
	return DetailOther
}

func isTLS(err error) bool {
	var (
		rh   tls.RecordHeaderError
		rhp  *tls.RecordHeaderError
		cv   *tls.CertificateVerificationError
		alrt tls.AlertError
		ua   x509.UnknownAuthorityError
		uap  *x509.UnknownAuthorityError
		hn   x509.HostnameError
		hnp  *x509.HostnameError
		ci   x509.CertificateInvalidError
		cip  *x509.CertificateInvalidError
		sr   x509.SystemRootsError
		srp  *x509.SystemRootsError
		cvi  x509.ConstraintViolationError
		cvp  *x509.ConstraintViolationError
		ia   x509.InsecureAlgorithmError
		uc   x509.UnhandledCriticalExtension
	)
	return errors.As(err, &rh) || errors.As(err, &rhp) || errors.As(err, &cv) ||
		errors.As(err, &alrt) || errors.As(err, &ua) || errors.As(err, &uap) ||
		errors.As(err, &hn) || errors.As(err, &hnp) || errors.As(err, &ci) ||
		errors.As(err, &cip) || errors.As(err, &sr) || errors.As(err, &srp) ||
		errors.As(err, &cvi) || errors.As(err, &cvp) || errors.As(err, &ia) ||
		errors.As(err, &uc)
}

// hasTimeout reports whether any error in the chain says Timeout() == true.
func hasTimeout(err error) bool {
	if err == nil {
		return false
	}
	if t, ok := err.(interface{ Timeout() bool }); ok && t.Timeout() {
		return true
	}
	switch u := err.(type) {
	case interface{ Unwrap() error }:
		return hasTimeout(u.Unwrap())
	case interface{ Unwrap() []error }:
		for _, e := range u.Unwrap() {
			if hasTimeout(e) {
				return true
			}
		}
	}
	return false
}
