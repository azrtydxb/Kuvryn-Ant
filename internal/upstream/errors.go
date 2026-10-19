package upstream

import (
	"crypto/x509"
	"errors"
	"io"
	"net"
	"syscall"
)

// FetchError is an unavailable-upstream failure carrying a coarse, secret-free
// class ("dns", "dial", "tls", "timeout", "reset", "connection", "status").
// It unwraps to ErrUnavailable so existing errors.Is checks keep working.
type FetchError struct {
	Class string
	Host  string
	// Status is the last upstream HTTP status when Class is "status".
	Status int
}

func (e *FetchError) Error() string { return ErrUnavailable.Error() + " (" + e.Class + ")" }
func (e *FetchError) Unwrap() error { return ErrUnavailable }

// RedirectError reports a redirect whose target is outside the allowed
// origins. It unwraps to ErrPolicy; the origin answered, it just redirected.
type RedirectError struct{ Status int }

func (e *RedirectError) Error() string { return ErrPolicy.Error() + " (redirect off origin)" }
func (e *RedirectError) Unwrap() error { return ErrPolicy }

// classify maps a transport error to a FetchError, preserving one already made
// by the dialer.
func classify(err error, host string) error {
	var fe *FetchError
	if errors.As(err, &fe) {
		return fe
	}
	class := "connection"
	var ne net.Error
	var ua x509.UnknownAuthorityError
	var ci x509.CertificateInvalidError
	var he x509.HostnameError
	switch {
	case errors.As(err, &ua), errors.As(err, &ci), errors.As(err, &he):
		class = "tls"
	case errors.As(err, &ne) && ne.Timeout():
		class = "timeout"
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		class = "reset"
	}
	return &FetchError{Class: class, Host: host}
}
