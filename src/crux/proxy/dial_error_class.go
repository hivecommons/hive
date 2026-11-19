package proxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"strings"
	"syscall"
)

// Dial error classes carried as the "error_class" log field on upstream dial
// failures (#18867), so an operator can tell a flaky egress path (timeout,
// unreachable) from a policy or name problem (refused, dns, tls) at a glance.
const (
	dialErrTimeout     = "timeout"
	dialErrRefused     = "refused"
	dialErrDNS         = "dns"
	dialErrUnreachable = "unreachable"
	dialErrTLS         = "tls"
	dialErrReset       = "reset"
	dialErrOther       = "other"
)

// classifyDialError buckets err from a TCP/TLS upstream dial. Diagnostic only.
func classifyDialError(err error) string {
	if err == nil {
		return dialErrOther
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return dialErrDNS
	}
	var certErr x509.UnknownAuthorityError
	var hostErr x509.HostnameError
	var recErr tls.RecordHeaderError
	if errors.As(err, &certErr) || errors.As(err, &hostErr) || errors.As(err, &recErr) {
		return dialErrTLS
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return dialErrRefused
	}
	if errors.Is(err, syscall.ECONNRESET) {
		return dialErrReset
	}
	if errors.Is(err, syscall.ENETUNREACH) || errors.Is(err, syscall.EHOSTUNREACH) {
		return dialErrUnreachable
	}
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) ||
		strings.Contains(err.Error(), "i/o timeout") {
		return dialErrTimeout
	}
	if strings.HasPrefix(err.Error(), "tls:") || strings.Contains(err.Error(), "tls: ") {
		return dialErrTLS
	}
	return dialErrOther
}
