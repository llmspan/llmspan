#!/bin/sh
# Real traffic for `make demo-real`: four named callers send varied prompts to a
# local llama3.2:1b through llmspan, a mix of streaming and non-streaming, with
# a few multi-turn conversations. Token counts and latencies are whatever the
# model actually does; nothing here is synthesized.
set -eu
apk add --no-cache curl > /dev/null
url="http://llmspan:4000/v1/chat/completions"

# call <caller> <conversation> <stream:true|false> <prompt>
call() {
  curl -s -o /dev/null "$url" \
    -H 'Content-Type: application/json' \
    -H "X-Caller: $1" -H "X-Conversation-Id: $2" \
    -d "{\"model\":\"llama3.2:1b\",\"stream\":$3,\"stream_options\":{\"include_usage\":true},\"max_tokens\":120,\"messages\":[{\"role\":\"user\",\"content\":\"$4\"}]}" || true
}

i=0
while true; do
  i=$((i + 1))
  case $((i % 6)) in
    0) call team-search  "search-$((i % 4))"  false "Rewrite this search query to be more specific: cheap flights to lisbon" ;;
    1) call team-support "support-$((i % 3))" true  "A customer says their invoice shows a duplicate charge. Draft a short, polite reply." ;;
    2) call batch-jobs   "batch-1"            false "Summarize in two sentences: OpenTelemetry is a vendor-neutral set of APIs and SDKs for traces, metrics and logs." ;;
    3) call research-agents "research-$((i % 5))" true "List three risks of putting a proxy in front of an LLM API." ;;
    4) call team-search  "search-$((i % 4))"  true  "Suggest five keywords for a blog post about observability." ;;
    5) call team-support "support-$((i % 3))" false "Explain in one paragraph what a circuit breaker does." ;;
  esac
  sleep 2
done
