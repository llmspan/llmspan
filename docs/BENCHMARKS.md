# Benchmarks

Two sets of measurements:

1. **[AWS, three machines](#aws-three-machine-matrix)**: client, proxy and
   upstream on separate hosts, open-loop load, with and without TLS, traces,
   metrics and content capture. This is the one to quote.
2. **[Laptop microbenchmarks](#laptop-microbenchmarks)**: the SSE parser and
   in-process proxy overhead on an M4 Pro. Useful for regressions, not capacity.

Both use a synthetic upstream (fixed latency, tiny payloads). They measure
llmspan's own cost, not what you will see against a real provider.

## AWS three-machine matrix

Run on 2026-11-24 in us-east-2a. Raw results (every run) are in
[`test/bench/aws/results-2026-11-24.jsonl`](../test/bench/aws/results-2026-11-24.jsonl);
the harness is [`test/bench`](../test/bench) (stdlib-only Go: stub upstream, OTLP
sink, bare reverse proxy, open-loop load generator) and
[`test/bench/aws/run.sh`](../test/bench/aws/run.sh) /
[`analyze.py`](../test/bench/aws/analyze.py) regenerate the tables below.

| | |
|---|---|
| Hosts | 3 x EC2 in one cluster placement group, same AZ, private IPs |
| Load generator | c7i.2xlarge (8 vCPU) |
| Proxy under test | c7i.xlarge (4 vCPU, Intel Xeon Platinum 8488C), Ubuntu 24.04, Linux 7.0 |
| Upstream stub + OTLP sink | c7i.xlarge |
| Build | llmspan built from the repo at commit `e99ec08` with Go 1.27.4, `CGO_ENABLED=0 -trimpath -ldflags '-s -w'` |
| Load | open loop at a constant rate; latency is measured from the *intended* send time, so a stalled proxy is charged for the requests it delayed (no coordinated omission). 4 s warmup + 20 s measured, 3 repetitions per cell, cells show the median |
| JSON scenario | stub answers after a fixed 50 ms, 1 KiB assistant text |
| Stream scenario | stub sends 50 SSE chunks 20 ms apart (about 1 s per stream), then usage |
| Exporter | real OTLP/HTTP exporter to a sink on the upstream host that accepts and discards (no collector processing, no backend) |

Variants:

| Variant | What it is |
|---|---|
| `direct` | client straight to the stub (the floor) |
| `passthru` | a 30-line Go `httputil.ReverseProxy`, the cost of *any* Go proxy hop |
| `llmspan-sampled` | llmspan with span sampling off (`sample_ratio: 0`); metrics, parsing and cost still run |
| `llmspan-default` | llmspan, traces + metrics exported, content capture off |
| `llmspan-full` | `llmspan-default` plus `content.mode: events`, default redaction, conversation header |
| `llmspan-tls` | `llmspan-default` with the client-facing listener on TLS (self-signed, TLS 1.3); llmspan to upstream stays plain HTTP |

Settings that differ from the defaults, and why: `max_in_flight` and
`request.max_buffered_bodies` were raised to 20000 and the breaker was disabled
so the harness could not trip them. **At the defaults (256 and 512) llmspan
answers 429 once more than about 512 requests are in flight.** An earlier
validation run with the defaults at 1000 concurrent streams completed only
about half the requests for that reason. Size those limits for your concurrency.

### Results

Added latency is the variant's median minus the `direct` median for the same
scenario. For the 1-second streams the p50/p99 differences are within about
+/-1 ms of noise at 200 req/s. "Proxy cores" is the proxy process's average CPU
over the whole run, "peak RSS" its high-water mark.

**json @ 500 req/s (stub latency 50 ms)**

| variant | p50 ms | added p50 | p99 ms | added p99 | p99.9 ms | errors | proxy cores | peak RSS MiB |
|---|---|---|---|---|---|---|---|---|
| direct | 51.34 | - | 52.38 | - | 52.68 | 0 | - | - |
| passthru | 51.65 | +0.31 | 52.80 | +0.42 | 53.15 | 0 | 0.13 | 14 |
| llmspan-sampled | 51.76 | +0.42 | 52.94 | +0.56 | 53.34 | 0 | 0.26 | 35 |
| llmspan-default | 51.72 | +0.38 | 52.91 | +0.53 | 53.36 | 0 | 0.27 | 37 |
| llmspan-full | 51.79 | +0.45 | 52.98 | +0.60 | 53.37 | 0 | 0.28 | 37 |
| llmspan-tls | 51.71 | +0.38 | 52.96 | +0.58 | 53.32 | 0 | 0.27 | 37 |

**json @ 2000 req/s (stub latency 50 ms)**

| variant | p50 ms | added p50 | p99 ms | added p99 | p99.9 ms | errors | proxy cores | peak RSS MiB |
|---|---|---|---|---|---|---|---|---|
| direct | 51.04 | - | 52.18 | - | 52.49 | 0 | - | - |
| passthru | 51.13 | +0.09 | 52.32 | +0.14 | 52.71 | 0 | 0.36 | 20 |
| llmspan-sampled | 51.22 | +0.18 | 52.58 | +0.40 | 53.79 | 0 | 0.77 | 48 |
| llmspan-default | 51.22 | +0.18 | 52.60 | +0.43 | 53.78 | 0 | 0.81 | 50 |
| llmspan-full | 51.25 | +0.21 | 52.69 | +0.51 | 54.15 | 0 | 0.87 | 50 |
| llmspan-tls | 51.23 | +0.18 | 52.64 | +0.47 | 54.00 | 0 | 0.83 | 53 |

**json @ 5000 req/s (stub latency 50 ms)**

| variant | p50 ms | added p50 | p99 ms | added p99 | p99.9 ms | errors | proxy cores | peak RSS MiB |
|---|---|---|---|---|---|---|---|---|
| direct | 50.98 | - | 52.07 | - | 52.36 | 0 | - | - |
| passthru | 51.04 | +0.06 | 52.38 | +0.30 | 53.20 | 0 | 0.79 | 35 |
| llmspan-sampled | 51.10 | +0.12 | 53.99 | +1.92 | 56.43 | 0 | 1.80 | 73 |
| llmspan-default | 51.11 | +0.13 | 54.52 | +2.45 | 58.43 | 0 | 1.86 | 78 |
| llmspan-full | 51.15 | +0.16 | 54.97 | +2.90 | 57.30 | 0 | 2.02 | 79 |
| llmspan-tls | 51.24 | +0.25 | 56.36 | +4.29 | 59.40 | 0 | 1.89 | 92 |

**stream @ 200 req/s (each stream ~1 s, 50 chunks)**

| variant | p50 ms | added p50 | p99 ms | added p99 | p99.9 ms | TTFT p50 / p99 ms | errors | proxy cores | peak RSS MiB |
|---|---|---|---|---|---|---|---|---|---|
| direct | 1028.45 | - | 1034.28 | - | 1035.46 | 0.67 / 1.50 | 0 | - | - |
| passthru | 1027.76 | -0.69 | 1033.81 | -0.48 | 1035.74 | 0.89 / 1.85 | 0 | 0.23 | 45 |
| llmspan-sampled | 1027.50 | -0.95 | 1033.38 | -0.91 | 1036.15 | 0.93 / 1.89 | 0 | 0.34 | 75 |
| llmspan-default | 1028.28 | -0.17 | 1033.96 | -0.32 | 1035.48 | 0.97 / 1.92 | 0 | 0.35 | 78 |
| llmspan-full | 1028.24 | -0.20 | 1033.98 | -0.30 | 1036.48 | 1.02 / 1.95 | 0 | 0.40 | 79 |
| llmspan-tls | 1028.04 | -0.41 | 1033.41 | -0.87 | 1035.97 | 1.00 / 1.90 | 0 | 0.37 | 92 |

**stream @ 1000 req/s (each stream ~1 s, 50 chunks)**

| variant | p50 ms | added p50 | p99 ms | added p99 | p99.9 ms | TTFT p50 / p99 ms | errors | proxy cores | peak RSS MiB |
|---|---|---|---|---|---|---|---|---|---|
| direct | 1021.59 | - | 1028.53 | - | 1033.86 | 0.63 / 1.35 | 0 | - | - |
| passthru | 1023.02 | +1.44 | 1030.75 | +2.22 | 1033.33 | 0.80 / 1.72 | 0 | 0.85 | 170 |
| llmspan-sampled | 1024.18 | +2.60 | 1034.26 | +5.73 | 1041.01 | 0.84 / 11.28 | 0 | 1.37 | 231 |
| llmspan-default | 1024.23 | +2.64 | 1034.95 | +6.42 | 1041.11 | 0.85 / 11.63 | 0 | 1.42 | 228 |
| llmspan-full | 1024.56 | +2.98 | 1037.37 | +8.84 | 1045.37 | 0.88 / 14.92 | 0 | 1.71 | 231 |
| llmspan-tls | 1023.96 | +2.37 | 1035.88 | +7.35 | 1042.52 | 0.85 / 14.51 | 0 | 1.50 | 251 |

### What the numbers say

- **Per-request latency.** At 2,000 req/s llmspan adds about 0.2 ms at p50 and
  0.4 to 0.5 ms at p99 over a direct call. At 5,000 req/s (which uses about
  half of the proxy's 4 vCPUs) that is +0.13 ms at p50 and +2.5 ms at p99 with
  default settings (+0.16 / +2.9 ms with content capture and redaction, +0.25 /
  +4.3 ms with TLS termination). A bare Go reverse proxy adds +0.06 / +0.3 ms
  at the same rate, so most of the p50 is the cost of having a proxy at all.
- **CPU is the real price.** At 5,000 req/s llmspan used about 1.9 cores against
  0.8 for the bare proxy (roughly 0.37 ms of CPU per request versus 0.16 ms).
  Parsing, metrics, cost and span creation are what you pay for.
- **Streaming.** Time to first chunk stays under a millisecond at p50 (0.85 ms
  versus 0.63 ms direct). At 1,000 new streams per second (1,000 concurrent) the
  tail grows: TTFT p99 is 11 to 15 ms against 1.4 ms direct, and whole-stream
  p99 is 6 to 9 ms higher, on a 1 s stream.
- **Memory.** About 78 MiB peak RSS at 200 concurrent streams and about 230 MiB at
  1,000 (roughly 0.2 MiB per additional stream), against 45 and 170 MiB for the
  bare proxy. 5,000 JSON req/s peaked at about 80 MiB.
- **Errors.** Zero errors and zero dropped requests in all 90 runs.
- **Content capture and redaction** cost about 0.1 core and a few hundred
  microseconds at p99 at these (tiny) prompt sizes. Large prompts will cost more;
  that was not measured.

### Not measured

Hosted providers (only a local model was tried, above), saturation (the proxy never exceeded about 50% CPU, so this is not a maximum
throughput figure), HTTP/2, large or multimodal bodies, real provider latency
or TLS to the upstream, a real collector or backend, other instance sizes or
architectures, and runs longer than 24 seconds. One region, one day, one
instance family. Treat the figures as a baseline to compare your own run
against, and use `test/bench` to reproduce them in your environment.

## Against a real model (local)

A stub has zero inference time, which flatters nothing but also hides nothing
useful. To check the claim "the proxy is lost in the noise next to a real model",
[`test/bench/real-model.sh`](../test/bench/real-model.sh) alternates direct and
via-llmspan rounds (A/B/A/B, so drift hits both equally) against a real model:
`llama3.2:1b` in Ollama on an Apple M4 Pro, non-streaming, `max_tokens: 48`,
0.5 req/s, 4 rounds of 40 s, 84 measured requests per variant, llmspan with
traces and metrics exported to a discarding OTLP sink.

| | p50 (median of 4 rounds) | p50 range | p99 (median) | p99 range |
|---|---|---|---|---|
| direct to Ollama | 364.2 ms | 356.4 to 375.4 | 416.1 ms | 369.8 to 439.4 |
| via llmspan | 364.4 ms | 357.7 to 370.1 | 386.0 ms | 367.4 to 399.9 |

The difference at p50 (+0.2 ms) is far inside the round-to-round spread (about
20 ms), and the p99 difference points the other way, which is just noise. So
against a real model llmspan's overhead is below what this setup can measure;
the AWS matrix above is the place to read its actual cost. This is a local
model on one machine, single client, low rate, non-streaming. It is not a
hosted provider and says nothing about their network latency.

## Laptop microbenchmarks

Everything below was measured on one laptop with an in-process stub upstream
(zero upstream latency, tiny payloads). Client, proxy and stub share the same
CPUs, so absolute req/s is a floor for the proxy, not a service capacity
figure. No per-core numbers were measured (no CPU pinning).

## Machine

| | |
|---|---|
| CPU | Apple M4 Pro (`sysctl -n machdep.cpu.brand_string`), 12 logical CPUs |
| Go | go1.27.4 darwin/arm64 |
| GOMAXPROCS | unset (defaults to 12) |
| Telemetry | real SDK tracer + meter providers, no exporter (spans created and dropped) |
| Date | 2026-11-24 |

## Commands

```sh
# SSE parser (before/after numbers below)
go test ./internal/provider -run xxx -bench 'SSETee|FindFrame' -benchtime 300x -count 5

# Proxy overhead, non-streaming (ns/op, B/op, allocs/op)
go test ./internal/proxy -run xxx -bench JSON -benchtime 3000x -count 5

# Added latency p50/p99, streaming TTFT, 200 concurrent streams + leak check
BENCH_N=5000 go test ./internal/proxy -run TestBench -v -count=1
#   tunables: BENCH_N, BENCH_STREAMS (200), BENCH_CHUNKS (3000)

# Load-harness leak check (120 concurrent streams)
go test -tags loadtest -count=1 -run ConcurrentStreamsNoLeak -v ./test/load
```

Compare runs with `benchstat old.txt new.txt` (`go install golang.org/x/perf/cmd/benchstat@latest`;
it was not installed here, so the table below uses medians of 5 runs by hand).

## 1. SSE frame parser (internal/provider/sse.go)

Old: `findFrameBoundary` ran three `bytes.Index` scans over the whole buffer on
every read (quadratic for a large event arriving in small reads), allocated a
separator slice table per call, copied every frame, and `splitSSELines`
allocated a line slice per event. New: one allocation-free pass that resumes
from the tail of the previous scan, no frame copy, no line slice. Behaviour is
checked against the old implementation by a differential test
(`sse_diff_test.go`) and read-split invariance test, plus the existing tests.

Workloads: 1000 events of ~100 B in 512 B reads; same with CRLF framing;
one 256 KiB event in 1 KiB reads.

| Benchmark (median of 5) | Before | After | Change |
|---|---|---|---|
| SmallEvents (`\n\n`) | 122.0 us, 262.6 KB, 3014 allocs | 89.4 us, 86.5 KB, 1012 allocs | -27% time, -67% bytes, -66% allocs |
| SmallEventsCRLF | 241.7 us, 262.6 KB, 3014 allocs | 83.1 us, 86.5 KB, 1012 allocs | -66% time |
| LargeEventSmallReads | 1285 us, 1.58 MB, 17 allocs | 241 us, 1.31 MB, 15 allocs | -81% time (5.3x) |
| FindFrameBoundary (4 KiB buf) | 170 ns | 109 ns | -36% |

The remaining ~1 alloc/event is the per-event `data` slice and event-name
string handed to the callback; it is kept because handlers may retain it.

Raw output, before:

```
BenchmarkSSETeeSmallEvents-12             	     300	    126325 ns/op	 625.48 MB/s	  262621 B/op	    3014 allocs/op
BenchmarkSSETeeSmallEvents-12             	     300	    121988 ns/op	 647.72 MB/s	  262622 B/op	    3014 allocs/op
BenchmarkSSETeeSmallEvents-12             	     300	    120558 ns/op	 655.40 MB/s	  262602 B/op	    3014 allocs/op
BenchmarkSSETeeSmallEvents-12             	     300	    122024 ns/op	 647.53 MB/s	  262603 B/op	    3014 allocs/op
BenchmarkSSETeeSmallEvents-12             	     300	    124264 ns/op	 635.86 MB/s	  262603 B/op	    3014 allocs/op
BenchmarkSSETeeSmallEventsCRLF-12         	     300	    245092 ns/op	 330.55 MB/s	  262649 B/op	    3014 allocs/op
BenchmarkSSETeeSmallEventsCRLF-12         	     300	    241673 ns/op	 335.23 MB/s	  262649 B/op	    3014 allocs/op
BenchmarkSSETeeSmallEventsCRLF-12         	     300	    240961 ns/op	 336.22 MB/s	  262650 B/op	    3014 allocs/op
BenchmarkSSETeeSmallEventsCRLF-12         	     300	    242114 ns/op	 334.62 MB/s	  262649 B/op	    3014 allocs/op
BenchmarkSSETeeSmallEventsCRLF-12         	     300	    240448 ns/op	 336.94 MB/s	  262649 B/op	    3014 allocs/op
BenchmarkSSETeeLargeEventSmallReads-12    	     300	   1288298 ns/op	 203.49 MB/s	 1582088 B/op	      17 allocs/op
BenchmarkSSETeeLargeEventSmallReads-12    	     300	   1284143 ns/op	 204.15 MB/s	 1582088 B/op	      17 allocs/op
BenchmarkSSETeeLargeEventSmallReads-12    	     300	   1285203 ns/op	 203.98 MB/s	 1582088 B/op	      17 allocs/op
BenchmarkSSETeeLargeEventSmallReads-12    	     300	   1284255 ns/op	 204.13 MB/s	 1582086 B/op	      17 allocs/op
BenchmarkSSETeeLargeEventSmallReads-12    	     300	   1286573 ns/op	 203.76 MB/s	 1582088 B/op	      17 allocs/op
BenchmarkFindFrameBoundary-12             	     300	       170.7 ns/op	      31 B/op	       0 allocs/op
BenchmarkFindFrameBoundary-12             	     300	       172.8 ns/op	      31 B/op	       0 allocs/op
BenchmarkFindFrameBoundary-12             	     300	       170.7 ns/op	      31 B/op	       0 allocs/op
BenchmarkFindFrameBoundary-12             	     300	       168.8 ns/op	      31 B/op	       0 allocs/op
BenchmarkFindFrameBoundary-12             	     300	       169.4 ns/op	      31 B/op	       0 allocs/op
```

After:

```
BenchmarkSSETeeSmallEvents-12             	     300	     91195 ns/op	 866.43 MB/s	   86526 B/op	    1012 allocs/op
BenchmarkSSETeeSmallEvents-12             	     300	     89386 ns/op	 883.96 MB/s	   86491 B/op	    1012 allocs/op
BenchmarkSSETeeSmallEvents-12             	     300	     90517 ns/op	 872.92 MB/s	   86490 B/op	    1012 allocs/op
BenchmarkSSETeeSmallEvents-12             	     300	     89074 ns/op	 887.06 MB/s	   86490 B/op	    1012 allocs/op
BenchmarkSSETeeSmallEvents-12             	     300	     88466 ns/op	 893.16 MB/s	   86491 B/op	    1012 allocs/op
BenchmarkSSETeeSmallEventsCRLF-12         	     300	     83125 ns/op	 974.62 MB/s	   86537 B/op	    1012 allocs/op
BenchmarkSSETeeSmallEventsCRLF-12         	     300	     81984 ns/op	 988.20 MB/s	   86537 B/op	    1012 allocs/op
BenchmarkSSETeeSmallEventsCRLF-12         	     300	     83084 ns/op	 975.11 MB/s	   86537 B/op	    1012 allocs/op
BenchmarkSSETeeSmallEventsCRLF-12         	     300	     84480 ns/op	 959.00 MB/s	   86537 B/op	    1012 allocs/op
BenchmarkSSETeeSmallEventsCRLF-12         	     300	     83248 ns/op	 973.18 MB/s	   86537 B/op	    1012 allocs/op
BenchmarkSSETeeLargeEventSmallReads-12    	     300	    243922 ns/op	1074.73 MB/s	 1311657 B/op	      15 allocs/op
BenchmarkSSETeeLargeEventSmallReads-12    	     300	    241500 ns/op	1085.51 MB/s	 1311654 B/op	      15 allocs/op
BenchmarkSSETeeLargeEventSmallReads-12    	     300	    240778 ns/op	1088.77 MB/s	 1311657 B/op	      15 allocs/op
BenchmarkSSETeeLargeEventSmallReads-12    	     300	    238693 ns/op	1098.28 MB/s	 1311654 B/op	      15 allocs/op
BenchmarkSSETeeLargeEventSmallReads-12    	     300	    244704 ns/op	1071.30 MB/s	 1311654 B/op	      15 allocs/op
BenchmarkFindFrameBoundary-12             	     300	       108.6 ns/op	      31 B/op	       0 allocs/op
BenchmarkFindFrameBoundary-12             	     300	       115.6 ns/op	      31 B/op	       0 allocs/op
BenchmarkFindFrameBoundary-12             	     300	       107.6 ns/op	      31 B/op	       0 allocs/op
BenchmarkFindFrameBoundary-12             	     300	       125.1 ns/op	      31 B/op	       0 allocs/op
BenchmarkFindFrameBoundary-12             	     300	       109.4 ns/op	      31 B/op	       0 allocs/op
```

## 2. Non-streaming completion: direct vs via proxy

Single request in flight, keep-alive, 11-token JSON completion. B/op and
allocs/op include the in-process client and stub upstream, so read the
difference, not the absolute values.

```
cpu: Apple M4 Pro
BenchmarkJSONDirect-12              	    3000	     43834 ns/op	    7906 B/op	      85 allocs/op
BenchmarkJSONDirect-12              	    3000	     41150 ns/op	    7986 B/op	      85 allocs/op
BenchmarkJSONDirect-12              	    3000	     38609 ns/op	    7893 B/op	      85 allocs/op
BenchmarkJSONDirect-12              	    3000	     38534 ns/op	    7930 B/op	      85 allocs/op
BenchmarkJSONDirect-12              	    3000	     38703 ns/op	    7975 B/op	      85 allocs/op
BenchmarkJSONViaProxy-12            	    3000	    119390 ns/op	   71933 B/op	     293 allocs/op
BenchmarkJSONViaProxy-12            	    3000	    114256 ns/op	   68143 B/op	     291 allocs/op
BenchmarkJSONViaProxy-12            	    3000	    108638 ns/op	   68990 B/op	     291 allocs/op
BenchmarkJSONViaProxy-12            	    3000	    108908 ns/op	   69110 B/op	     291 allocs/op
BenchmarkJSONViaProxy-12            	    3000	    107852 ns/op	   69045 B/op	     291 allocs/op
BenchmarkJSONViaProxyParallel-12    	    3000	     43730 ns/op	   68375 B/op	     291 allocs/op
BenchmarkJSONViaProxyParallel-12    	    3000	     36915 ns/op	   62934 B/op	     289 allocs/op
BenchmarkJSONViaProxyParallel-12    	    3000	     37352 ns/op	   63630 B/op	     289 allocs/op
BenchmarkJSONViaProxyParallel-12    	    3000	     42341 ns/op	   64779 B/op	     289 allocs/op
BenchmarkJSONViaProxyParallel-12    	    3000	     39186 ns/op	   62925 B/op	     289 allocs/op
```

| | Direct | Via proxy | Delta |
|---|---|---|---|
| ns/op (median) | 38.7 us | 108.9 us | +70 us |
| B/op | 7.9 KB | 69.0 KB | +61 KB |
| allocs/op | 85 | 291 | +206 |
| Latency p50 (N=5000) | 36.7 us | 104.1 us | +67 us |
| Latency p99 (N=5000) | 88.3 us | 323.0 us | +235 us |
| Sequential req/s, 1 client | 24,596 | 8,475 | |

`BenchmarkJSONViaProxyParallel` (GOMAXPROCS goroutines) sustains ~37-43 us/op
wall clock, i.e. roughly 25k req/s total with client and stub competing for
the same 12 CPUs. That is a stub-upstream number, not a capacity claim.

## 3. Streaming time-to-first-chunk

Upstream emits a chunk, sleeps 50 ms, repeats (4 chunks). 30 repetitions
after warm-up.

```
    bench_test.go:268: TTFT direct p50=116.875µs p99=192.083µs | via-proxy p50=277.75µs p99=361.625µs | delta p50=160.875µs
    bench_test.go:270: second-chunk arrival direct p50=51.182541ms via-proxy p50=51.297875ms delta=115.334µs (upstream gap 50ms)
--- PASS: TestBenchStreamingTTFT (10.08s)
```

The proxy adds about 0.16 ms to time-to-first-chunk and about 0.1 ms to later
chunks; it does not buffer (the test fails if first-chunk p50 exceeds half the
upstream gap).

## 4. Concurrent SSE streams and leak check

200 streams held open simultaneously (a barrier in the stub upstream proves
peak concurrency), 3000 small chunks each (600k chunks), through the proxy.

```
    bench_test.go:402: streams=200 chunks/stream=3000 peak_concurrent=200 total_chunks=600000 drain_time=92.549417ms (6483023 chunks/s aggregate)
    bench_test.go:404: goroutines baseline=4 open=1804 after=4
    bench_test.go:405: heap baseline=2229 KiB, while open=22556 KiB, after drain=8108 KiB; sys while open=53074 KiB
    bench_test.go:407: per stream (proxy + in-process client + stub upstream, all in this process): heap=101 KiB goroutines=9.0
```

- All 200 streams were concurrently active (`peak_concurrent=200`).
- Goroutines returned exactly to baseline (4 -> 4) once the proxy's pooled
  upstream keep-alive connections were closed. Before closing them, ~1
  goroutine per stream remained; stack inspection showed those are the
  transport's idle-connection read/write loops, not leaked handlers.
- Under `-race` the same test passes (drain ~1.3 s instead of ~0.1 s). Client launches are staggered every 25 streams; an unstaggered 200-way dial burst occasionally stalled a few streams on loopback SYN retransmit.
- Memory: ~101-106 KiB heap and ~9 goroutines per open stream. This is the
  whole process (proxy + client + stub upstream), so it is an upper bound on
  the proxy's share.

## Summary

| Metric | Result on this machine (stub upstream) |
|---|---|
| Added latency, non-streaming | p50 +67 us, p99 +235 us |
| Added allocs per request | ~206 allocs, ~61 KB |
| TTFT delta (streaming) | +0.16 ms p50 |
| Mem per concurrent stream | ~0.1 MiB (process-wide upper bound) |
| Goroutine leak after 200 x 3000-chunk streams | none |
| Sequential req/s, 1 client | 8.5k via proxy vs 24.6k direct |

What this supports: "tens of microseconds to a fraction of a millisecond of
added latency per request" and "streams are not buffered". What it does not
support: "zero overhead" (the proxy costs ~3x a bare stub round trip in CPU
terms, though that is invisible against a real LLM call), and any
requests-per-core figure.

## Proposed Makefile targets (not applied)

```make
bench:
	$(GO) test ./internal/provider -run xxx -bench 'SSETee|FindFrame' -benchtime 300x -count 5
	$(GO) test ./internal/proxy -run xxx -bench JSON -benchtime 3000x -count 5

bench-proxy:
	BENCH_N=5000 $(GO) test ./internal/proxy -run TestBench -v -count=1
```
