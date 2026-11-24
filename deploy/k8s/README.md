# Kubernetes packaging

llmspan ships a Helm chart under [`../helm/llmspan`](../helm/llmspan) plus
stand-alone examples in this directory.

```bash
# Create TLS + auth secrets first (see certificate.yaml), then:
helm upgrade --install llmspan ../helm/llmspan --set image.tag=v0.1.5
```

`auth.enabled` defaults to **true**: the chart binds `0.0.0.0` and llmspan
refuses a non-loopback listener without caller auth. Create the tokens
Secret first (entries may be named; the name becomes the caller label):

```bash
echo -n "$BILLING_SECRET" | llmspan hash-token --name billing >  tokens
echo -n "$SEARCH_SECRET"  | llmspan hash-token --name search  >> tokens
kubectl create secret generic llmspan-auth-tokens --from-file=tokens
```

To run without listener auth (e.g. mesh mTLS + NetworkPolicy already limit
callers) set `--set auth.enabled=false --set auth.allowUnauthenticated=true`;
rendering fails otherwise.

## Self metrics (optional)

`--set metrics.enabled=true` opens a separate plaintext, unauthenticated
admin port (default 9464) serving Prometheus-format `/metrics` for the
`llmspan.*` self metrics. Add `--set metrics.serviceMonitor.enabled=true`
for a Prometheus Operator `ServiceMonitor`. Allow only your Prometheus to
reach the port in the NetworkPolicy.

## Pricing override

`--set pricing.existingConfigMap=my-prices` mounts that ConfigMap (key
`prices.yaml`, override with `pricing.key`) and sets `pricing.path`.

## Probes & drain

| Probe | Path | Notes |
|---|---|---|
| liveness | `/healthz` | Process alive; always 200 once listening |
| readiness | `/readyz` | 200 while serving, independent of collector reachability (opt in to waiting for the first OTLP export with `telemetry.ready_requires_export`); **503 on SIGTERM** before stream drain |

The distroless image has no `sleep` binary, so the chart does **not** use a
`preStop` sleep. Instead:

1. SIGTERM → `BeginDrain` → `/readyz` flips 503
2. `terminationGracePeriodSeconds` = preStop budget + `shutdown_timeout`
3. kube removes the Endpoint; in-flight SSE streams drain inside the grace window

Set `GOMEMLIMIT` ~10% under the memory limit (chart default: 1800MiB / 2Gi).

## Per-replica state (important)

These are **process-local**, not cluster-wide:

| Mechanism | Multi-replica behaviour |
|---|---|
| Circuit breaker | Each pod trips independently; one brownout can open N breakers |
| `max_in_flight` | Cap is per pod; cluster fan-out ≈ `replicas × max_in_flight` |
| Auth argon2 / negative cache | Per pod — failed tokens cached 30s before argon2 re-runs |
| Model-label cardinality (`_other` bucket) | Per pod — the same model can be a real series on one replica and `_other` on another if the label set diverges |

Size caps for **per-pod** load, not cluster aggregate. Prefer fewer larger
replicas if you need coherent breaker behaviour; prefer more smaller ones for
blast-radius isolation.

## `auth.tokens_file`

Mount a Secret key `tokens` (one entry per line: `<hash>`, `name:<hash>` or
`name <hash>`) and set:

```yaml
auth:
  tokens_file: /etc/llmspan/auth/tokens
```

Or `LLMSPAN_AUTH_TOKENS_FILE`. Inline `auth.tokens` in ConfigMaps is discouraged.

## Container health

```
llmspan healthcheck                 # GET $LLMSPAN_HEALTHCHECK_URL
llmspan healthcheck --insecure      # HTTPS + self-signed (TLS pods)
```

Dockerfile `HEALTHCHECK` uses the same subcommand (no curl in distroless).
