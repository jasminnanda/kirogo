package kiro

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeAuth is a TokenProvider backed by fixed values.
type fakeAuth struct {
	mu             sync.Mutex
	token          string
	host           string
	profileARN     string
	tokenType      string
	refreshCount   int
	refreshErr     error
	tokenErr       error
	tokensObserved []string
}

func (f *fakeAuth) Token(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tokenErr != nil {
		return "", f.tokenErr
	}
	f.tokensObserved = append(f.tokensObserved, f.token)
	return f.token, nil
}

func (f *fakeAuth) ForceRefresh(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.refreshErr != nil {
		return "", f.refreshErr
	}
	f.refreshCount++
	f.token = "refreshed-token-" + string(rune('a'+f.refreshCount-1))
	return f.token, nil
}

func (f *fakeAuth) Refreshes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.refreshCount
}

func (f *fakeAuth) ProfileARN() string       { return f.profileARN }
func (f *fakeAuth) RuntimeHost() string      { return f.host }
func (f *fakeAuth) ControlPlaneHost() string { return f.host }
func (f *fakeAuth) Fingerprint() string      { return "deadbeefdeadbeef" }
func (f *fakeAuth) KiroVersion() string      { return "0.7.45" }
func (f *fakeAuth) TokenTypeHeader() string  { return f.tokenType }

// capturedRequest records what the stub upstream received.
type capturedRequest struct {
	Method string
	Path   string
	Query  string
	Header http.Header
	Body   []byte
}

// stubUpstream is a scripted Kiro backend.
type stubUpstream struct {
	*httptest.Server
	mu       sync.Mutex
	requests []capturedRequest
	// responses is consumed one per request; the last entry repeats.
	responses []stubResponse
}

// stubResponse is one scripted reply.
type stubResponse struct {
	Status int
	Body   string
	Header map[string]string
	Delay  time.Duration
}

// newStubUpstream starts a backend that replays the given responses.
func newStubUpstream(t *testing.T, responses ...stubResponse) *stubUpstream {
	t.Helper()
	s := &stubUpstream{responses: responses}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		idx := len(s.requests)
		s.requests = append(s.requests, capturedRequest{
			Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery,
			Header: r.Header.Clone(), Body: body,
		})
		resp := stubResponse{Status: http.StatusOK, Body: "{}"}
		if len(s.responses) > 0 {
			if idx < len(s.responses) {
				resp = s.responses[idx]
			} else {
				resp = s.responses[len(s.responses)-1]
			}
		}
		s.mu.Unlock()

		if resp.Delay > 0 {
			time.Sleep(resp.Delay)
		}
		for k, v := range resp.Header {
			w.Header().Set(k, v)
		}
		w.WriteHeader(resp.Status)
		_, _ = io.WriteString(w, resp.Body)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *stubUpstream) Requests() []capturedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]capturedRequest, len(s.requests))
	copy(out, s.requests)
	return out
}

// newTestClient wires a Client to a stub upstream with instant backoff.
func newTestClient(t *testing.T, up *stubUpstream, auth *fakeAuth) (*Client, *[]time.Duration) {
	t.Helper()
	if auth.token == "" {
		auth.token = "initial-token"
	}
	auth.host = up.URL

	var slept []time.Duration
	var mu sync.Mutex
	c := NewClient(Options{
		Auth: auth,
		Sleep: func(d time.Duration) {
			mu.Lock()
			slept = append(slept, d)
			mu.Unlock()
		},
		HTTPClient:          up.Client(),
		StreamClientFactory: func() *http.Client { return up.Client() },
	})
	return c, &slept
}

// simpleRequest is a minimal valid request for transport tests.
func simpleRequest() *Request {
	return &Request{
		ConversationState: ConversationState{
			ChatTriggerType: ChatTriggerType,
			ConversationID:  "conv-test",
			CurrentMessage: CurrentMessage{
				UserInputMessage: &UserInputMessage{Content: "hello", ModelID: "claude-opus-5", Origin: Origin},
			},
		},
	}
}

func TestRequestHeaders(t *testing.T) {
	up := newStubUpstream(t, stubResponse{Status: http.StatusOK, Body: ""})
	auth := &fakeAuth{token: "my-access-token", profileARN: "arn:aws:codewhisperer:us-east-1:1:profile/A"}
	c, _ := newTestClient(t, up, auth)

	resp, err := c.GenerateAssistantResponse(context.Background(), simpleRequest())
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	reqs := up.Requests()
	if len(reqs) != 1 {
		t.Fatalf("upstream saw %d requests, want 1", len(reqs))
	}
	h := reqs[0].Header

	exact := map[string]string{
		"Authorization":               "Bearer my-access-token",
		"Content-Type":                "application/x-amz-json-1.0",
		"X-Amz-Target":                "AmazonCodeWhispererStreamingService.GenerateAssistantResponse",
		"X-Amz-User-Agent":            "aws-sdk-js/1.0.27 KiroIDE-0.7.45-deadbeefdeadbeef",
		"X-Amzn-Codewhisperer-Optout": "true",
		"X-Amzn-Kiro-Agent-Mode":      "vibe",
		"Amz-Sdk-Request":             "attempt=1; max=3",
		"User-Agent": "aws-sdk-js/1.0.27 ua/2.1 os/win32#10.0.19044 lang/js " +
			"md/nodejs#22.21.1 api/codewhispererstreaming#1.0.27 m/E KiroIDE-0.7.45-deadbeefdeadbeef",
	}
	for name, want := range exact {
		if got := h.Get(name); got != want {
			t.Errorf("header %s = %q, want %q", name, got, want)
		}
	}

	if id := h.Get("Amz-Sdk-Invocation-Id"); len(id) != 36 {
		t.Errorf("amz-sdk-invocation-id = %q, want a uuid4", id)
	}
	if h.Get("TokenType") != "" {
		t.Errorf("TokenType = %q, want it absent for an auth method that needs none", h.Get("TokenType"))
	}
	if reqs[0].Path != generatePath {
		t.Errorf("path = %q, want %q", reqs[0].Path, generatePath)
	}
	if reqs[0].Method != http.MethodPost {
		t.Errorf("method = %q, want POST", reqs[0].Method)
	}
}

func TestInvocationIDIsUniquePerRequest(t *testing.T) {
	up := newStubUpstream(t, stubResponse{Status: http.StatusOK})
	c, _ := newTestClient(t, up, &fakeAuth{})

	for i := 0; i < 3; i++ {
		resp, err := c.GenerateAssistantResponse(context.Background(), simpleRequest())
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	seen := map[string]bool{}
	for _, r := range up.Requests() {
		id := r.Header.Get("Amz-Sdk-Invocation-Id")
		if seen[id] {
			t.Errorf("invocation id %q was reused across requests", id)
		}
		seen[id] = true
	}
}

func TestTokenTypeHeaderIsSentWhenTheAuthMethodNeedsIt(t *testing.T) {
	up := newStubUpstream(t, stubResponse{Status: http.StatusOK})
	c, _ := newTestClient(t, up, &fakeAuth{tokenType: "EXTERNAL_IDP"})

	resp, err := c.GenerateAssistantResponse(context.Background(), simpleRequest())
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if got := up.Requests()[0].Header.Get("TokenType"); got != "EXTERNAL_IDP" {
		t.Errorf("TokenType = %q, want EXTERNAL_IDP", got)
	}
}

func TestStreamingSendsConnectionClose(t *testing.T) {
	c := NewClient(Options{Auth: &fakeAuth{}})

	streaming := c.buildHeaders("tok", kindGenerate)
	if got := streaming.Get("Connection"); !strings.EqualFold(got, "close") {
		t.Errorf("streaming Connection header = %q, want close to avoid the CLOSE_WAIT socket leak", got)
	}

	pooled := c.buildHeaders("tok", kindREST)
	if got := pooled.Get("Connection"); got != "" {
		t.Errorf("non-streaming Connection header = %q, want it absent so the pool is used", got)
	}
}

func TestStreamingUsesAFreshClientPerAttempt(t *testing.T) {
	up := newStubUpstream(t,
		stubResponse{Status: 503, Body: `{"message":"try again"}`},
		stubResponse{Status: http.StatusOK, Body: "ok"},
	)
	auth := &fakeAuth{host: up.URL, token: "t"}

	var built atomic.Int64
	c := NewClient(Options{
		Auth:       auth,
		HTTPClient: up.Client(),
		StreamClientFactory: func() *http.Client {
			built.Add(1)
			return up.Client()
		},
		Sleep: func(time.Duration) {},
	})

	resp, err := c.GenerateAssistantResponse(context.Background(), simpleRequest())
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if n := built.Load(); n != 2 {
		t.Errorf("built %d streaming clients, want one per attempt (2)", n)
	}
}

func TestStreamingTransportDisablesKeepAlives(t *testing.T) {
	c := NewClient(Options{Auth: &fakeAuth{}, StreamReadTimeout: 42 * time.Second})
	client := c.newStreamClient()

	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("streaming transport is %T, want *http.Transport", client.Transport)
	}
	if !transport.DisableKeepAlives {
		t.Error("the streaming transport must disable keep-alives")
	}
	if transport.ResponseHeaderTimeout != 42*time.Second {
		t.Errorf("ResponseHeaderTimeout = %v, want the configured stream read timeout", transport.ResponseHeaderTimeout)
	}
	if client.Timeout != 0 {
		t.Errorf("streaming client Timeout = %v, want 0: a stream may run for minutes", client.Timeout)
	}
}

func TestPooledTransportReusesConnections(t *testing.T) {
	c := NewClient(Options{Auth: &fakeAuth{}})
	transport, ok := c.pooled.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("pooled transport is %T, want *http.Transport", c.pooled.Transport)
	}
	if transport.DisableKeepAlives {
		t.Error("the pooled transport must keep connections alive")
	}
	if transport.MaxIdleConnsPerHost <= 0 {
		t.Error("the pooled transport should allow idle connections per host")
	}
	if transport.Proxy == nil {
		t.Error("the pooled transport must honour HTTPS_PROXY")
	}
}

func TestNonStreamingDoesNotSendConnectionClose(t *testing.T) {
	up := newStubUpstream(t, stubResponse{Status: http.StatusOK, Body: `{"models":[]}`})
	c, _ := newTestClient(t, up, &fakeAuth{})

	if _, err := c.ListAvailableModels(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if got := up.Requests()[0].Header.Get("Connection"); strings.EqualFold(got, "close") {
		t.Error("the pooled, non-streaming path should keep connections alive")
	}
}

func TestAgentModeIsConfigurable(t *testing.T) {
	up := newStubUpstream(t, stubResponse{Status: http.StatusOK})
	auth := &fakeAuth{host: up.URL, token: "t"}
	c := NewClient(Options{
		Auth: auth, AgentMode: "spec",
		HTTPClient:          up.Client(),
		StreamClientFactory: func() *http.Client { return up.Client() },
		Sleep:               func(time.Duration) {},
	})
	if c.AgentMode() != "spec" {
		t.Errorf("AgentMode() = %q, want spec", c.AgentMode())
	}

	resp, err := c.GenerateAssistantResponse(context.Background(), simpleRequest())
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if got := up.Requests()[0].Header.Get("X-Amzn-Kiro-Agent-Mode"); got != "spec" {
		t.Errorf("x-amzn-kiro-agent-mode = %q, want spec", got)
	}
	// It must still be absent from the body.
	if bytes.Contains(up.Requests()[0].Body, []byte("agentMode")) {
		t.Error("agentMode leaked into the request body")
	}
}

func TestAgentModeDefaultsToVibe(t *testing.T) {
	c := NewClient(Options{Auth: &fakeAuth{}})
	if c.AgentMode() != "vibe" {
		t.Errorf("AgentMode() = %q, want vibe", c.AgentMode())
	}
}

func TestBodyIsTheMarshalledRequest(t *testing.T) {
	up := newStubUpstream(t, stubResponse{Status: http.StatusOK})
	c, _ := newTestClient(t, up, &fakeAuth{})

	req := fullyPopulatedRequest()
	resp, err := c.GenerateAssistantResponse(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if got := string(up.Requests()[0].Body); got != goldenRequestJSON {
		t.Errorf("upstream body =\n %s\nwant\n %s", got, goldenRequestJSON)
	}
}

func TestSuccessReturnsImmediatelyWithoutRetrying(t *testing.T) {
	up := newStubUpstream(t, stubResponse{Status: http.StatusOK, Body: "stream-bytes"})
	c, slept := newTestClient(t, up, &fakeAuth{})

	resp, err := c.GenerateAssistantResponse(context.Background(), simpleRequest())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "stream-bytes" {
		t.Errorf("body = %q, want the upstream stream handed straight through", body)
	}
	if len(up.Requests()) != 1 {
		t.Errorf("made %d requests, want 1", len(up.Requests()))
	}
	if len(*slept) != 0 {
		t.Errorf("slept %v, want no backoff on success", *slept)
	}
}

func Test403TriggersRefreshAndImmediateRetry(t *testing.T) {
	up := newStubUpstream(t,
		stubResponse{Status: http.StatusForbidden, Body: `{"message":"expired"}`},
		stubResponse{Status: http.StatusOK, Body: "ok"},
	)
	auth := &fakeAuth{}
	c, slept := newTestClient(t, up, auth)

	resp, err := c.GenerateAssistantResponse(context.Background(), simpleRequest())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 after the refresh", resp.StatusCode)
	}
	if auth.Refreshes() != 1 {
		t.Errorf("ForceRefresh called %d times, want 1", auth.Refreshes())
	}
	if len(*slept) != 0 {
		t.Errorf("slept %v, want no backoff on a 403: refreshing is what fixes it", *slept)
	}

	reqs := up.Requests()
	if len(reqs) != 2 {
		t.Fatalf("made %d requests, want 2", len(reqs))
	}
	if got := reqs[0].Header.Get("Authorization"); got != "Bearer initial-token" {
		t.Errorf("first attempt used %q", got)
	}
	if got := reqs[1].Header.Get("Authorization"); got == "Bearer initial-token" {
		t.Error("the retry reused the rejected token instead of the refreshed one")
	}
}

func TestAll403sExhaustAttemptsWithAClearError(t *testing.T) {
	up := newStubUpstream(t, stubResponse{Status: http.StatusForbidden, Body: `{"message":"nope"}`})
	auth := &fakeAuth{}
	c, _ := newTestClient(t, up, auth)

	_, err := c.GenerateAssistantResponse(context.Background(), simpleRequest())
	if err == nil {
		t.Fatal("expected an error when every attempt is refused")
	}
	if !strings.Contains(err.Error(), "rejected kirogo's token") {
		t.Errorf("error = %q, want an explanation about the token", err)
	}
	if !strings.Contains(err.Error(), "Sign in to Kiro IDE") {
		t.Errorf("error = %q, want an actionable next step", err)
	}
	if n := len(up.Requests()); n != maxAttempts {
		t.Errorf("made %d requests, want %d", n, maxAttempts)
	}
}

func Test403WithFailingRefreshPropagatesTheRefreshError(t *testing.T) {
	up := newStubUpstream(t, stubResponse{Status: http.StatusForbidden})
	want := errors.New("refresh token rejected")
	auth := &fakeAuth{refreshErr: want}
	c, _ := newTestClient(t, up, auth)

	_, err := c.GenerateAssistantResponse(context.Background(), simpleRequest())
	if !errors.Is(err, want) {
		t.Errorf("error = %v, want the refresh failure surfaced", err)
	}
}

func TestRetryableStatusesBackOffAndReturnTheLastResponse(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, 500, 502, 503, 504, 599} {
		t.Run(http.StatusText(status)+"-"+string(rune('0'+status/100)), func(t *testing.T) {
			up := newStubUpstream(t, stubResponse{
				Status: status,
				Body:   `{"message":"upstream said no","reason":"SOME_REASON"}`,
				Header: map[string]string{"x-amzn-requestid": "req-123"},
			})
			c, slept := newTestClient(t, up, &fakeAuth{})

			resp, err := c.GenerateAssistantResponse(context.Background(), simpleRequest())
			if err != nil {
				t.Fatalf("retries running out must return the last response, not an error: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != status {
				t.Errorf("status = %d, want the real upstream status %d", resp.StatusCode, status)
			}
			body, _ := io.ReadAll(resp.Body)
			if !strings.Contains(string(body), "upstream said no") {
				t.Errorf("body = %q, want the upstream body preserved", body)
			}
			if got := resp.Header.Get("x-amzn-requestid"); got != "req-123" {
				t.Errorf("request id header = %q, want it preserved", got)
			}
			if n := len(up.Requests()); n != maxAttempts {
				t.Errorf("made %d requests, want %d", n, maxAttempts)
			}
			want := []time.Duration{time.Second, 2 * time.Second}
			if len(*slept) != len(want) {
				t.Fatalf("slept %v, want %v", *slept, want)
			}
			for i := range want {
				if (*slept)[i] != want[i] {
					t.Errorf("backoff %d = %v, want %v", i, (*slept)[i], want[i])
				}
			}
		})
	}
}

func TestRetryableThenSuccess(t *testing.T) {
	up := newStubUpstream(t,
		stubResponse{Status: http.StatusTooManyRequests, Body: `{"message":"slow down"}`},
		stubResponse{Status: 503, Body: `{"message":"unavailable"}`},
		stubResponse{Status: http.StatusOK, Body: "finally"},
	)
	c, slept := newTestClient(t, up, &fakeAuth{})

	resp, err := c.GenerateAssistantResponse(context.Background(), simpleRequest())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "finally" {
		t.Errorf("body = %q", body)
	}
	if want := []time.Duration{time.Second, 2 * time.Second}; len(*slept) != 2 || (*slept)[0] != want[0] || (*slept)[1] != want[1] {
		t.Errorf("backoff = %v, want %v", *slept, want)
	}
}

func TestNonRetryableClientErrorsAreReturnedAsIs(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusPaymentRequired,
		http.StatusNotFound, http.StatusConflict, http.StatusRequestEntityTooLarge} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			up := newStubUpstream(t, stubResponse{Status: status, Body: `{"message":"Improperly formed request."}`})
			c, slept := newTestClient(t, up, &fakeAuth{})

			resp, err := c.GenerateAssistantResponse(context.Background(), simpleRequest())
			if err != nil {
				t.Fatalf("a non-retryable status should be returned, not raised: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != status {
				t.Errorf("status = %d, want %d", resp.StatusCode, status)
			}
			if n := len(up.Requests()); n != 1 {
				t.Errorf("made %d requests, want exactly 1 with no retry", n)
			}
			if len(*slept) != 0 {
				t.Errorf("slept %v, want no backoff", *slept)
			}
		})
	}
}

func TestTokenErrorAbortsBeforeAnyRequest(t *testing.T) {
	up := newStubUpstream(t, stubResponse{Status: http.StatusOK})
	want := errors.New("no credentials")
	c, _ := newTestClient(t, up, &fakeAuth{tokenErr: want})

	_, err := c.GenerateAssistantResponse(context.Background(), simpleRequest())
	if !errors.Is(err, want) {
		t.Errorf("error = %v, want the token failure", err)
	}
	if n := len(up.Requests()); n != 0 {
		t.Errorf("made %d requests, want none without a token", n)
	}
}

func TestContextCancellationStopsRetrying(t *testing.T) {
	up := newStubUpstream(t, stubResponse{Status: http.StatusTooManyRequests})
	auth := &fakeAuth{host: up.URL, token: "t"}

	ctx, cancel := context.WithCancel(context.Background())
	c := NewClient(Options{
		Auth:                auth,
		HTTPClient:          up.Client(),
		StreamClientFactory: func() *http.Client { return up.Client() },
		// Cancel during the first backoff, so the loop must notice.
		Sleep: func(time.Duration) { cancel() },
	})

	_, err := c.GenerateAssistantResponse(ctx, simpleRequest())
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
	if n := len(up.Requests()); n != 1 {
		t.Errorf("made %d requests, want 1 before the cancellation was noticed", n)
	}
}

func TestTransportFailureIsClassifiedAndRetried(t *testing.T) {
	// A closed server produces a connection refused on every attempt.
	up := newStubUpstream(t, stubResponse{Status: http.StatusOK})
	url := up.URL
	up.Close()

	auth := &fakeAuth{host: url, token: "t"}
	var slept []time.Duration
	c := NewClient(Options{
		Auth:                auth,
		HTTPClient:          &http.Client{Timeout: 2 * time.Second},
		StreamClientFactory: func() *http.Client { return &http.Client{Timeout: 2 * time.Second} },
		Sleep:               func(d time.Duration) { slept = append(slept, d) },
	})

	_, err := c.GenerateAssistantResponse(context.Background(), simpleRequest())
	if err == nil {
		t.Fatal("expected a transport error")
	}
	if !strings.Contains(err.Error(), "could not open a connection") &&
		!strings.Contains(err.Error(), "connection refused") {
		t.Errorf("error = %q, want an actionable connection message", err)
	}
	if len(slept) != maxAttempts-1 {
		t.Errorf("slept %v, want %d backoffs", slept, maxAttempts-1)
	}
}

func TestListAvailableModelsQueryAndParsing(t *testing.T) {
	up := newStubUpstream(t, stubResponse{Status: http.StatusOK, Body: `{
	  "models": [
	    {"modelId":"claude-opus-5","modelName":"Claude Opus 5","rateMultiplier":2.2,"rateUnit":"credit",
	     "tokenLimits":{"maxInputTokens":1000000,"maxOutputTokens":64000}}
	  ],
	  "defaultModel": {"modelId":"claude-sonnet-4.5"},
	  "nextToken": "page-2"
	}`})
	auth := &fakeAuth{profileARN: "arn:aws:codewhisperer:us-east-1:1:profile/ABC"}
	c, _ := newTestClient(t, up, auth)

	got, err := c.ListAvailableModels(context.Background(), "page-1")
	if err != nil {
		t.Fatal(err)
	}

	req := up.Requests()[0]
	if req.Method != http.MethodGet {
		t.Errorf("method = %q, want GET", req.Method)
	}
	if req.Path != listPath {
		t.Errorf("path = %q, want %q", req.Path, listPath)
	}
	for _, want := range []string{"origin=AI_EDITOR", "nextToken=page-1", "profileArn=arn"} {
		if !strings.Contains(req.Query, want) {
			t.Errorf("query %q should contain %q", req.Query, want)
		}
	}

	if len(got.Models) != 1 {
		t.Fatalf("parsed %d models, want 1", len(got.Models))
	}
	m := got.Models[0]
	if m.ModelID != "claude-opus-5" || m.ModelName != "Claude Opus 5" {
		t.Errorf("model = %+v", m)
	}
	if m.RateMultiplier != 2.2 || m.RateUnit != "credit" {
		t.Errorf("rate = %v %q", m.RateMultiplier, m.RateUnit)
	}
	if m.TokenLimits == nil || m.TokenLimits.MaxInputTokens != 1000000 || m.TokenLimits.MaxOutputTokens != 64000 {
		t.Errorf("tokenLimits = %+v", m.TokenLimits)
	}
	if got.DefaultModel == nil || got.DefaultModel.ModelID != "claude-sonnet-4.5" {
		t.Errorf("defaultModel = %+v", got.DefaultModel)
	}
	if got.NextToken != "page-2" {
		t.Errorf("nextToken = %q", got.NextToken)
	}
}

func TestListAvailableModelsOmitsProfileARNWhenEmpty(t *testing.T) {
	up := newStubUpstream(t, stubResponse{Status: http.StatusOK, Body: `{"models":[]}`})
	c, _ := newTestClient(t, up, &fakeAuth{})

	if _, err := c.ListAvailableModels(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	q := up.Requests()[0].Query
	if strings.Contains(q, "profileArn") {
		t.Errorf("query = %q, should omit an empty profileArn", q)
	}
	if strings.Contains(q, "nextToken") {
		t.Errorf("query = %q, should omit an empty nextToken", q)
	}
}

func TestListAvailableModelsErrorPaths(t *testing.T) {
	t.Run("non-200 becomes an APIError", func(t *testing.T) {
		up := newStubUpstream(t, stubResponse{
			Status: http.StatusBadRequest,
			Body:   `{"message":"Improperly formed request."}`,
			Header: map[string]string{"x-amzn-requestid": "rq-9"},
		})
		c, _ := newTestClient(t, up, &fakeAuth{})

		_, err := c.ListAvailableModels(context.Background(), "")
		var apiErr *APIError
		if !errors.As(err, &apiErr) {
			t.Fatalf("error = %T (%v), want *APIError", err, err)
		}
		if apiErr.StatusCode != http.StatusBadRequest {
			t.Errorf("StatusCode = %d", apiErr.StatusCode)
		}
		if apiErr.RequestID != "rq-9" {
			t.Errorf("RequestID = %q, want it captured from the header", apiErr.RequestID)
		}
	})

	t.Run("invalid JSON is reported", func(t *testing.T) {
		up := newStubUpstream(t, stubResponse{Status: http.StatusOK, Body: `<html>`})
		c, _ := newTestClient(t, up, &fakeAuth{})

		_, err := c.ListAvailableModels(context.Background(), "")
		if err == nil || !strings.Contains(err.Error(), "not valid JSON") {
			t.Errorf("error = %v, want a JSON complaint", err)
		}
	})
}

func TestBackoffDelay(t *testing.T) {
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}
	for i, w := range want {
		if got := backoffDelay(i); got != w {
			t.Errorf("backoffDelay(%d) = %v, want %v", i, got, w)
		}
	}
}

func TestDebugLoggingPrintsThePayloadAndRedactsTheToken(t *testing.T) {
	buf := &bytes.Buffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(previous)

	up := newStubUpstream(t, stubResponse{Status: http.StatusOK})
	c, _ := newTestClient(t, up, &fakeAuth{token: "SUPER-SECRET-TOKEN-VALUE"})

	c.LogRequestHeaders(true)
	resp, err := c.GenerateAssistantResponse(context.Background(), fullyPopulatedRequest())
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	logged := buf.String()
	if !strings.Contains(logged, "Kiro request payload") {
		t.Errorf("expected the payload to be logged at DEBUG, got:\n%s", logged)
	}
	if !strings.Contains(logged, "systemPrompt") {
		t.Error("the logged payload should be the real request body")
	}
	if !strings.Contains(logged, "Kiro request headers") {
		t.Error("expected the headers to be logged")
	}
	if strings.Contains(logged, "SUPER-SECRET-TOKEN-VALUE") {
		t.Errorf("the access token leaked into the log:\n%s", logged)
	}
}

func TestDebugLoggingIsSkippedWhenNotEnabled(t *testing.T) {
	buf := &bytes.Buffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	defer slog.SetDefault(previous)

	up := newStubUpstream(t, stubResponse{Status: http.StatusOK})
	c, _ := newTestClient(t, up, &fakeAuth{})
	c.LogRequestHeaders(false)
	resp, err := c.GenerateAssistantResponse(context.Background(), fullyPopulatedRequest())
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if strings.Contains(buf.String(), "Kiro request payload") {
		t.Error("the payload must not be logged above DEBUG: it contains the conversation")
	}
}

func TestReadErrorResponse(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusBadRequest,
		Body:       io.NopCloser(strings.NewReader(`{"message":"Input is too long.","reason":"CONTENT_LENGTH_EXCEEDS_THRESHOLD"}`)),
		Header:     http.Header{"X-Amzn-Requestid": []string{"rid-1"}},
	}
	apiErr := ReadErrorResponse(resp)
	if apiErr.Reason != ReasonContentLengthExceeded {
		t.Errorf("Reason = %q", apiErr.Reason)
	}
	if apiErr.Message != "Input is too long." {
		t.Errorf("Message = %q", apiErr.Message)
	}
	if apiErr.RequestID != "rid-1" {
		t.Errorf("RequestID = %q", apiErr.RequestID)
	}
}

func TestConcurrentRequestsShareThePooledClient(t *testing.T) {
	up := newStubUpstream(t, stubResponse{Status: http.StatusOK, Body: `{"models":[]}`})
	c, _ := newTestClient(t, up, &fakeAuth{})

	var wg sync.WaitGroup
	var failures atomic.Int64
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.ListAvailableModels(context.Background(), ""); err != nil {
				failures.Add(1)
			}
		}()
	}
	wg.Wait()

	if n := failures.Load(); n != 0 {
		t.Errorf("%d concurrent catalog calls failed", n)
	}
	if n := len(up.Requests()); n != 20 {
		t.Errorf("upstream saw %d requests, want 20", n)
	}
}

func TestGenerateAssistantResponseFillsAgentModeOnTheRequest(t *testing.T) {
	up := newStubUpstream(t, stubResponse{Status: http.StatusOK})
	c, _ := newTestClient(t, up, &fakeAuth{})

	req := simpleRequest()
	if req.AgentMode != "" {
		t.Fatal("fixture should start with no agent mode")
	}
	resp, err := c.GenerateAssistantResponse(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if req.AgentMode != "vibe" {
		t.Errorf("AgentMode = %q, want the client default applied", req.AgentMode)
	}
}

func TestUnencodableRequestFailsBeforeAnyNetworkCall(t *testing.T) {
	up := newStubUpstream(t, stubResponse{Status: http.StatusOK})
	c, _ := newTestClient(t, up, &fakeAuth{})

	req := simpleRequest()
	req.AdditionalModelRequestFields = map[string]any{"bad": make(chan int)}

	if _, err := c.GenerateAssistantResponse(context.Background(), req); err == nil {
		t.Fatal("expected an encode failure")
	}
	if n := len(up.Requests()); n != 0 {
		t.Errorf("made %d requests, want none", n)
	}
}

func TestBufferedRetryResponseIsReadableAfterConnectionRelease(t *testing.T) {
	body := `{"message":"still readable","reason":"X"}`
	up := newStubUpstream(t, stubResponse{Status: 503, Body: body})
	c, _ := newTestClient(t, up, &fakeAuth{})

	resp, err := c.GenerateAssistantResponse(context.Background(), simpleRequest())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("buffered body should still be readable: %v", err)
	}
	if string(got) != body {
		t.Errorf("body = %q, want %q", got, body)
	}
	if resp.ContentLength != int64(len(body)) {
		t.Errorf("ContentLength = %d, want %d", resp.ContentLength, len(body))
	}

	var parsed map[string]any
	if err := json.Unmarshal(got, &parsed); err != nil {
		t.Errorf("buffered body is not valid JSON: %v", err)
	}
}

// Copyright (c) 2026 Jasmin (https://github.com/jasminnanda)
// Licensed under the MIT License. See LICENSE in the project root.
