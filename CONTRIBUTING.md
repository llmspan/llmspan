# Contributing to llmspan

Thanks for considering a contribution. llmspan sits on a production
trust boundary — it sees credentials and prompts — so the bar for
correctness and test coverage is deliberately high, but the codebase
is small and the workflow is simple.

## Prerequisites

- Go 1.26+ (see [Go toolchain](#go-toolchain))
- `make` and a POSIX shell
- Optional: `golangci-lint` (the `lint` target falls back to `go vet`
  without it), Docker for the compose demo stack

## Getting started

```bash
git clone https://github.com/llmspan/llmspan
cd llmspan
make test      # go test -race ./...
make run       # builds and starts against config.example.yaml
```

`llmspan check --config <file>` validates a config without starting
the server — useful while iterating.

## Your first PR

New here? This path works:

1. Read [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) (ten minutes) to see
   how a request flows through the proxy.
2. Pick an issue labelled
   [`good first issue`](https://github.com/llmspan/llmspan/labels/good%20first%20issue)
   (or `help wanted` for larger ones). Comment that you are taking it so
   we do not duplicate work. For anything larger than a small fix, open or
   find an issue first and agree on the approach.
3. Fork, branch from `main`, and run `make test` once before you change
   anything, so you know the baseline is green on your machine.
4. Make the change with tests, then run the local gate (below).
5. Open a PR using the template. A draft PR early is welcome; say what you
   want feedback on.

Adding support for a new LLM API? Follow
[docs/ADDING_A_PROVIDER.md](docs/ADDING_A_PROVIDER.md).

Contributions are accepted under the project's Apache-2.0 license (inbound = outbound, per section 5 of the license); there is no CLA. Sign off commits with `git commit -s` if your employer requires it.

Participation is covered by the [Code of Conduct](CODE_OF_CONDUCT.md).

## Go toolchain

`go.mod` declares the minimum supported version (currently `go 1.26.0`), and
CI runs the test suite on a matrix of Go `1.26` and the latest `stable`
release, so a toolchain or dependency break surfaces before users hit it. Lint,
fuzz, vuln, load and build jobs use 1.26. Develop on any Go at or above the
`go.mod` version; do not use language or library features newer than it.

## Making changes

1. Fork and branch from `main`.
2. Keep changes focused; one logical change per PR.
3. Add or update tests. Bug fixes need a regression test that fails
   without the fix. `-race` is always on in CI.
4. Run the full gate locally before pushing:

   ```bash
   make lint test build
   ```

5. Update docs that your change makes stale: `README.md`,
   `config.example.yaml` comments, and `CHANGELOG.md` (add an entry
   under **Unreleased**).

### Running the CI coverage gate locally

CI enforces 70% total line coverage over `internal/` plus per-package floors
(auth 55, config 65, provider 50, proxy 55, redact 70, telemetry 45). These are
the exact commands from `.github/workflows/ci.yml`:

```bash
go test -race -count=1 -timeout=2m \
  -covermode=atomic \
  -coverpkg=./internal/... \
  -coverprofile=coverage.out \
  ./...

go tool cover -func=coverage.out | tee coverage.txt
pct=$(go tool cover -func=coverage.out | awk '/^total:/ {gsub(/%/, "", $NF); print $NF}')
echo "total coverage: ${pct}%"
awk -v p="$pct" 'BEGIN { exit (p+0 < 70.0) }' || echo "FAIL: below 70% gate"

for spec in "auth:55" "config:65" "provider:50" "proxy:55" "redact:70" "telemetry:45"; do
  dir=${spec%%:*}; min=${spec##*:}
  avg=$(go tool cover -func=coverage.out | grep "/internal/${dir}/" | awk '{gsub(/%/,"",$NF); sum+=$NF; n++} END { if (n) printf "%.1f", sum/n; else print "0" }')
  echo "internal/${dir} avg coverage: ${avg}% (floor ${min}%)"
  awk -v p="$avg" -v m="$min" 'BEGIN { exit (p+0 < m+0) }' || echo "FAIL: internal/${dir} below ${min}%"
done
```

The per-package number is the average of per-function percentages, not
statement coverage, so `go test -cover` for a package will differ slightly.

Other CI jobs you can run locally: `make load-test` (p99 added-latency budget),
`govulncheck ./...`, and the short fuzz targets (`go test ./internal/redact/
-fuzz=FuzzRedactIdempotent -fuzztime=30s`; see the `fuzz` job for the full
list). Run `./test/smoke/smoke.sh` for the compose end-to-end check (needs
Docker).

### Benchmarks

Performance claims need numbers. Methodology, commands and current results are
in [docs/BENCHMARKS.md](docs/BENCHMARKS.md). If you touch the SSE parser, the
proxy hot path, pricing or redaction, run the relevant benchmark before and
after and paste both in the PR:

```bash
go test ./internal/provider -run xxx -bench 'SSETee|FindFrame' -benchtime 300x -count 5
go test ./internal/proxy -run xxx -bench JSON -benchtime 3000x -count 5
```

Compare with `benchstat old.txt new.txt`.

## Pre-1.0 stability

llmspan is pre-1.0. Until 1.0, config keys, metric names, span and attribute
names, label sets, the `prices.yaml` schema and CLI flags may change in a minor
release. The OpenTelemetry GenAI conventions we follow are themselves in
Development status (see
[docs/SEMCONV-COMPLIANCE.md](docs/SEMCONV-COMPLIANCE.md)). We try to avoid
gratuitous breakage: when something is renamed or removed we keep the old
form for at least one release where practical, and every deprecation or
breaking change is listed in `CHANGELOG.md`. If your PR changes a name or
config key that users could depend on, say so in the PR and the changelog entry.

## Commit messages

Follow the existing convention:

```
type(scope): imperative summary

fix(proxy): release breaker probe slot on passthrough exit
feat(config): add LLMSPAN_SAMPLE_RATIO env override
docs: correct PromQL examples for cost metrics
```

Types in use: `feat`, `fix`, `perf`, `refactor`, `test`, `docs`,
`chore`, `ci`.

## What reviewers look for

- **Bounded everything.** Buffers, retries, goroutines, and attribute
  sizes all need explicit ceilings. Unbounded growth on a hostile
  input is a rejected review, not a nit.
- **No content leaks.** Prompt/completion bytes must never reach a
  span, log line, or error message unless content capture is on, and
  then only after redaction. Redact before truncating.
- **Passthrough fidelity.** The proxy forwards bytes it doesn't
  understand rather than failing the request. Enrichment is
  best-effort; the data path is not.
- **Honest failure classification.** Client-caused failures (cancel,
  oversized body) must not be attributed to the upstream.

## Further reading

- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md): request flow and package map
- [docs/ADDING_A_PROVIDER.md](docs/ADDING_A_PROVIDER.md): worked example for a new provider
- [docs/BENCHMARKS.md](docs/BENCHMARKS.md), [docs/PRICING.md](docs/PRICING.md),
  [docs/SEMCONV-COMPLIANCE.md](docs/SEMCONV-COMPLIANCE.md),
  [docs/adr/](docs/adr/)
- [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md)

## Security issues

Do **not** open a public issue or PR for a vulnerability. See
[SECURITY.md](SECURITY.md) for the private reporting path.

## Releases

Maintainers cut releases by pushing a `v*` tag. The release pipeline
produces signed binaries, a signed container image, SLSA-3 provenance,
SBOMs, and a checksums file; nothing is published if provenance
generation fails.
