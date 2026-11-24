package proxy_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/llmspan/llmspan/internal/auth"
	"github.com/llmspan/llmspan/internal/config"
	"github.com/llmspan/llmspan/internal/provider"
	"github.com/llmspan/llmspan/internal/proxy"
)

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func postChat(t *testing.T, base string, hdr map[string]string) *http.Response {
	t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, base+"/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp
}

// A breaker must ignore 429 and 529 (default) and keep counting 500s.
func TestBreakerIgnoresRateLimitAndOverloaded(t *testing.T) {
	t.Parallel()
	var status atomic.Int32
	status.Store(429)
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(int(status.Load()))
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.Upstreams = []config.Upstream{{
		Name: "openai", Prefix: "/v1", Target: upstream.URL, Provider: "openai",
		Breaker: config.BreakerConfig{Failures: 2, RecoveryWindow: time.Minute},
	}}
	h, err := proxy.New(cfg, provider.BuiltIn(), fakeProv(t), quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	defer ts.Close()

	for _, st := range []int32{429, 529, 429, 529, 400} {
		status.Store(st)
		if resp := postChat(t, ts.URL, nil); resp.StatusCode != int(st) {
			t.Fatalf("status %d: breaker interfered, got %d", st, resp.StatusCode)
		}
	}
	// A 500 between ignorable statuses must not trip on its own.
	status.Store(500)
	postChat(t, ts.URL, nil)
	status.Store(429)
	postChat(t, ts.URL, nil) // 429 is "success": resets the run
	status.Store(500)
	postChat(t, ts.URL, nil)
	postChat(t, ts.URL, nil) // second consecutive 500: trips the breaker
	before := hits.Load()
	if resp := postChat(t, ts.URL, nil); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("breaker should be open after 2 consecutive 500s, got %d", resp.StatusCode)
	}
	if hits.Load() != before {
		t.Error("open breaker must not reach upstream")
	}
}

func TestBreakerTripOnOverloadedOptIn(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(529)
	}))
	defer upstream.Close()
	cfg := config.Default()
	cfg.Upstreams = []config.Upstream{{
		Name: "openai", Prefix: "/v1", Target: upstream.URL, Provider: "openai",
		Breaker: config.BreakerConfig{Failures: 2, RecoveryWindow: time.Minute, TripOnOverloaded: true},
	}}
	h, err := proxy.New(cfg, provider.BuiltIn(), fakeProv(t), quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	defer ts.Close()
	postChat(t, ts.URL, nil)
	postChat(t, ts.URL, nil)
	if resp := postChat(t, ts.URL, nil); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("trip_on_overloaded should open the breaker, got %d", resp.StatusCode)
	}
}

// With max_buffered_bodies=1, a second request arriving while the first
// still holds its buffer is refused with 429 and never reaches upstream.
func TestGlobalBodyBufferCap(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	entered := make(chan struct{}, 4)
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		entered <- struct{}{}
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","model":"gpt-4o-mini","choices":[],"usage":{}}`)
	}))
	defer upstream.Close()
	cfg := config.Default()
	cfg.Request.MaxBufferedBodies = 1
	cfg.Upstreams = []config.Upstream{{Name: "openai", Prefix: "/v1", Target: upstream.URL, Provider: "openai"}}
	h, err := proxy.New(cfg, provider.BuiltIn(), fakeProv(t), quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	defer ts.Close()

	done := make(chan int, 1)
	go func() { done <- postChat(t, ts.URL, nil).StatusCode }()
	<-entered
	resp := postChat(t, ts.URL, nil)
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("second request: status=%d retry-after=%q, want 429 + Retry-After", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	close(release)
	if st := <-done; st != 200 {
		t.Fatalf("first request status = %d", st)
	}
	if hits.Load() != 1 {
		t.Errorf("upstream hits = %d, want 1", hits.Load())
	}
	// Slot released: next request succeeds.
	if resp := postChat(t, ts.URL, nil); resp.StatusCode != 200 {
		t.Errorf("after release status = %d", resp.StatusCode)
	}
}

// A named token is the caller identity and the X-Caller header cannot
// override it; the label shows up on the span.
func TestNamedTokenIsCallerAndNotSpoofable(t *testing.T) {
	t.Parallel()
	hash, _ := auth.Hash("secret-billing")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Caller") != "" {
			t.Errorf("X-Caller leaked upstream")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","model":"gpt-4o-mini","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer upstream.Close()
	cfg := config.Default()
	cfg.Attribution.Header = "X-Caller"
	cfg.Auth.Tokens = []string{"billing:" + hash}
	cfg.Upstreams = []config.Upstream{{Name: "openai", Prefix: "/v1", Target: upstream.URL, Provider: "openai"}}
	prov, rec, _ := realMeterProviders(t)
	h, err := proxy.New(cfg, provider.BuiltIn(), prov, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	defer ts.Close()

	resp := postChat(t, ts.URL, map[string]string{"X-LLMSPAN-Token": "secret-billing", "X-Caller": "mallory"})
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var caller string
	for _, s := range rec.Ended() {
		for _, a := range s.Attributes() {
			if strings.HasSuffix(string(a.Key), "caller") {
				caller = a.Value.AsString()
			}
		}
	}
	if caller != "billing" {
		t.Errorf("caller attribute = %q, want billing", caller)
	}
}

// DefaultResponseHeaderTimeout is applied when unset and the configured
// value reaches the transport.
func TestResponseHeaderTimeoutConfigured(t *testing.T) {
	t.Parallel()
	cfg := config.Default()
	cfg.Upstreams = []config.Upstream{
		{Name: "a", Prefix: "/a", Target: "http://127.0.0.1:1", Provider: "openai"},
		{Name: "b", Prefix: "/b", Target: "http://127.0.0.1:1", Provider: "openai", ResponseHeaderTimeout: 17 * time.Minute},
	}
	h, err := proxy.New(cfg, provider.BuiltIn(), fakeProv(t), quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]time.Duration{"a": config.DefaultResponseHeaderTimeout, "b": 17 * time.Minute} {
		tr := proxy.ReverseProxiesForTest(h)[name].Transport.(*http.Transport)
		if tr.ResponseHeaderTimeout != want {
			t.Errorf("%s: ResponseHeaderTimeout = %v, want %v", name, tr.ResponseHeaderTimeout, want)
		}
		if !tr.DisableCompression {
			t.Errorf("%s: DisableCompression must be true (byte-for-byte passthrough)", name)
		}
	}
}

// A slow-header upstream is cut off at the configured timeout.
func TestResponseHeaderTimeoutEnforced(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(3 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer upstream.Close()
	cfg := config.Default()
	cfg.Upstreams = []config.Upstream{{Name: "openai", Prefix: "/v1", Target: upstream.URL, Provider: "openai", ResponseHeaderTimeout: 100 * time.Millisecond}}
	h, err := proxy.New(cfg, provider.BuiltIn(), fakeProv(t), quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	defer ts.Close()
	start := time.Now()
	resp := postChat(t, ts.URL, nil)
	if resp.StatusCode != http.StatusBadGateway || time.Since(start) > 2*time.Second {
		t.Fatalf("status=%d after %v, want 502 within timeout", resp.StatusCode, time.Since(start))
	}
}

func TestMetricsHandlerExposesSelfMetrics(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","model":"gpt-4o-mini","choices":[],"usage":{}}`)
	}))
	defer upstream.Close()
	cfg := config.Default()
	cfg.Upstreams = []config.Upstream{{
		Name: "openai", Prefix: "/v1", Target: upstream.URL, Provider: "openai",
		Breaker: config.BreakerConfig{Failures: 3},
	}}
	prov, _, _ := realMeterProviders(t)
	h, err := proxy.New(cfg, provider.BuiltIn(), prov, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	defer ts.Close()
	postChat(t, ts.URL, nil)

	// recordRequest runs in a defer after the response is written, so
	// the client can observe the reply a moment before the counter moves.
	var rec *httptest.ResponseRecorder
	var body string
	for i := 0; i < 100; i++ {
		rec = httptest.NewRecorder()
		h.MetricsHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		body = rec.Body.String()
		if strings.Contains(body, `llmspan_requests_total{http_response_status_code="200",upstream="openai"} 1`) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain; version=0.0.4") {
		t.Errorf("content-type = %q", ct)
	}
	for _, want := range []string{
		"# TYPE llmspan_requests_total counter",
		`llmspan_requests_total{http_response_status_code="200",upstream="openai"} 1`,
		"# TYPE llmspan_breaker_state gauge",
		`llmspan_breaker_state{upstream="openai"} 0`,
		"# TYPE llmspan_requests_in_flight gauge",
		`llmspan_requests_in_flight{upstream="openai"} 0`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q in:\n%s", want, body)
		}
	}
	if strings.Contains(body, "gen_ai") {
		t.Error("GenAI metrics must not be exposed on the admin endpoint")
	}
	rec = httptest.NewRecorder()
	h.MetricsHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/metrics", bytes.NewReader(nil)))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /metrics = %d", rec.Code)
	}
}

// Documents the mid-stream upstream failure contract: once response
// headers (200) are on the wire the status cannot change, so the proxy
// ABORTS the client connection (httputil panics http.ErrAbortHandler) and
// the client sees a truncated body / read error rather than a clean EOF.
// The breaker is fed the already-sent 200, so mid-stream failures never
// trip it; ActiveStreams still drains to zero.
func TestMidStreamUpstreamAbortTruncatesClient(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n")
		w.(http.Flusher).Flush()
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close() // abrupt: no chunk terminator
		}
	}))
	defer upstream.Close()
	cfg := config.Default()
	cfg.Upstreams = []config.Upstream{{
		Name: "openai", Prefix: "/v1", Target: upstream.URL, Provider: "openai",
		Breaker: config.BreakerConfig{Failures: 1, RecoveryWindow: time.Minute},
	}}
	h, err := proxy.New(cfg, provider.BuiltIn(), fakeProv(t), quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	defer ts.Close()

	for i := 0; i < 3; i++ {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, ts.URL+"/v1/chat/completions",
			strings.NewReader(`{"model":"gpt-4o-mini","stream":true,"messages":[]}`))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != 200 {
			t.Fatalf("req %d: status %d (breaker tripped by mid-stream failure?)", i, resp.StatusCode)
		}
		_, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr == nil {
			t.Fatalf("req %d: client saw a clean EOF; want a truncation error", i)
		}
	}
	deadline := time.Now().Add(3 * time.Second)
	for proxy.ActiveStreamsForTest(h) != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := proxy.ActiveStreamsForTest(h); n != 0 {
		t.Errorf("active streams = %d, want 0", n)
	}
}
