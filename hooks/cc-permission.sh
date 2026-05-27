#!/bin/bash
# cc-permission.sh — Claude Code PermissionRequest hook for cc-connect
#
# Configure in Claude Code settings.json:
#   "hooks": { "PermissionRequest": [{ "type": "command", "command": "/path/to/cc-permission.sh" }] }
#
# Environment:
#   CC_CONNECT_URL   — cc-connect webhook URL (default: http://localhost:9111/hook)
#   CC_CONNECT_TOKEN — authentication token
#
# Exit codes:
#   0 — decision returned (stdout contains JSON for Claude Code)
#   1 — fallback to terminal interactive mode
#
# Requires: jq, curl

set -euo pipefail

INPUT=$(cat)

CC_URL="${CC_CONNECT_URL:-http://localhost:9111/hook}/permission"
TIMEOUT="${CC_CONNECT_TIMEOUT:-630}"

AUTH_HEADER=""
if [ -n "${CC_CONNECT_TOKEN:-}" ]; then
  AUTH_HEADER="Authorization: Bearer $CC_CONNECT_TOKEN"
fi

RESP=$(curl -s --max-time "$TIMEOUT" \
  ${AUTH_HEADER:+-H "$AUTH_HEADER"} \
  -H "Content-Type: application/json" \
  -d "$INPUT" \
  "$CC_URL" 2>/dev/null) || exit 1

STATUS=$(echo "$RESP" | jq -r '.status // empty')

if [ "$STATUS" = "resolved" ]; then
  DECISION=$(echo "$RESP" | jq -c '.decision')
  printf '{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":%s}}' "$DECISION"
  exit 0
fi

exit 1
