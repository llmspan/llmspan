package proxy_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/llmspan/llmspan/internal/config"
	"github.com/llmspan/llmspan/internal/provider"
	"github.com/llmspan/llmspan/internal/proxy"
	"github.com/llmspan/llmspan/internal/telemetry"
)

const benchReqBody = `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hello"}]}`

const benchRespBody = `{"id":"chatcmpl-1","object":"chat.completion","model":"gpt-4o-mini","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hi there, this is a short stub completion"}}],"usage":{"prompt_tokens":11,"completion_tokens":9,"total_tokens":20}}`

func benchProviders() telemetry.Providers {
	tp := sdktrace.NewTracerProvider() // no exporter: spans are created and dropped
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(sdkmetric.NewManualReader()))
	meter := mp.Meter("bench")
	meters, err := telemetry.NewGenAIMeters(meter)
	if err != nil {
		panic(err)
	}
	return telemetry.Providers{
		Tracer:   tp.Tracer("bench"),
		Meter:    meter,
		Meters:   meters,
		Shutdown: func(context.Context) error { return mp.Shutdown(context.Background()) },
	}
}

func benchProxy(tb testing.TB, upstreamURL string) *httptest.Server {
	tb.Helper()
	cfg := config.Default()
	cfg.Upstreams = []config.Upstream{{Name: "openai", Prefix: "/v1", Target: upstreamURL, Provider: "openai"}}
	h, err := proxy.New(cfg, provider.BuiltIn(), benchProviders(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		tb.Fatal(err)
	}
	ts := httptest.NewServer(h)
	tb.Cleanup(ts.Close)
	return ts
}

func benchClient() *http.Client {
	return &http.Client{Transport: &http.Transport{
		MaxIdleConns: 256, MaxIdleConnsPerHost: 256, MaxConnsPerHost: 0,
		IdleConnTimeout: 30 * time.Second,
	}}
}

func benchJSONUpstream(tb testing.TB) *httptest.Server {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, benchRespBody)
	}))
	tb.Cleanup(up.Close)
	return up
}

func doJSON(c *http.Client, base string) error {
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/chat/completions", strings.NewReader(benchReqBody))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.Body.Close()
}

func benchJSON(b *testing.B, via bool) {
	up := benchJSONUpstream(b)
	base := up.URL
	if via {
		base = benchProxy(b, up.URL).URL
	}
	c := benchClient()
	for i := 0; i < 50; i++ { // warm connections
		_ = doJSON(c, base)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := doJSON(c, base); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkJSONDirect / BenchmarkJSONViaProxy: sequential non-streaming
// completion round trip. B/op and allocs/op include the in-process stub
// upstream and client, so compare the two, do not read either in isolation.
func BenchmarkJSONDirect(b *testing.B)   { benchJSON(b, false) }
func BenchmarkJSONViaProxy(b *testing.B) { benchJSON(b, true) }

func BenchmarkJSONViaProxyParallel(b *testing.B) {
	up := benchJSONUpstream(b)
	base := benchProxy(b, up.URL).URL
	c := benchClient()
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if err := doJSON(c, base); err != nil {
				b.Error(err)
				return
			}
		}
	})
}

func pct(d []time.Duration, p float64) time.Duration {
	return d[int(float64(len(d)-1)*p)]
}

func measureLatencies(t *testing.T, base string, n int) []time.Duration {
	c := benchClient()
	for i := 0; i < 100; i++ {
		_ = doJSON(c, base)
	}
	out := make([]time.Duration, n)
	for i := range out {
		s := time.Now()
		if err := doJSON(c, base); err != nil {
			t.Fatal(err)
		}
		out[i] = time.Since(s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// TestBenchAddedLatency reports added latency (proxy minus direct) at
// p50/p99 over N sequential requests against a zero-latency stub upstream.
// Run with: BENCH_N=5000 go test ./internal/proxy -run TestBenchAddedLatency -v
func TestBenchAddedLatency(t *testing.T) {
	if testing.Short() {
		t.Skip("benchmark-style test")
	}
	n := benchEnvInt("BENCH_N", 3000)
	up := benchJSONUpstream(t)
	direct := measureLatencies(t, up.URL, n)
	via := measureLatencies(t, benchProxy(t, up.URL).URL, n)
	t.Logf("N=%d direct p50=%v p99=%v | via-proxy p50=%v p99=%v | added p50=%v p99=%v",
		n, pct(direct, .5), pct(direct, .99), pct(via, .5), pct(via, .99),
		pct(via, .5)-pct(direct, .5), pct(via, .99)-pct(direct, .99))
	t.Logf("throughput (sequential, 1 client): direct=%.0f req/s via-proxy=%.0f req/s",
		float64(n)/sum(direct).Seconds(), float64(n)/sum(via).Seconds())
	// Loose sanity bound only: the number is the point, not the gate.
	if pct(via, .5)-pct(direct, .5) > 5*time.Millisecond {
		t.Errorf("added p50 latency %v exceeds 5ms sanity bound", pct(via, .5)-pct(direct, .5))
	}
}

func sum(d []time.Duration) time.Duration {
	var s time.Duration
	for _, x := range d {
		s += x
	}
	return s
}

func benchEnvInt(k string, def int) int {
	var v int
	if _, err := fmt.Sscanf(os.Getenv(k), "%d", &v); err != nil || v <= 0 {
		return def
	}
	return v
}

// timedStreamUpstream emits `chunks` SSE events separated by `gap`.
func timedStreamUpstream(tb testing.TB, chunks int, gap time.Duration) *httptest.Server {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for i := 0; i < chunks; i++ {
			if i > 0 {
				time.Sleep(gap)
			}
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"c%d\"}}]}\n\n", i)
			fl.Flush()
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	tb.Cleanup(up.Close)
	return up
}

type streamTimes struct{ first, second, total time.Duration }

func streamOnce(t *testing.T, c *http.Client, base string) streamTimes {
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o-mini","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	start := time.Now()
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var st streamTimes
	buf := make([]byte, 4096)
	seen := 0
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			seen += bytes.Count(buf[:n], []byte("\n\n"))
			if st.first == 0 {
				st.first = time.Since(start)
			}
			if seen >= 2 && st.second == 0 {
				st.second = time.Since(start)
			}
		}
		if err != nil {
			break
		}
	}
	st.total = time.Since(start)
	return st
}

// TestBenchStreamingTTFT: upstream emits chunk, sleeps 50ms, emits chunk...
// Reports time-to-first-chunk and inter-chunk timing direct vs via proxy,
// and asserts the proxy does not buffer (first chunk well before 50ms).
func TestBenchStreamingTTFT(t *testing.T) {
	if testing.Short() {
		t.Skip("benchmark-style test")
	}
	const gap = 50 * time.Millisecond
	up := timedStreamUpstream(t, 4, gap)
	px := benchProxy(t, up.URL)
	c := benchClient()
	const reps = 30
	var dFirst, pFirst, dSecond, pSecond []time.Duration
	for i := 0; i < reps+3; i++ {
		d := streamOnce(t, c, up.URL)
		p := streamOnce(t, c, px.URL)
		if i < 3 {
			continue
		}
		dFirst, pFirst = append(dFirst, d.first), append(pFirst, p.first)
		dSecond, pSecond = append(dSecond, d.second), append(pSecond, p.second)
	}
	for _, s := range [][]time.Duration{dFirst, pFirst, dSecond, pSecond} {
		sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	}
	t.Logf("TTFT direct p50=%v p99=%v | via-proxy p50=%v p99=%v | delta p50=%v",
		pct(dFirst, .5), pct(dFirst, .99), pct(pFirst, .5), pct(pFirst, .99), pct(pFirst, .5)-pct(dFirst, .5))
	t.Logf("second-chunk arrival direct p50=%v via-proxy p50=%v delta=%v (upstream gap %v)",
		pct(dSecond, .5), pct(pSecond, .5), pct(pSecond, .5)-pct(dSecond, .5), gap)
	if pct(pFirst, .5) > gap/2 {
		t.Errorf("proxy TTFT p50 %v >= half the upstream gap: proxy appears to buffer", pct(pFirst, .5))
	}
}

// TestBenchConcurrentStreams opens many truly concurrent SSE streams, each
// with thousands of small chunks, holds them open simultaneously via a
// start barrier, then verifies no goroutine leak and reports memory/stream.
// Tunables: BENCH_STREAMS (default 200), BENCH_CHUNKS (default 3000).
func TestBenchConcurrentStreams(t *testing.T) {
	if testing.Short() {
		t.Skip("benchmark-style test")
	}
	streams := benchEnvInt("BENCH_STREAMS", 200)
	chunks := benchEnvInt("BENCH_CHUNKS", 3000)

	var inflight, peak int64
	var mu sync.Mutex
	release := make(chan struct{})
	var arrived sync.WaitGroup
	arrived.Add(streams)
	chunk := "data: {\"choices\":[{\"delta\":{\"content\":\"tok\"}}]}\n\n"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		_, _ = io.WriteString(w, chunk)
		fl.Flush()
		mu.Lock()
		inflight++
		if inflight > peak {
			peak = inflight
		}
		mu.Unlock()
		arrived.Done()
		<-release // all streams are now simultaneously open
		for i := 1; i < chunks; i++ {
			_, _ = io.WriteString(w, chunk)
			if i%50 == 0 {
				fl.Flush()
			}
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		fl.Flush()
		mu.Lock()
		inflight--
		mu.Unlock()
	}))
	defer up.Close()
	px := benchProxy(t, up.URL)

	runtime.GC()
	time.Sleep(100 * time.Millisecond)
	baseG := runtime.NumGoroutine()
	var msBase runtime.MemStats
	runtime.ReadMemStats(&msBase)

	c := benchClient()
	var wg sync.WaitGroup
	var errMu sync.Mutex
	var firstErr error
	for i := 0; i < streams; i++ {
		if i%25 == 24 {
			// Stagger dial bursts: a 200-way SYN burst can overflow the
			// loopback listen backlog and stall a few streams on SYN
			// retransmit, which would measure the kernel, not llmspan.
			time.Sleep(5 * time.Millisecond)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest(http.MethodPost, px.URL+"/v1/chat/completions",
				strings.NewReader(`{"model":"gpt-4o-mini","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
			req.Header.Set("Content-Type", "application/json")
			resp, err := c.Do(req)
			if err == nil {
				var n int64
				n, err = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if err == nil && n < int64(chunks)*int64(len(chunk)) {
					err = fmt.Errorf("short stream: %d bytes", n)
				}
			}
			if err != nil {
				errMu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				errMu.Unlock()
			}
		}()
	}

	done := make(chan struct{})
	go func() { arrived.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		close(release)
		t.Fatalf("only %d/%d streams became concurrently active", peak, streams)
	}
	var ms runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&ms)
	heapOpen := ms.HeapAlloc
	sysOpen := ms.Sys
	gOpen := runtime.NumGoroutine()

	start := time.Now()
	close(release)
	wg.Wait()
	elapsed := time.Since(start)
	if firstErr != nil {
		t.Fatalf("stream error: %v", firstErr)
	}

	c.CloseIdleConnections()
	// The proxy keeps pooled keep-alive connections to the upstream (by
	// design); close them so the leak check measures only leaked work.
	if h, ok := px.Config.Handler.(*proxy.Handler); ok {
		for _, rp := range proxy.ReverseProxiesForTest(h) {
			if ci, ok := rp.Transport.(interface{ CloseIdleConnections() }); ok {
				ci.CloseIdleConnections()
			}
		}
	}
	var gAfter int
	for i := 0; i < 100; i++ {
		runtime.GC()
		gAfter = runtime.NumGoroutine()
		if gAfter <= baseG+2 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	runtime.ReadMemStats(&ms)
	t.Logf("streams=%d chunks/stream=%d peak_concurrent=%d total_chunks=%d drain_time=%v (%.0f chunks/s aggregate)",
		streams, chunks, peak, streams*chunks, elapsed, float64(streams*chunks)/elapsed.Seconds())
	t.Logf("goroutines baseline=%d open=%d after=%d", baseG, gOpen, gAfter)
	t.Logf("heap baseline=%d KiB, while open=%d KiB, after drain=%d KiB; sys while open=%d KiB",
		msBase.HeapAlloc/1024, heapOpen/1024, ms.HeapAlloc/1024, sysOpen/1024)
	t.Logf("per stream (proxy + in-process client + stub upstream, all in this process): heap=%d KiB goroutines=%.1f",
		(int64(heapOpen)-int64(msBase.HeapAlloc))/int64(streams)/1024, float64(gOpen-baseG)/float64(streams))
	if peak < int64(streams) {
		t.Errorf("peak concurrency %d < %d", peak, streams)
	}
	if gAfter > baseG+2 && os.Getenv("BENCH_DUMP") != "" {
		buf := make([]byte, 1<<20)
		t.Logf("%s", buf[:runtime.Stack(buf, true)])
	}
	if gAfter > baseG+2 {
		t.Errorf("goroutine leak: baseline=%d after=%d", baseG, gAfter)
	}
}
