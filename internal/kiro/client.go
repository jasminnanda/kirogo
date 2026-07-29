package kiro

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"kirogo/internal/util"
)

// Request path and target constants.
const (
	generatePath = "/generateAssistantResponse"
	listPath     = "/ListAvailableModels"
	amzTarget    = "AmazonCodeWhispererStreamingService.GenerateAssistantResponse"
)

// Retry policy, ported from the reference gateway.
const (
	// maxAttempts is the total number of tries, not the number of retries.
	maxAttempts = 3
	// baseRetryDelay is doubled per attempt: 1s, 2s, 4s. No jitter.
	baseRetryDelay = time.Second
)

// maxErrorBodyBytes caps how much of an error body is read into memory.
const maxErrorBodyBytes = 1 << 20

// maxListBodyBytes caps the model catalog response size.
const maxListBodyBytes = 8 << 20

// TokenProvider supplies credentials and the derived endpoint details. It is an
// interface so the client can be tested without a real credential store.
type TokenProvider interface {
	// Token returns a valid access token, refreshing it if needed.
	Token(ctx context.Context) (string, error)
	// ForceRefresh renews the token unconditionally, after an upstream 403.
	ForceRefresh(ctx context.Context) (string, error)
	// ProfileARN returns the CodeWhisperer profile ARN, possibly empty.
	ProfileARN() string
	// RuntimeHost returns the Kiro runtime base URL, which serves the streaming
	// chat operation.
	RuntimeHost() string
	// ControlPlaneHost returns the Kiro control plane base URL, which serves the
	// model catalog. It is a different service from the runtime host.
	ControlPlaneHost() string
	// Fingerprint returns the machine fingerprint for the User-Agent.
	Fingerprint() string
	// KiroVersion returns the Kiro IDE version for the User-Agent.
	KiroVersion() string
	// TokenTypeHeader returns the TokenType header value, or an empty string.
	TokenTypeHeader() string
}

// Options configures a Client.
type Options struct {
	// Auth supplies tokens and endpoints.
	Auth TokenProvider
	// AgentMode populates x-amzn-kiro-agent-mode. Defaults to "vibe".
	AgentMode string
	// StreamReadTimeout bounds the gap between streamed chunks.
	StreamReadTimeout time.Duration
	// Sleep is the backoff function. Defaults to time.Sleep, overridden in tests.
	Sleep func(time.Duration)
	// HTTPClient replaces the pooled non-streaming client, for tests.
	HTTPClient *http.Client
	// StreamClientFactory replaces per-request streaming client construction,
	// for tests.
	StreamClientFactory func() *http.Client
}

// Client talks to the Kiro backend.
//
// Non-streaming calls share a pooled client so connections are reused. Streaming
// calls get a fresh client plus Connection: close, because reusing a pooled
// connection for a long-lived stream leaks sockets in CLOSE_WAIT when the
// network interface changes underneath it.
type Client struct {
	auth              TokenProvider
	agentMode         string
	streamReadTimeout time.Duration
	sleep             func(time.Duration)
	pooled            *http.Client
	newStreamClient   func() *http.Client
}

// NewClient builds a Client.
func NewClient(opts Options) *Client {
	c := &Client{
		auth:              opts.Auth,
		agentMode:         opts.AgentMode,
		streamReadTimeout: opts.StreamReadTimeout,
		sleep:             opts.Sleep,
		pooled:            opts.HTTPClient,
		newStreamClient:   opts.StreamClientFactory,
	}
	if c.agentMode == "" {
		c.agentMode = "vibe"
	}
	if c.streamReadTimeout <= 0 {
		c.streamReadTimeout = 300 * time.Second
	}
	if c.sleep == nil {
		c.sleep = time.Sleep
	}
	if c.pooled == nil {
		c.pooled = &http.Client{
			Timeout: 300 * time.Second,
			Transport: &http.Transport{
				Proxy:               http.ProxyFromEnvironment,
				MaxIdleConns:        20,
				MaxIdleConnsPerHost: 10,
				IdleConnTimeout:     90 * time.Second,
				TLSHandshakeTimeout: 15 * time.Second,
			},
		}
	}
	if c.newStreamClient == nil {
		c.newStreamClient = func() *http.Client {
			return &http.Client{
				// No overall timeout: a stream may legitimately run for minutes.
				// The gap between chunks is bounded by ResponseHeaderTimeout and
				// by the caller's own first-token deadline.
				Transport: &http.Transport{
					Proxy:                 http.ProxyFromEnvironment,
					DisableKeepAlives:     true,
					TLSHandshakeTimeout:   15 * time.Second,
					ResponseHeaderTimeout: c.streamReadTimeout,
					ExpectContinueTimeout: time.Second,
				},
			}
		}
	}
	return c
}

// AgentMode returns the configured agent mode.
func (c *Client) AgentMode() string { return c.agentMode }

// requestKind selects the header set for a call.
type requestKind int

const (
	// kindGenerate is the streaming GenerateAssistantResponse call, which belongs
	// to AmazonCodeWhispererStreamingService and is targeted by header.
	kindGenerate requestKind = iota
	// kindREST is a plain REST call such as GET /ListAvailableModels. That
	// operation belongs to AmazonCodeWhispererService and is routed by URI, so
	// sending the streaming service's x-amz-target makes the backend answer 404.
	kindREST
)

// buildHeaders assembles the request headers for a Kiro API call.
//
// The User-Agent string is reproduced verbatim from a working client; the
// x-amzn-* headers are confirmed against the Kiro IDE bundle.
func (c *Client) buildHeaders(token string, kind requestKind) http.Header {
	fingerprint := c.auth.Fingerprint()
	version := c.auth.KiroVersion()
	kiroTag := "KiroIDE-" + version + "-" + fingerprint

	h := http.Header{}
	h.Set("Authorization", "Bearer "+token)
	h.Set("User-Agent", "aws-sdk-js/1.0.27 ua/2.1 os/win32#10.0.19044 lang/js "+
		"md/nodejs#22.21.1 api/codewhispererstreaming#1.0.27 m/E "+kiroTag)
	h.Set("x-amz-user-agent", "aws-sdk-js/1.0.27 "+kiroTag)
	h.Set("x-amzn-codewhisperer-optout", "true")
	h.Set("amz-sdk-invocation-id", util.UUID4())
	h.Set("amz-sdk-request", "attempt=1; max="+strconv.Itoa(maxAttempts))

	// Only some auth methods carry a TokenType.
	if tt := c.auth.TokenTypeHeader(); tt != "" {
		h.Set("TokenType", tt)
	}

	switch kind {
	case kindGenerate:
		h.Set("Content-Type", "application/x-amz-json-1.0")
		h.Set("x-amz-target", amzTarget)
		// agentMode is a member of GenerateAssistantResponseRequest bound to this
		// header, so it belongs only on that call.
		h.Set("x-amzn-kiro-agent-mode", c.agentMode)
		// Prevents the CLOSE_WAIT socket leak seen when a pooled connection is
		// reused for a stream.
		h.Set("Connection", "close")
	case kindREST:
		h.Set("Accept", "application/json")
	}
	return h
}

// GenerateAssistantResponse sends a chat request and returns the streaming
// response.
//
// The caller owns the response body and must close it. Retries happen before any
// body byte is handed back, so a retry never replays a partially consumed stream.
func (c *Client) GenerateAssistantResponse(ctx context.Context, req *Request) (*http.Response, error) {
	if req.AgentMode == "" {
		req.AgentMode = c.agentMode
	}
	payload, err := req.Marshal()
	if err != nil {
		return nil, err
	}

	url := c.auth.RuntimeHost() + generatePath
	c.LogRequestHeaders(true)
	logRequestPayload(url, payload)

	return c.doWithRetry(ctx, http.MethodPost, url, payload, nil, kindGenerate)
}

// ListAvailableModels fetches one page of the model catalog.
func (c *Client) ListAvailableModels(ctx context.Context, nextToken string) (*ListModelsResponse, error) {
	query := url.Values{}
	query.Set("origin", Origin)
	if arn := c.auth.ProfileARN(); arn != "" {
		query.Set("profileArn", arn)
	}
	if nextToken != "" {
		query.Set("nextToken", nextToken)
	}

	// The catalog lives on the control plane, not the runtime host.
	endpoint := c.auth.ControlPlaneHost() + listPath
	resp, err := c.doWithRetry(ctx, http.MethodGet, endpoint, nil, query, kindREST)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxListBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("could not read the model catalog response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, ParseAPIError(resp.StatusCode, body, resp.Header)
	}

	var parsed ListModelsResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("the model catalog response was not valid JSON: %w", err)
	}
	return &parsed, nil
}

// doWithRetry issues a request, applying the retry policy.
//
// The policy, ported unchanged from the reference gateway:
//   - 200: return immediately.
//   - 403: force a token refresh and retry at once, with no backoff.
//   - 429 and 5xx: sleep 1s, 2s, 4s and retry; after the attempts run out,
//     return the last response so the caller sees the real status and body.
//   - any other status: return as-is without retrying.
func (c *Client) doWithRetry(
	ctx context.Context,
	method, endpoint string,
	body []byte,
	query url.Values,
	kind requestKind,
) (*http.Response, error) {
	var (
		lastRetryable *http.Response
		lastErr       error
	)

	// Only one client is built for the non-streaming path; streaming builds a
	// fresh one per attempt so an aborted stream cannot poison a reused socket.
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			closeResponse(lastRetryable)
			return nil, err
		}

		token, err := c.auth.Token(ctx)
		if err != nil {
			closeResponse(lastRetryable)
			return nil, err
		}

		target := endpoint
		if len(query) > 0 {
			target += "?" + query.Encode()
		}

		var reader io.Reader
		if body != nil {
			reader = bytes.NewReader(body)
		}
		httpReq, err := http.NewRequestWithContext(ctx, method, target, reader)
		if err != nil {
			closeResponse(lastRetryable)
			return nil, fmt.Errorf("could not build the Kiro request: %w", err)
		}
		httpReq.Header = c.buildHeaders(token, kind)
		if body != nil {
			httpReq.ContentLength = int64(len(body))
		}

		client := c.pooled
		if kind == kindGenerate {
			client = c.newStreamClient()
		}

		resp, err := client.Do(httpReq)
		if err != nil {
			lastErr = err
			if ctxErr := ctx.Err(); ctxErr != nil {
				closeResponse(lastRetryable)
				return nil, ctxErr
			}
			if attempt < maxAttempts-1 {
				delay := backoffDelay(attempt)
				slog.Warn("Kiro request failed at the network level, retrying",
					"attempt", attempt+1, "of", maxAttempts, "retry_in", delay,
					"error", classifyTransportError(err))
				c.sleep(delay)
				continue
			}
			closeResponse(lastRetryable)
			return nil, transportError(endpoint, err)
		}

		if requestID := resp.Header.Get("x-amzn-requestid"); requestID != "" {
			slog.Debug("Kiro response", "status", resp.StatusCode, "request_id", requestID)
		}

		switch {
		case resp.StatusCode == http.StatusOK:
			closeResponse(lastRetryable)
			return resp, nil

		case resp.StatusCode == http.StatusForbidden:
			// The token was rejected. Refresh and retry at once: waiting would
			// not help, and the refresh is what fixes it.
			drainAndClose(resp)
			slog.Warn("Kiro returned 403, refreshing the access token and retrying",
				"attempt", attempt+1, "of", maxAttempts)
			if _, refreshErr := c.auth.ForceRefresh(ctx); refreshErr != nil {
				closeResponse(lastRetryable)
				return nil, refreshErr
			}
			continue

		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
			// Keep this response: if the retries run out, the caller must see
			// the real status and body rather than a synthesised error.
			closeResponse(lastRetryable)
			lastRetryable = bufferResponse(resp)
			if attempt < maxAttempts-1 {
				delay := backoffDelay(attempt)
				slog.Warn("Kiro returned a retryable status, backing off",
					"status", resp.StatusCode, "attempt", attempt+1, "of", maxAttempts, "retry_in", delay)
				c.sleep(delay)
				continue
			}

		default:
			// Any other 4xx is the caller's problem to interpret.
			closeResponse(lastRetryable)
			return resp, nil
		}
	}

	if lastRetryable != nil {
		slog.Warn("Kiro retries exhausted, returning the last upstream response",
			"status", lastRetryable.StatusCode)
		return lastRetryable, nil
	}
	if lastErr != nil {
		return nil, transportError(endpoint, lastErr)
	}
	// Reached only when every attempt was a 403 whose refresh succeeded.
	return nil, fmt.Errorf("kiro api: the backend rejected kirogo's token on all %d attempts. "+
		"Sign in to Kiro IDE again so a fresh token is written, then restart kirogo", maxAttempts)
}

// backoffDelay returns the delay before the next attempt: 1s, 2s, 4s. No jitter,
// matching the reference gateway.
func backoffDelay(attempt int) time.Duration {
	return baseRetryDelay * time.Duration(1<<attempt)
}

// bufferResponse reads a response body into memory so the response stays usable
// after its connection is released.
func bufferResponse(resp *http.Response) *http.Response {
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
	if err != nil {
		slog.Debug("could not buffer a retryable response body", "error", err)
		body = nil
	}
	drainAndClose(resp)
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	return resp
}

// closeResponse closes a response body when there is one.
func closeResponse(resp *http.Response) {
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
}

// drainAndClose discards and closes a body so the connection can be reused.
func drainAndClose(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBodyBytes))
	resp.Body.Close()
}

// ReadErrorResponse converts a non-200 response into an APIError.
func ReadErrorResponse(resp *http.Response) *APIError {
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
	if err != nil {
		slog.Debug("could not read the Kiro error body", "error", err)
	}
	return ParseAPIError(resp.StatusCode, body, resp.Header)
}

// logRequestPayload writes the exact upstream payload at DEBUG.
//
// This is the escape hatch for the backend's uninformative validation error, so
// it prints the bytes verbatim, minus anything that looks like a credential.
func logRequestPayload(url string, payload []byte) {
	if !slog.Default().Enabled(context.Background(), slog.LevelDebug) {
		return
	}
	slog.Debug("Kiro request payload",
		"url", url,
		"bytes", len(payload),
		"payload", util.Redact(string(payload)))
}

// LogRequestHeaders writes the outgoing headers at DEBUG with the bearer token
// removed.
func (c *Client) LogRequestHeaders(streaming bool) {
	if !slog.Default().Enabled(context.Background(), slog.LevelDebug) {
		return
	}
	// A placeholder token keeps the real one out of the log entirely.
	kind := kindREST
	if streaming {
		kind = kindGenerate
	}
	h := c.buildHeaders("<token>", kind)
	attrs := make([]any, 0, len(h)*2)
	for name, values := range h {
		value := values[0]
		if name == "Authorization" {
			value = "Bearer <redacted>"
		}
		attrs = append(attrs, name, value)
	}
	slog.Debug("Kiro request headers", attrs...)
}

// Copyright (c) 2026 Jasmin (https://github.com/jasminnanda)
// Licensed under the MIT License. See LICENSE in the project root.
