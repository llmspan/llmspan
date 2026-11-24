#!/usr/bin/env bash
# Overhead against a REAL model (local Ollama), not a stub. Alternates direct
# and via-llmspan rounds (A/B/A/B) so machine drift hits both equally, with a
# fixed max_tokens so response length, and so latency, is comparable.
#
#   ollama serve &  ollama pull llama3.2:1b
#   ../../llmspan (built) and ./bench (go build -o bench .) must exist
#   ./real-model.sh [rounds] [seconds-per-round]
set -euo pipefail
cd "$(dirname "$0")"
ROUNDS=${1:-4}; DUR=${2:-40}
LLMSPAN=${LLMSPAN:-../../llmspan}; BENCH=${BENCH:-./bench}
W=$(mktemp -d); trap 'kill $(jobs -p) 2>/dev/null || true; rm -rf "$W"' EXIT
$BENCH sink --listen 127.0.0.1:14318 >/dev/null 2>&1 &
cat > "$W/cfg.yaml" <<YAML
listen: 127.0.0.1:14200
upstreams:
  - {name: openai, prefix: /v1, target: "http://127.0.0.1:11434", provider: openai}
telemetry: {endpoint: "127.0.0.1:14318", protocol: http, insecure: true}
YAML
$LLMSPAN up --config "$W/cfg.yaml" >"$W/up.log" 2>&1 &
sleep 2
args=(--model llama3.2:1b --max-tokens 48 --rate 0.5 --duration "${DUR}s" --warmup 4s
      --prompt "Explain in two sentences what a reverse proxy does.")
# warm the model so the first sample is not a cold load
curl -s -o /dev/null localhost:11434/v1/chat/completions -d '{"model":"llama3.2:1b","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}'
for r in $(seq 1 "$ROUNDS"); do
  $BENCH load --url http://127.0.0.1:11434/v1/chat/completions "${args[@]}" --label "direct/round$r"
  $BENCH load --url http://127.0.0.1:14200/v1/chat/completions "${args[@]}" --label "llmspan/round$r"
done
