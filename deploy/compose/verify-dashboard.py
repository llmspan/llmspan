#!/usr/bin/env python3
"""Runs every Prometheus query in the shipped dashboard against a live
Prometheus and reports panels that return no data.

    python3 deploy/compose/verify-dashboard.py [http://localhost:9090]

Template variables are substituted with "match everything" values. Intended
for the `make demo` stack, a minute or two after it starts.
"""
import json, sys, urllib.parse, urllib.request, pathlib

prom = sys.argv[1] if len(sys.argv) > 1 else "http://localhost:9090"
dash = json.loads((pathlib.Path(__file__).parent / ".." / "grafana" / "dashboards" / "llmspan-overview.json").read_text())

def sub(q):
    return (q.replace("$model", ".*").replace("$caller", ".*").replace("$upstream", ".*")
             .replace("$__range", "10m").replace("$__rate_interval", "1m").replace("$__interval", "1m"))

def panels(items):
    for p in items:
        yield p
        yield from panels(p.get("panels", []))

empty, bad, ok = [], [], 0
for p in panels(dash["panels"]):
    for t in p.get("targets", []):
        expr = t.get("expr")
        if not expr or (t.get("datasource") or {}).get("type") not in (None, "prometheus"):
            continue
        url = prom + "/api/v1/query?" + urllib.parse.urlencode({"query": sub(expr)})
        try:
            data = json.load(urllib.request.urlopen(url, timeout=10))
        except Exception as e:  # noqa: BLE001
            bad.append((p.get("title"), str(e))); continue
        if data.get("status") != "success":
            bad.append((p.get("title"), data.get("error"))); continue
        if data["data"]["result"]:
            ok += 1
        else:
            empty.append(p.get("title"))
print(f"{ok} queries returned data")
for title in empty: print("EMPTY:", title)
for title, err in bad: print("ERROR:", title, err)
sys.exit(1 if bad else 0)
