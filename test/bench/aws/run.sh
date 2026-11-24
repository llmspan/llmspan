#!/usr/bin/env bash
# Runs the benchmark matrix across three hosts (loadgen, proxy, upstream).
#
#   KEY=key.pem LOADGEN=<pub> PROXY=<pub> UPSTREAM=<pub> \
#   PROXY_PRIV=<private ip> UPSTREAM_PRIV=<private ip> ./run.sh results.jsonl
#
# Each host must already have `llmspan` and `bench` (linux binaries) in $HOME.
# Variants: direct (client -> stub), passthru (bare Go ReverseProxy),
# llmspan-sampled (spans sampled out, metrics on), llmspan-default (traces+metrics
# exported to an OTLP sink, content off), llmspan-full (default + content events,
# redaction, conversation header), llmspan-tls (default over TLS 1.3).
set -euo pipefail
OUT=${1:-results.jsonl}; : > "$OUT"
SSH="ssh -i $KEY -o StrictHostKeyChecking=accept-new -o LogLevel=ERROR"
px() { $SSH ubuntu@"$PROXY" "$@"; }
up() { $SSH ubuntu@"$UPSTREAM" "$@"; }
lg() { $SSH ubuntu@"$LOADGEN" "$@"; }
REPS=${REPS:-3}; DUR=${DUR:-20s}; WARM=${WARM:-5s}

up 'pkill -x bench || true; ulimit -n 200000; nohup ./bench stub --listen 0.0.0.0:8090 --latency 50ms --chunks 50 --chunk-interval 20ms --payload-bytes 1024 >stub.log 2>&1 &
    nohup ./bench sink --listen 0.0.0.0:4318 >sink.log 2>&1 & sleep 1; echo up-ok'

cfg() { # name, extra-yaml
cat <<YAML
listen: 0.0.0.0:4000
allow_insecure: true
allow_unauthenticated: true
upstreams:
  - name: openai
    prefix: /v1
    target: http://$UPSTREAM_PRIV:8090
    provider: openai
    max_in_flight: 20000
    breaker:
      failures: 0
telemetry:
  endpoint: $UPSTREAM_PRIV:4318
  protocol: http
  insecure: true
  acknowledge_insecure: true
$2
request:
  max_buffered_bodies: 20000
YAML
}

start_proxy() { # variant
  px 'pkill -x llmspan || true; pkill -x bench || true; sleep 0.5' || true
  case "$1" in
    direct) return ;;
    passthru) px "ulimit -n 200000; nohup ./bench passthru --listen 0.0.0.0:4000 --target http://$UPSTREAM_PRIV:8090 >pt.log 2>&1 & sleep 1" ;;
    llmspan-sampled) cfg x "  sample_ratio: 0" | px 'cat > cfg.yaml'
        px 'ulimit -n 200000; nohup ./llmspan up --config cfg.yaml >lt.log 2>&1 & sleep 2' ;;
    llmspan-default) cfg x "  sample_ratio: 1.0" | px 'cat > cfg.yaml'
        px 'ulimit -n 200000; nohup ./llmspan up --config cfg.yaml >lt.log 2>&1 & sleep 2' ;;
    llmspan-full) { cfg x "  sample_ratio: 1.0"; printf 'content:\n  mode: events\n  redact: default\nattribution:\n  conversation_header: X-Conversation-Id\n  grafana_agent_o11y: true\n'; } | px 'cat > cfg.yaml'
        px 'ulimit -n 200000; nohup ./llmspan up --config cfg.yaml >lt.log 2>&1 & sleep 2' ;;
    llmspan-tls) px "test -f tls.crt || ./llmspan gen-cert --out-dir . --host localhost >/dev/null 2>&1"
        { cfg x "  sample_ratio: 1.0" | sed '/allow_insecure/d'; printf 'tls:\n  cert_file: /home/ubuntu/tls.crt\n  key_file: /home/ubuntu/tls.key\n'; } | px 'cat > cfg.yaml'
        px 'ulimit -n 200000; nohup ./llmspan up --config cfg.yaml >lt.log 2>&1 & sleep 2' ;;
  esac
}

for variant in direct passthru llmspan-sampled llmspan-default llmspan-full llmspan-tls; do
  start_proxy "$variant"
  case "$variant" in
    direct) base="http://$UPSTREAM_PRIV:8090" ;;
    llmspan-tls) base="https://$PROXY_PRIV:4000"; INS=--insecure ;;
    *) base="http://$PROXY_PRIV:4000" ;;
  esac
  INS=${INS:-}; [ "$variant" = llmspan-tls ] || INS=""
  for scenario in "json 500" "json 2000" "json 5000" "stream 200" "stream 1000"; do
    kind=${scenario% *}; rate=${scenario#* }
    flag=""; [ "$kind" = stream ] && flag="--stream"
    for rep in $(seq 1 "$REPS"); do
      pid=$(px 'pgrep -x "llmspan|bench" | head -1' || true)
      t0=""; [ -n "$pid" ] && t0=$(px "awk '{print \$14+\$15}' /proc/$pid/stat")
      line=$(lg "ulimit -n 200000; ./bench load --url $base/v1/chat/completions --rate $rate --duration $DUR --warmup $WARM $flag $INS --label $variant/$kind/$rate/rep$rep")
      cpu=null; rss=null
      if [ -n "$pid" ]; then
        t1=$(px "awk '{print \$14+\$15}' /proc/$pid/stat"); hz=100
        cpu=$(awk -v a="$t0" -v b="$t1" -v hz=$hz 'BEGIN{printf "%.2f",(b-a)/hz}')
        rss=$(px "awk '/VmHWM/{print \$2}' /proc/$pid/status")
      fi
      echo "$line" | sed "s/}\$/,\"variant\":\"$variant\",\"proxy_cpu_s\":$cpu,\"proxy_hwm_kb\":$rss}/" | tee -a "$OUT"
      sleep 2
    done
  done
done
px 'pkill -x llmspan || true; pkill -x bench || true'; up 'pkill -x bench || true'
