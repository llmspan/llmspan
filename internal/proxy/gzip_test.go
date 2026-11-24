package proxy_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/llmspan/llmspan/internal/config"
	"github.com/llmspan/llmspan/internal/provider"
	"github.com/llmspan/llmspan/internal/proxy"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// TestProxyGzippedResponseStillCountsTokens is a regression test.
//
// When a client sets Accept-Encoding: gzip, http.Transport's
// transparent decompression backs off — it only decodes responses it
// asked for itself. The proxy then sees gzip-encoded bytes in modify,
// json.Unmarshal fails silently, and every recorded span shows
// input_tokens = 0, output_tokens = 0, cost = $0. A FinOps disaster
// dressed up as a parser quirk.
//
// With the fix (Accept-Encoding stripped at the proxy boundary), the
// Transport requests + decompresses gzip on llmspan's behalf, and
// parseResponseJSON sees plaintext.
func TestProxyGzippedResponseStillCountsTokens(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Always serve gzipped JSON, regardless of Accept-Encoding,
		// to keep the test deterministic. Production upstreams behave
		// similarly for clients that requested compression.
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		_, _ = io.WriteString(gz, `{
			"id":"chatcmpl-test",
			"model":"gpt-4o-mini-2024-07-18",
			"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}],
			"usage":{"prompt_tokens":11,"completion_tokens":2,"total_tokens":13}
		}`)
		_ = gz.Close()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Length", "")
		_, _ = w.Write(buf.Bytes())
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.Upstreams = []config.Upstream{{
		Name: "openai", Prefix: "/v1", Target: upstream.URL, Provider: "openai",
	}}
	prov, rec, _ := realMeterProviders(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h, err := proxy.New(cfg, provider.BuiltIn(), prov, logger)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	defer ts.Close()

	reqBody, _ := json.Marshal(map[string]any{
		"model":    "gpt-4o-mini",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	req, err := http.NewRequestWithContext(context.Background(),
		http.MethodPost, ts.URL+"/v1/chat/completions", bytes.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	// Force the failure mode: explicit Accept-Encoding suppresses the
	// transparent gzip decoding that Transport would otherwise inject.
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("Content-Type", "application/json")

	// Use a custom Transport with DisableCompression=true so the test
	// client itself doesn't decode anything we read for assertions.
	client := &http.Client{
		Transport: &http.Transport{DisableCompression: true},
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body = %s", resp.StatusCode, body)
	}

	// Span must carry parsed token usage. Without the fix, both are 0.
	var inputTokens int64
	for _, s := range rec.Ended() {
		for _, a := range s.Attributes() {
			if string(a.Key) == "gen_ai.usage.input_tokens" {
				inputTokens = a.Value.AsInt64()
			}
		}
	}
	if inputTokens <= 0 {
		// Dump for diagnosis on failure.
		for _, s := range rec.Ended() {
			t.Logf("span %q attrs:", s.Name())
			for _, a := range s.Attributes() {
				t.Logf("  %s = %s", a.Key, a.Value.String())
			}
		}
		t.Fatalf("gen_ai.usage.input_tokens = %d, want > 0 (gzip response was not decoded for parsing)", inputTokens)
	}
}

// TestProxyNarrowsAcceptEncodingToDecodable asserts the outbound
// Accept-Encoding keeps only encodings llmspan can decode for telemetry
// (so the upstream never answers with br/zstd it cannot parse) while
// leaving a client that offered nothing decodable untouched.
func TestProxyNarrowsAcceptEncodingToDecodable(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ in, want string }{
		{"gzip, br", "gzip"},
		{"br;q=1.0, gzip;q=0.8, zstd", "gzip;q=0.8"},
		{"br", "br"},
		{"*", "gzip"},
		{"", ""},
	} {
		gotAccept := make(chan string, 1)
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotAccept <- r.Header.Get("Accept-Encoding")
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"x","model":"gpt-4o-mini","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
		}))
		cfg := config.Default()
		cfg.Upstreams = []config.Upstream{{Name: "openai", Prefix: "/v1", Target: upstream.URL, Provider: "openai"}}
		h, err := proxy.New(cfg, provider.BuiltIn(), fakeProv(t), slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err != nil {
			t.Fatal(err)
		}
		ts := httptest.NewServer(h)
		req, _ := http.NewRequestWithContext(context.Background(),
			http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o-mini","messages":[]}`))
		if tc.in != "" {
			req.Header.Set("Accept-Encoding", tc.in)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := (&http.Client{Transport: &http.Transport{DisableCompression: true}}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if got := <-gotAccept; got != tc.want {
			t.Errorf("client Accept-Encoding %q: upstream saw %q, want %q", tc.in, got, tc.want)
		}
		ts.Close()
		upstream.Close()
	}
}

func gzipBytes(t *testing.T, plain string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, _ = io.WriteString(gz, plain)
	_ = gz.Close()
	return buf.Bytes()
}

func inputTokensOf(rec interface {
	Ended() []sdktrace.ReadOnlySpan
}) int64 {
	var n int64
	for _, s := range rec.Ended() {
		for _, a := range s.Attributes() {
			if string(a.Key) == "gen_ai.usage.input_tokens" {
				n = a.Value.AsInt64()
			}
		}
	}
	return n
}

// TestProxyGzipPassthroughIsByteIdentical: the client must receive the
// upstream's compressed bytes, Content-Encoding and Content-Length
// verbatim (no decode + re-encode), AND telemetry must still read the
// plaintext usage.
func TestProxyGzipPassthroughIsByteIdentical(t *testing.T) {
	t.Parallel()

	compressed := gzipBytes(t, `{"id":"c","model":"gpt-4o-mini-2024-07-18","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":17,"completion_tokens":2,"total_tokens":19}}`)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept-Encoding") != "gzip" {
			t.Errorf("upstream Accept-Encoding = %q", r.Header.Get("Accept-Encoding"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Length", strconv.Itoa(len(compressed)))
		_, _ = w.Write(compressed)
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.Upstreams = []config.Upstream{{Name: "openai", Prefix: "/v1", Target: upstream.URL, Provider: "openai"}}
	prov, rec, _ := realMeterProviders(t)
	h, err := proxy.New(cfg, provider.BuiltIn(), prov, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	defer ts.Close()

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, ts.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Transport: &http.Transport{DisableCompression: true}}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.Header.Get("Content-Encoding") != "gzip" {
		t.Errorf("Content-Encoding = %q, want gzip", resp.Header.Get("Content-Encoding"))
	}
	if !bytes.Equal(got, compressed) {
		t.Fatalf("client bytes differ from upstream's compressed bytes (%d vs %d)", len(got), len(compressed))
	}
	if resp.Header.Get("Content-Length") != strconv.Itoa(len(compressed)) {
		t.Errorf("Content-Length = %q", resp.Header.Get("Content-Length"))
	}
	if n := inputTokensOf(rec); n != 17 {
		t.Errorf("input_tokens = %d, want 17 (telemetry must still decode the tee)", n)
	}
}

// TestProxyGzipSSEPassthrough streams a gzip-encoded SSE body in
// flushed chunks; bytes must match and usage must still be parsed.
func TestProxyGzipSSEPassthrough(t *testing.T) {
	t.Parallel()

	events := "data: {\"id\":\"1\",\"model\":\"gpt-4o-mini\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"id\":\"1\",\"model\":\"gpt-4o-mini\",\"choices\":[],\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":3,\"total_tokens\":12}}\n\n" +
		"data: [DONE]\n\n"
	var wire bytes.Buffer
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Encoding", "gzip")
		var buf bytes.Buffer
		gz := gzip.NewWriter(io.MultiWriter(w, &buf))
		for _, chunk := range strings.SplitAfter(events, "\n\n") {
			_, _ = io.WriteString(gz, chunk)
			_ = gz.Flush()
			w.(http.Flusher).Flush()
		}
		_ = gz.Close()
		wire = buf
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.Upstreams = []config.Upstream{{Name: "openai", Prefix: "/v1", Target: upstream.URL, Provider: "openai"}}
	prov, rec, _ := realMeterProviders(t)
	h, err := proxy.New(cfg, provider.BuiltIn(), prov, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	defer ts.Close()

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, ts.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o-mini","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Transport: &http.Transport{DisableCompression: true}}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !bytes.Equal(got, wire.Bytes()) {
		t.Fatalf("client SSE bytes differ from upstream wire bytes (%d vs %d)", len(got), wire.Len())
	}
	if n := inputTokensOf(rec); n != 9 {
		t.Errorf("input_tokens = %d, want 9", n)
	}
}

// TestProxyCorruptGzipStillRelaysBytes: a broken gzip stream must not
// hang or alter the client's bytes; telemetry just gives up.
func TestProxyCorruptGzipStillRelaysBytes(t *testing.T) {
	t.Parallel()
	junk := bytes.Repeat([]byte("not gzip at all "), 4096)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(junk)
	}))
	defer upstream.Close()
	cfg := config.Default()
	cfg.Upstreams = []config.Upstream{{Name: "openai", Prefix: "/v1", Target: upstream.URL, Provider: "openai"}}
	h, err := proxy.New(cfg, provider.BuiltIn(), fakeProv(t), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	defer ts.Close()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, ts.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o-mini","messages":[]}`))
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := (&http.Client{Transport: &http.Transport{DisableCompression: true}, Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !bytes.Equal(got, junk) {
		t.Fatalf("bytes altered: got %d want %d", len(got), len(junk))
	}
}

// TestProxyUnsupportedEncodingRelayedUntouched: br is relayed verbatim.
func TestProxyUnsupportedEncodingRelayedUntouched(t *testing.T) {
	t.Parallel()
	payload := []byte{0x1b, 0x2a, 0x00, 0xff, 0x10}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "br")
		_, _ = w.Write(payload)
	}))
	defer upstream.Close()
	cfg := config.Default()
	cfg.Upstreams = []config.Upstream{{Name: "openai", Prefix: "/v1", Target: upstream.URL, Provider: "openai"}}
	h, err := proxy.New(cfg, provider.BuiltIn(), fakeProv(t), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	defer ts.Close()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, ts.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o-mini","messages":[]}`))
	req.Header.Set("Accept-Encoding", "br")
	resp, err := (&http.Client{Transport: &http.Transport{DisableCompression: true}}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.Header.Get("Content-Encoding") != "br" || !bytes.Equal(got, payload) {
		t.Fatalf("br response altered: enc=%q body=%v", resp.Header.Get("Content-Encoding"), got)
	}
}
