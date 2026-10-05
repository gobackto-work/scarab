#!/usr/bin/env bash
#
# Start a worker in an existing workspace, which is what produces run events.
#
# Why this is a script and not a curl line: the broker authenticates with the workspace
# capability token, which is a Secret in the tenant namespace, and it serves TLS with its own
# CA. Two steps that are easy to get wrong once and impossible to remember later.
#
# Requirements
#   - kubectl configured against the cluster
#   - a workspace that already exists
#
# Usage
#   cluster-setup-scripts/spawn-worker.sh [slug] [task]
#   SLUG=amber-shrew-uucs cluster-setup-scripts/spawn-worker.sh
#
# Notes
#   - the agent id this prints IS the run id in the record. That is deliberate: a parent
#     already holds it from agents_spawn, so nothing has to be plumbed to join the two.
#   - a worker with no model credential fails fast. It still records a start and an end,
#     because the broker asserts the start it knows happened rather than waiting to observe
#     a phase it might miss.
#   - -k is correct here. The broker's certificate is its own CA, and this is a poke.
set -euo pipefail

SLUG="${SLUG:-${1:-amber-shrew-uucs}}"
TASK="${TASK:-${2:-say hello and stop}}"
LOCAL_PORT="${LOCAL_PORT:-18090}"

TOKEN="$(kubectl -n "ws-$SLUG" get secret pi-root-token -o jsonpath='{.data.token}' | base64 -d)"

kubectl -n scarab port-forward "svc/broker-$SLUG" "$LOCAL_PORT:8443" >/dev/null 2>&1 &
PF=$!
trap 'kill $PF 2>/dev/null || true' EXIT
sleep 4

curl -sSk -X POST -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d "$(python3 -c 'import json,sys;print(json.dumps({"task":sys.argv[1]}))' "$TASK")" \
  "https://127.0.0.1:$LOCAL_PORT/agents"
echo
