#!/bin/bash
# cc-notify.sh — Claude Code Stop/UserPromptSubmit hook for cc-connect
#
# Sends a fire-and-forget notification to cc-connect.
# Always exits 0 (never blocks Claude Code).
#
# Configure in Claude Code settings.json:
#   "hooks": { "Stop": [{ "type": "command", "command": "/path/to/cc-notify.sh" }] }
#
# Environment:
#   CC_CONNECT_URL   — cc-connect webhook URL (default: http://localhost:9111/hook)
#   CC_CONNECT_TOKEN — authentication token

INPUT=$(cat)

CC_URL="${CC_CONNECT_URL:-http://localhost:9111/hook}/notify"

AUTH_HEADER=""
if [ -n "${CC_CONNECT_TOKEN:-}" ]; then
  AUTH_HEADER="Authorization: Bearer $CC_CONNECT_TOKEN"
fi

curl -s --max-time 5 \
  ${AUTH_HEADER:+-H "$AUTH_HEADER"} \
  -H "Content-Type: application/json" \
  -d "$INPUT" \
  "$CC_URL" >/dev/null 2>&1 &

exit 0
