#!/bin/sh
# Synthetic traffic for the keyless demo. Four named callers (X-Caller) each
# favour different models, mix in a streaming call, and rotate conversation
# ids, so the dashboard's per-caller / per-model / conversation views fill in.
# The mock upstream returns fixed token counts and never fails; nothing here
# fakes errors.
set -eu
apk add --no-cache curl > /dev/null
url="http://llmspan:4000/v1/chat/completions"

# call <caller> <conversation> <model> [stream]
call() {
  stream=false
  [ "${4:-}" = stream ] && stream=true
  curl -s -o /dev/null "$url" \
    -H 'Content-Type: application/json' \
    -H "X-Caller: $1" \
    -H "X-Conversation-Id: $2" \
    -d "{\"model\":\"$3\",\"stream\":$stream,\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}" || true
}

i=0
while true; do
  i=$((i + 1))
  conv="demo-$((i % 5))"
  # team-search: cheap and chatty
  call team-search "search-$((i % 4))" gpt-5-mini
  call team-search "search-$((i % 4))" gpt-5-mini stream
  # team-support: mid-tier, streaming
  call team-support "support-$((i % 3))" gpt-4.1
  call team-support "support-$((i % 3))" gpt-4.1 stream
  # batch-jobs: heavy reasoning models, every other tick
  if [ $((i % 2)) -eq 0 ]; then
    call batch-jobs "batch-$((i % 2))" gpt-5.4-mini
    call batch-jobs "batch-$((i % 2))" gpt-5
  fi
  # research-agents: occasional expensive calls
  if [ $((i % 3)) -eq 0 ]; then
    call research-agents "$conv" gpt-5
    call research-agents "$conv" gpt-5-mini stream
  fi
  sleep 1
done
