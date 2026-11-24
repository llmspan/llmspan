//go:build loadtest

package load_test

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/llmspan/llmspan/internal/config"
	"github.com/llmspan/llmspan/internal/provider"
	"github.com/llmspan/llmspan/internal/proxy"
)

// TestLoadConcurrentStreamsNoLeak holds 120 SSE streams open at once (a
// barrier in the upstream guarantees overlap), each carrying 2000 small
// chunks, and asserts the goroutine count returns to baseline afterwards.
func TestLoadConcurrentStreamsNoLeak(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in -short mode")
	}
	const streams, chunks = 120, 2000
	const chunk = "data: {\"choices\":[{\"delta\":{\"content\":\"t\"}}]}\n\n"

	release := make(chan struct{})
	var arrived sync.WaitGroup
	arrived.Add(streams)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		_, _ = io.WriteString(w, chunk)
		fl.Flush()
		arrived.Done()
		<-release
		for i := 1; i < chunks; i++ {
			_, _ = io.WriteString(w, chunk)
			if i%40 == 0 {
				fl.Flush()
			}
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.Upstreams = []config.Upstream{{Name: "openai", Prefix: "/v1", Target: upstream.URL, Provider: "openai"}}
	h, err := proxy.New(cfg, provider.BuiltIn(), fakeProv(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	defer ts.Close()

	runtime.GC()
	time.Sleep(100 * time.Millisecond)
	base := runtime.NumGoroutine()

	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: streams}}
	errs := make(chan error, streams)
	var wg sync.WaitGroup
	for i := 0; i < streams; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := client.Post(ts.URL+"/v1/chat/completions", "application/json",
				strings.NewReader(`{"model":"gpt-4o-mini","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
			if err != nil {
				errs <- err
				return
			}
			n, err := io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if err == nil && n < int64(chunks*len(chunk)) {
				err = fmt.Errorf("short stream: %d bytes", n)
			}
			if err != nil {
				errs <- err
			}
		}()
	}
	ready := make(chan struct{})
	go func() { arrived.Wait(); close(ready) }()
	select {
	case <-ready:
	case <-time.After(30 * time.Second):
		close(release)
		t.Fatal("streams never became concurrently active")
	}
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("stream: %v", err)
	}

	client.CloseIdleConnections()
	// Drop the proxy's pooled keep-alive connections to the upstream.
	upstream.CloseClientConnections()
	var after int
	for i := 0; i < 100; i++ {
		after = runtime.NumGoroutine()
		if after <= base+2 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("goroutine leak: baseline=%d after=%d", base, after)
}
