#!/usr/bin/env bash
#
# sandbox-shell.sh — provision a Daytona sandbox via the Go orchestrator and
# open an interactive shell.
#
# Requires a running Go orchestrator (default: http://localhost:8081).
# Start it with:
#   cd go && ./orchestrator &
#
# Required environment:
#   DAYTONA_API_KEY        Daytona staging key (from .env).
#
# Optional environment:
#   ORCHESTRATOR_URL       Default: http://localhost:8081
#   SANDBOX_IMAGE          Default: ubuntu:24.04
#   USER_ID                Random UUID if unset.
#   AUTO_DESTROY           If "1", delete sandbox on exit.
#
set -euo pipefail

ORCHESTRATOR_URL="${ORCHESTRATOR_URL:-http://localhost:8081}"
SANDBOX_IMAGE="${SANDBOX_IMAGE:-ubuntu:24.04}"
USER_ID="${USER_ID:-$(uuidgen 2>/dev/null || python3 -c 'import uuid; print(uuid.uuid4())')}"
AUTO_DESTROY="${AUTO_DESTROY:-0}"

echo "[sandbox-shell] ORCHESTRATOR_URL=${ORCHESTRATOR_URL}"
echo "[sandbox-shell] SANDBOX_IMAGE=${SANDBOX_IMAGE}"
echo "[sandbox-shell] USER_ID=${USER_ID}"

# 1) Provision sandbox via Go orchestrator HTTP API.
RESPONSE=$(curl -sS -X POST "${ORCHESTRATOR_URL}/sandboxes" \
  -H "Content-Type: application/json" \
  -d "{\"user_id\":\"${USER_ID}\",\"image\":\"${SANDBOX_IMAGE}\"}")

echo "[sandbox-shell] orchestrator response: ${RESPONSE}"
SANDBOX_ID=$(echo "${RESPONSE}" | python3 -c 'import sys,json; print(json.load(sys.stdin)["id"])' 2>/dev/null \
  || echo "${RESPONSE}" | grep -o '"id":"[^"]*"' | cut -d'"' -f4)

if [[ -z "${SANDBOX_ID}" ]]; then
  echo "ERROR: could not extract sandbox id from response" >&2
  exit 1
fi
echo "[sandbox-shell] sandbox.id=${SANDBOX_ID}"

# 2) Open interactive shell via daytona CLI.
echo "[sandbox-shell] entering ${SANDBOX_ID} — type 'exit' to leave"
if command -v daytona >/dev/null 2>&1; then
  daytona sandboxes exec "${SANDBOX_ID}" -- /bin/bash -i || true
else
  echo "[sandbox-shell] WARN: 'daytona' CLI not found on PATH."
  echo "[sandbox-shell] exec into sandbox manually with:"
  echo "    daytona sandboxes exec ${SANDBOX_ID} -- /bin/bash"
fi

# 3) Optional teardown.
if [[ "${AUTO_DESTROY}" == "1" ]]; then
  echo "[sandbox-shell] AUTO_DESTROY=1 — destroying ${SANDBOX_ID}"
  curl -sS -X DELETE "${ORCHESTRATOR_URL}/sandboxes/${SANDBOX_ID}" | cat
else
  echo "[sandbox-shell] sandbox ${SANDBOX_ID} left running (AUTO_DESTROY=0)"
fi
