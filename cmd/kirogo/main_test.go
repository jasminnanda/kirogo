package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"syscall"
	"testing"
	"time"

	"github.com/jasminnanda/kirogo/internal/auth"
	"github.com/jasminnanda/kirogo/internal/catalog"
	"github.com/jasminnanda/kirogo/internal/kiro"
)

// scriptedFetcher returns a queued error per call, then succeeds.
type scriptedFetcher struct {
	errs  []error
	calls int
}

func (f *scriptedFetcher) ListAvailableModels(context.Context, string) (*kiro.ListModelsResponse, error) {
	idx := f.calls
	f.calls++
	if idx < len(f.errs) && f.errs[idx] != nil {
		return nil, f.errs[idx]
	}
	return &kiro.ListModelsResponse{Models: []kiro.ModelSpec{
		{ModelID: "claude-opus-5", ModelName: "Claude Opus 5"},
	}}, nil
}

// dnsFailure mimics a resolver that is not answering yet, which is the failure a
// service hits when it starts moments after boot.
func dnsFailure() error {
	return fmt.Errorf("could not fetch the model catalog: %w",
		&auth.RefreshError{
			Flow:    auth.FlowKiroDesktop,
			Message: "could not reach the token refresh endpoint",
			Err: &net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{
				Err: "server misbehaving", Name: "prod.us-east-1.auth.desktop.kiro.dev",
			}},
		})
}

// credentialRejected mimics the refresh endpoint answering with a status, which no
// amount of waiting will fix.
func credentialRejected() error {
	return fmt.Errorf("could not fetch the model catalog: %w",
		&auth.RefreshError{
			StatusCode: http.StatusBadRequest,
			Flow:       auth.FlowKiroDesktop,
			Message:    "the refresh token was rejected",
		})
}

// recordingSleeper captures the backoff schedule instead of waiting.
type recordingSleeper struct{ waits []time.Duration }

func (r *recordingSleeper) sleep(d time.Duration) { r.waits = append(r.waits, d) }

func newCatalog(f catalog.Fetcher) *catalog.Catalog {
	return catalog.New(catalog.Options{Fetcher: f, TTL: time.Hour})
}

// ---------- loadCatalog ----------

func TestLoadCatalogSucceedsFirstTryWithoutWaiting(t *testing.T) {
	f := &scriptedFetcher{}
	s := &recordingSleeper{}

	if err := loadCatalog(context.Background(), newCatalog(f), s.sleep); err != nil {
		t.Fatalf("loadCatalog() = %v, want nil", err)
	}
	if f.calls != 1 {
		t.Errorf("fetcher called %d times, want exactly 1", f.calls)
	}
	if len(s.waits) != 0 {
		t.Errorf("slept %v on a clean first attempt; startup must not be delayed", s.waits)
	}
}

func TestLoadCatalogWaitsThroughTheBootDNSRace(t *testing.T) {
	// The real observed failure: DNS is not answering for the first few seconds
	// after boot, then it is.
	f := &scriptedFetcher{errs: []error{dnsFailure(), dnsFailure(), dnsFailure()}}
	s := &recordingSleeper{}

	if err := loadCatalog(context.Background(), newCatalog(f), s.sleep); err != nil {
		t.Fatalf("loadCatalog() = %v, want nil once DNS recovers", err)
	}
	if f.calls != 4 {
		t.Errorf("fetcher called %d times, want 4 (3 failures then success)", f.calls)
	}
	want := []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second}
	if len(s.waits) != len(want) {
		t.Fatalf("backoff = %v, want %v", s.waits, want)
	}
	for i := range want {
		if s.waits[i] != want[i] {
			t.Errorf("backoff[%d] = %v, want %v", i, s.waits[i], want[i])
		}
	}
}

func TestLoadCatalogBackoffIsCappedAndBounded(t *testing.T) {
	// Every attempt fails, so this exercises the full schedule and the cap.
	errs := make([]error, startupCatalogAttempts)
	for i := range errs {
		errs[i] = dnsFailure()
	}
	f := &scriptedFetcher{errs: errs}
	s := &recordingSleeper{}

	err := loadCatalog(context.Background(), newCatalog(f), s.sleep)
	if err == nil {
		t.Fatal("loadCatalog() = nil, want the final failure surfaced")
	}
	if f.calls != startupCatalogAttempts {
		t.Errorf("fetcher called %d times, want %d", f.calls, startupCatalogAttempts)
	}
	if len(s.waits) != startupCatalogAttempts-1 {
		t.Errorf("slept %d times, want %d: the last attempt must not sleep afterwards",
			len(s.waits), startupCatalogAttempts-1)
	}
	for i, d := range s.waits {
		if d > startupMaxBackoff {
			t.Errorf("backoff[%d] = %v, which exceeds the %v cap", i, d, startupMaxBackoff)
		}
	}
	// Doubling from 1s must reach the cap rather than growing without limit.
	if last := s.waits[len(s.waits)-1]; last != startupMaxBackoff {
		t.Errorf("final backoff = %v, want the cap %v", last, startupMaxBackoff)
	}
	var total time.Duration
	for _, d := range s.waits {
		total += d
	}
	if total > 45*time.Second {
		t.Errorf("total wait %v is too long for a boot-time service", total)
	}
}

func TestLoadCatalogFailsFastOnARejectedCredential(t *testing.T) {
	// Waiting cannot fix a bad token, and a user staring at a dead service deserves
	// the real reason immediately.
	f := &scriptedFetcher{errs: []error{credentialRejected()}}
	s := &recordingSleeper{}

	err := loadCatalog(context.Background(), newCatalog(f), s.sleep)
	if err == nil {
		t.Fatal("loadCatalog() = nil, want the rejection surfaced")
	}
	if f.calls != 1 {
		t.Errorf("fetcher called %d times, want 1: a rejected credential must not be retried", f.calls)
	}
	if len(s.waits) != 0 {
		t.Errorf("slept %v before reporting a permanent failure", s.waits)
	}
}

func TestLoadCatalogFailsFastWhenKiroRefusesTheRequest(t *testing.T) {
	f := &scriptedFetcher{errs: []error{fmt.Errorf("could not fetch the model catalog: %w",
		&kiro.APIError{StatusCode: http.StatusForbidden, Message: "not authorised"})}}
	s := &recordingSleeper{}

	if err := loadCatalog(context.Background(), newCatalog(f), s.sleep); err == nil {
		t.Fatal("loadCatalog() = nil, want the API error surfaced")
	}
	if f.calls != 1 {
		t.Errorf("fetcher called %d times, want 1: a modelled API error must not be retried", f.calls)
	}
}

func TestLoadCatalogStopsWhenTheContextIsCancelled(t *testing.T) {
	// Shutting the service down mid-retry must not wait out the whole schedule.
	ctx, cancel := context.WithCancel(context.Background())
	errs := make([]error, startupCatalogAttempts)
	for i := range errs {
		errs[i] = dnsFailure()
	}
	f := &scriptedFetcher{errs: errs}

	err := loadCatalog(ctx, newCatalog(f), func(time.Duration) { cancel() })
	if !errors.Is(err, context.Canceled) {
		t.Errorf("loadCatalog() = %v, want context.Canceled", err)
	}
	if f.calls != 1 {
		t.Errorf("fetcher called %d times, want 1 before the cancellation took effect", f.calls)
	}
}

// ---------- retryableAtStartup ----------

func TestRetryableAtStartup(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"resolver not up yet", dnsFailure(), true},
		{"connection refused", fmt.Errorf("dial: %w", syscall.ECONNREFUSED), true},
		{"connection reset", fmt.Errorf("read: %w", syscall.ECONNRESET), true},
		{"network unreachable", fmt.Errorf("dial: %w", syscall.EHOSTUNREACH), true},
		{"timed out", fmt.Errorf("wrapped: %w", context.DeadlineExceeded), true},

		{"rejected credential", credentialRejected(), false},
		{"kiro refused the request", &kiro.APIError{StatusCode: 403, Message: "no"}, false},
		{"cancelled deliberately", fmt.Errorf("wrapped: %w", context.Canceled), false},
		{"bad TLS certificate", errors.New("x509: certificate signed by unknown authority"), false},
		{"proxy misconfigured", errors.New("proxy connect refused by proxy"), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := retryableAtStartup(tc.err); got != tc.want {
				t.Errorf("retryableAtStartup() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRetryableAtStartupPrefersThePermanentSignal(t *testing.T) {
	// A rejected credential wrapped around a transport-shaped message must still be
	// treated as permanent: the status code is the authoritative signal.
	err := fmt.Errorf("could not fetch the model catalog: %w", &auth.RefreshError{
		StatusCode: http.StatusUnauthorized,
		Message:    "connection refused by the identity provider",
	})
	if retryableAtStartup(err) {
		t.Error("a refresh failure carrying a status code must not be retried")
	}
}

// Copyright (c) 2026 Jasmin (https://github.com/jasminnanda)
// Licensed under the MIT License. See LICENSE in the project root.
