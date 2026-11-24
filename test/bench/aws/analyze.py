#!/usr/bin/env python3
"""Summarise results.jsonl from run.sh as markdown tables.

Latency columns are medians across repetitions. "Added" is the variant's median
minus the `direct` (no proxy) median for the same scenario. CPU is the proxy
process's average cores over the whole run window (warmup + measurement)."""
import json, statistics, sys, collections

rows = [json.loads(l) for l in open(sys.argv[1]) if l.strip()]
WARM, DUR = float(sys.argv[2]) if len(sys.argv) > 2 else 4, float(sys.argv[3]) if len(sys.argv) > 3 else 20
window = WARM + DUR
order = ["direct", "passthru", "llmspan-sampled", "llmspan-default", "llmspan-full", "llmspan-tls"]
g = collections.defaultdict(list)
for r in rows:
    _, kind, rate, _ = r["label"].split("/")
    g[(kind, int(rate), r["variant"])].append(r)

def med(rs, path):
    vals = []
    for r in rs:
        v = r
        for k in path: v = v.get(k) if isinstance(v, dict) else None
        if v is not None: vals.append(v)
    return statistics.median(vals) if vals else None

def ms(us): return "-" if us is None else f"{us/1000:.2f}"
def add(us, base): return "-" if us is None or base is None else f"{(us-base)/1000:+.2f}"

for kind in ("json", "stream"):
    rates = sorted({k[1] for k in g if k[0] == kind})
    for rate in rates:
        title = f"{kind} @ {rate} req/s" + (" (each stream ~1 s, 50 chunks)" if kind == "stream" else " (stub latency 50 ms)")
        print(f"\n**{title}**\n")
        hdr = "| variant | p50 ms | added p50 | p99 ms | added p99 | p99.9 ms | " + ("TTFT p50 / p99 ms | " if kind == "stream" else "") + "errors | proxy cores | peak RSS MiB |"
        print(hdr); print("|" + "---|" * (hdr.count("|") - 1))
        for v in order:
            rs = g.get((kind, rate, v))
            if not rs: continue
            p50, p99, p999 = (med(rs, ["total_us", k]) for k in ("p50", "p99", "p999"))
            errs = sum(r["errors"] + r["dropped"] for r in rs)
            cpu = med(rs, ["proxy_cpu_s"]); hwm = med(rs, ["proxy_hwm_kb"])
            cores = "-" if cpu is None else f"{cpu/window:.2f}"
            rss = "-" if hwm is None else f"{hwm/1024:.0f}"
            ttft = f"{ms(med(rs,['ttft_us','p50']))} / {ms(med(rs,['ttft_us','p99']))} | " if kind == "stream" else ""
            a50 = "-" if v == "direct" else add(p50, med(g[(kind, rate, "direct")], ["total_us", "p50"]))
            a99 = "-" if v == "direct" else add(p99, med(g[(kind, rate, "direct")], ["total_us", "p99"]))
            print(f"| {v} | {ms(p50)} | {a50} | {ms(p99)} | {a99} | {ms(p999)} | {ttft}{errs} | {cores} | {rss} |")
