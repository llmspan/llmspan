// Command bench is the benchmark harness used for docs/BENCHMARKS.md. It is
// deliberately stdlib-only and has four modes:
//
//	bench stub      fake OpenAI-compatible upstream with tunable latency/SSE pacing
//	bench sink      OTLP/HTTP sink that accepts and discards (a real exporter has
//	                something to talk to, without needing a collector)
//	bench passthru  minimal httputil.ReverseProxy, the "bare proxy" baseline
//	bench load      open-loop constant-rate load generator (no coordinated omission)
//
// Latency is measured from the *intended* send time, so a stalled system is
// charged for the requests it delayed, as in wrk2.
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: bench stub|sink|passthru|load [flags]")
		os.Exit(2)
	}
	args := os.Args[2:]
	switch os.Args[1] {
	case "stub":
		runStub(args)
	case "sink":
		runSink(args)
	case "passthru":
		runPassthru(args)
	case "load":
		runLoad(args)
	default:
		fmt.Fprintln(os.Stderr, "unknown mode", os.Args[1])
		os.Exit(2)
	}
}

func serve(addr string, h http.Handler) {
	srv := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 5 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		c, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(c)
	}()
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// ---------------------------------------------------------------- stub

func runStub(args []string) {
	fs := flag.NewFlagSet("stub", flag.ExitOnError)
	listen := fs.String("listen", "0.0.0.0:8090", "listen address")
	latency := fs.Duration("latency", 0, "fixed delay before a non-streaming response")
	payload := fs.Int("payload-bytes", 512, "assistant text size for non-streaming responses")
	chunks := fs.Int("chunks", 50, "SSE content chunks per streamed response")
	interval := fs.Duration("chunk-interval", 0, "delay between SSE chunks")
	ttft := fs.Duration("ttft", 0, "delay before the first SSE chunk")
	_ = fs.Parse(args)
	text := strings.Repeat("x", *payload)
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok\n") })
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req)
		if req.Model == "" {
			req.Model = "gpt-5-mini"
		}
		if !req.Stream {
			if *latency > 0 {
				time.Sleep(*latency)
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"id":"chatcmpl_b","model":%q,"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":%q}}],"usage":{"prompt_tokens":120,"completion_tokens":%d,"total_tokens":%d}}`,
				req.Model, text, *payload/4, 120+*payload/4)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		if *ttft > 0 {
			time.Sleep(*ttft)
		}
		for i := 0; i < *chunks; i++ {
			fmt.Fprintf(w, "data: {\"id\":\"chatcmpl_b\",\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"content\":\"tok \"}}]}\n\n", req.Model)
			if fl != nil {
				fl.Flush()
			}
			if *interval > 0 {
				time.Sleep(*interval)
			}
		}
		fmt.Fprintf(w, "data: {\"id\":\"chatcmpl_b\",\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":120,\"completion_tokens\":%d}}\n\ndata: [DONE]\n\n", req.Model, *chunks)
	})
	serve(*listen, mux)
}

// ---------------------------------------------------------------- sink

func runSink(args []string) {
	fs := flag.NewFlagSet("sink", flag.ExitOnError)
	listen := fs.String("listen", "0.0.0.0:4318", "listen address")
	_ = fs.Parse(args)
	var reqs, bytes atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/stats", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "requests=%d bytes=%d\n", reqs.Load(), bytes.Load())
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.Copy(io.Discard, r.Body)
		reqs.Add(1)
		bytes.Add(n)
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(http.StatusOK)
	})
	serve(*listen, mux)
}

// ---------------------------------------------------------------- passthru

func runPassthru(args []string) {
	fs := flag.NewFlagSet("passthru", flag.ExitOnError)
	listen := fs.String("listen", "0.0.0.0:4001", "listen address")
	target := fs.String("target", "http://127.0.0.1:8090", "upstream")
	_ = fs.Parse(args)
	u, err := url.Parse(*target)
	if err != nil {
		panic(err)
	}
	rp := httputil.NewSingleHostReverseProxy(u)
	rp.FlushInterval = -1
	rp.Transport = &http.Transport{MaxIdleConns: 4096, MaxIdleConnsPerHost: 4096, IdleConnTimeout: 90 * time.Second}
	serve(*listen, rp)
}

// ---------------------------------------------------------------- load

type result struct {
	total, ttft time.Duration
	ok          bool
}

func runLoad(args []string) {
	fs := flag.NewFlagSet("load", flag.ExitOnError)
	target := fs.String("url", "http://127.0.0.1:4000/v1/chat/completions", "target URL")
	rate := fs.Float64("rate", 100, "requests per second (open loop)")
	dur := fs.Duration("duration", 30*time.Second, "measured duration")
	warm := fs.Duration("warmup", 5*time.Second, "warmup (excluded)")
	stream := fs.Bool("stream", false, "request streaming responses")
	insecure := fs.Bool("insecure", false, "skip TLS verification")
	maxInflight := fs.Int("max-inflight", 20000, "cap on concurrent requests (excess counted as dropped)")
	label := fs.String("label", "", "free-form label copied into the output")
	model := fs.String("model", "gpt-5-mini", "model name in the request body")
	maxTokens := fs.Int("max-tokens", 0, "max_tokens in the request body (0 = omit)")
	prompt := fs.String("prompt", "benchmark request, please ignore", "user message content")
	_ = fs.Parse(args)

	tr := &http.Transport{
		MaxIdleConns: 8192, MaxIdleConnsPerHost: 8192, IdleConnTimeout: 90 * time.Second,
		TLSClientConfig: &tls.Config{InsecureSkipVerify: *insecure}, //nolint:gosec // benchmark against self-signed cert
	}
	client := &http.Client{Transport: tr}
	mt := ""
	if *maxTokens > 0 {
		mt = fmt.Sprintf(`"max_tokens":%d,`, *maxTokens)
	}
	body := fmt.Sprintf(`{"model":%q,"stream":%v,%s"messages":[{"role":"user","content":%q}]}`, *model, *stream, mt, *prompt)

	var mu sync.Mutex
	var res []result
	var dropped, errs atomic.Int64
	var inflight atomic.Int64
	var wg sync.WaitGroup

	fire := func(intended time.Time, measure bool) {
		defer wg.Done()
		defer inflight.Add(-1)
		req, _ := http.NewRequest(http.MethodPost, *target, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer sk-bench")
		resp, err := client.Do(req)
		if err != nil {
			if measure {
				errs.Add(1)
			}
			return
		}
		var ttft time.Duration
		buf := make([]byte, 4096)
		first := true
		for {
			n, rerr := resp.Body.Read(buf)
			if n > 0 && first {
				ttft = time.Since(intended)
				first = false
			}
			if rerr != nil {
				break
			}
		}
		_ = resp.Body.Close()
		done := time.Since(intended)
		if !measure {
			return
		}
		if resp.StatusCode != 200 {
			errs.Add(1)
			return
		}
		mu.Lock()
		res = append(res, result{total: done, ttft: ttft, ok: true})
		mu.Unlock()
	}

	interval := time.Duration(float64(time.Second) / *rate)
	start := time.Now()
	end := start.Add(*warm + *dur)
	measureFrom := start.Add(*warm)
	for i := 0; ; i++ {
		intended := start.Add(time.Duration(i) * interval)
		if intended.After(end) {
			break
		}
		if d := time.Until(intended); d > 0 {
			time.Sleep(d)
		}
		if inflight.Load() >= int64(*maxInflight) {
			if !intended.Before(measureFrom) {
				dropped.Add(1)
			}
			continue
		}
		inflight.Add(1)
		wg.Add(1)
		go fire(intended, !intended.Before(measureFrom))
	}
	wg.Wait()

	pick := func(v []time.Duration, q float64) float64 {
		if len(v) == 0 {
			return 0
		}
		i := int(q * float64(len(v)-1))
		return float64(v[i].Microseconds())
	}
	tot := make([]time.Duration, len(res))
	tt := make([]time.Duration, len(res))
	for i, r := range res {
		tot[i], tt[i] = r.total, r.ttft
	}
	sort.Slice(tot, func(i, j int) bool { return tot[i] < tot[j] })
	sort.Slice(tt, func(i, j int) bool { return tt[i] < tt[j] })
	out := map[string]any{
		"label": *label, "rate": *rate, "stream": *stream, "n": len(res),
		"errors": errs.Load(), "dropped": dropped.Load(),
		"achieved_rps": float64(len(res)) / dur.Seconds(),
		"total_us":     map[string]float64{"p50": pick(tot, .5), "p90": pick(tot, .9), "p99": pick(tot, .99), "p999": pick(tot, .999), "max": pick(tot, 1)},
	}
	if *stream {
		out["ttft_us"] = map[string]float64{"p50": pick(tt, .5), "p90": pick(tt, .9), "p99": pick(tt, .99), "p999": pick(tt, .999), "max": pick(tt, 1)}
	}
	b, _ := json.Marshal(out)
	fmt.Println(string(b))
}
