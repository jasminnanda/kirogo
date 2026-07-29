package kiro

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"syscall"
)

// classifyTransportError returns a short, user-facing category for a network
// failure. It is deliberately coarse: the goal is a message the user can act on,
// not a taxonomy.
func classifyTransportError(err error) string {
	if err == nil {
		return ""
	}

	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timed out"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	}

	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "DNS resolution failed"
	}

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timed out"
	}

	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	case errors.Is(err, syscall.ECONNRESET):
		return "connection reset"
	case errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH):
		return "network unreachable"
	}

	msg := err.Error()
	switch {
	case strings.Contains(msg, "certificate"), strings.Contains(msg, "x509"):
		return "TLS certificate rejected"
	case strings.Contains(msg, "tls"):
		return "TLS handshake failed"
	case strings.Contains(msg, "proxy"):
		return "proxy error"
	}
	return "network error"
}

// transportError wraps a network failure with advice tailored to the category.
func transportError(endpoint string, err error) error {
	category := classifyTransportError(err)
	host := endpointHost(endpoint)

	var advice string
	switch category {
	case "DNS resolution failed":
		advice = "kirogo could not resolve " + host + ". Check your DNS settings; on a restricted network you may need HTTPS_PROXY."
	case "connection refused", "network unreachable":
		advice = "kirogo could not open a connection to " + host + ". Check your network, firewall and any HTTPS_PROXY setting."
	case "timed out":
		advice = "The connection to " + host + " timed out. The network may be slow or blocked; retry, and set HTTPS_PROXY if you need a proxy."
	case "connection reset":
		advice = "The connection to " + host + " was reset. This is often a proxy or firewall closing long-lived connections."
	case "TLS certificate rejected", "TLS handshake failed":
		advice = "The TLS handshake with " + host + " failed. A corporate proxy that intercepts TLS needs its certificate in the system trust store."
	case "proxy error":
		advice = "kirogo could not use the configured proxy. Check HTTPS_PROXY and NO_PROXY."
	case "cancelled":
		advice = "The request to " + host + " was cancelled."
	default:
		advice = "kirogo could not complete the request to " + host + "."
	}

	return fmt.Errorf("%s (%s): %w", advice, category, err)
}

// endpointHost extracts a host for an error message, falling back to the raw
// string when it will not parse.
func endpointHost(endpoint string) string {
	if u, err := url.Parse(endpoint); err == nil && u.Host != "" {
		return u.Host
	}
	return endpoint
}

// Copyright (c) 2026 Jasmin (https://github.com/jasminnanda)
// Licensed under the MIT License. See LICENSE in the project root.
