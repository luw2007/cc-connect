# cmux Phase-0 probes

Date: 2026-07-26 (initial capture) + 2026-07-27 (live re-verification, this
run). cmux CLI: `0.64.20 (100) [14e3400b9]`, app `/Applications/cmux.app`.

The four `events_*.jsonl` fixtures were copied from the real
`~/.cmuxterm/events.jsonl` audit stream. User/cwd values and UUIDs were
minimally redacted. They confirm the envelope uses `payload` (not `data`) and
that `feed.item.completed` follows `feed.item.received` by 1 ms with only an
`acknowledged` result; it is not a permission resolution. `feed.item.resolved`
is present and is also treated only as a refetch trigger.

`events_stream_ack.json` is a previously captured real two-frame stale-cursor
probe (`events.stream`, `after_seq:1`). It confirms top-level `resume.gap`,
`gap_reason`, and the authoritative `resume.after_seq` reset point.

`feed_list.json` is a redacted excerpt retaining the observed real wire types:
`tool_input` is a JSON-encoded string; permission state and decision live only
in `feed.list`. `claude-hook-sessions.json` preserves the real top-level and
mapping shapes used for workstream correlation.

## RPC method names: CLI verbs != wire method names (CORRECTED 2026-07-27)

The prior capture of this file asserted the bare-socket RPC method names were
the cmux CLI's kebab-case subcommand names (`ping`, `list-workspaces`,
`new-workspace`, `read-screen`, `send`, `send-key`) and noted live validation
was blocked by `EPERM` in that run's sandbox. **This was wrong and has now
been corrected against the live daemon** (this run's sandbox *can* reach the
socket at `~/.local/state/cmux/cmux-502.sock`, confirmed via `cmux ping` →
`PONG`).

`cmux rpc <method> <params>` sends the method string directly over the raw
v2 socket protocol — it does **not** translate CLI subcommand names. Proof
(bare-socket + `cmux rpc`, both real, read-only):

- `` `ping` `` → `Error: method_not_found: Unknown method`. `` `system.ping` ``
  → `{"pong":true}`.
- `` `list-workspaces` `` → `method_not_found`. `` `workspace.list` `` → real
  workspace list (see below).
- `system.capabilities` (real, read-only) enumerates every valid method; it
  lists `workspace.list`, `workspace.create`, `surface.read_text`,
  `surface.send_text`, `surface.send_key`, `system.ping`, `feed.list`,
  `feed.permission.reply`, `feed.question.reply`, `feed.exit_plan.reply` —
  and does **not** list any bare `ping`/`list-workspaces`/`read-screen`/
  `send`/`send-key`. It also does not list `` `events.stream` ``, which is
  a streaming (non-request/response) method excluded from the capabilities
  enumeration; see below for its independent confirmation.
- The four `feed.*` method names and `` `events.stream` `` in the design
  were already dotted and are unaffected by this correction.

**Corrected/pinned wire method names actually used by client.go:**
`` `system.ping` ``, `` `events.stream` ``, `` `feed.list` ``,
`` `feed.permission.reply` ``, `` `feed.question.reply` ``,
`` `feed.exit_plan.reply` ``, `` `workspace.list` ``, `` `workspace.create` ``,
`` `surface.read_text` ``, `` `surface.send_text` ``, `` `surface.send_key` ``.

`` `events.stream` `` was independently confirmed over a raw Python socket
(bypassing `cmux rpc`'s one-shot response parser, which cannot consume a
multi-frame streaming reply and reports a generic `v2 request failed` for
any streaming method — that error is not a "wrong method name" signal): a
literal `{"id":"probe","method":"events.stream","params":{"after_seq":
999999999,"categories":["feed","agent"]}}\n` line got a well-formed `ack`
frame back (`type:"ack"`, `boot_id`, `subscription_id`,
`heartbeat_interval_seconds:15`, `resume:{after_seq,gap,gap_reason,
oldest_seq,latest_seq,next_seq,requested_after_seq}`), matching
`streamAck`/`streamResume` in events.go exactly, including a *second*,
previously-uncaptured `gap_reason` variant for "requested sequence is newer
than this cmux process; cmux probably restarted" (the existing
`events_stream_ack.json` fixture already covers the "too old" variant, which
is the one that matters for `TestGapResetsCursorSeq`).

## Response shape corrections (live, read-only + one bad-value probe)

- `workspace.list` per-item real shape has **no** `name` or `cwd` field —
  the real fields are `title` and `current_directory` (confirmed against a
  real, live workspace: `{"id":"...", "title":"π ⠧ ...", "current_directory":
  "/Users/.../cc-connect", ...}`). `workspaceInfo`'s JSON tags were fixed to
  match (`Name string \`json:"title"\``, `CWD string \`json:"current_directory"\``).
  Golden fixture: `workspace_list_real.json`.
- `workspace.create`'s response is a **flat, different** shape from
  `workspace.list` items — `{"group_id","group_ref","surface_id",
  "surface_ref","window_id","window_ref","workspace_id","workspace_ref"}` —
  and never echoes back `name`/`cwd`/`title`. There is no `id` field (it's
  `workspace_id`) and no `{"workspace": {...}}` wrapper. `newWorkspace()` was
  rewritten to parse this real shape and build the returned `workspaceInfo`
  from the request's own `name`/`cwd` plus the response's `workspace_id`/
  `surface_id`. Golden fixture: `workspace_create_result.json`.

  **Caveat on how this fixture was captured**: the first two attempts to
  probe `workspace.create` used `{}` and `{"name":123}` expecting a
  validation error (the way `feed.permission.reply`'s bad-`mode` probe safely
  errors before any effect). Unlike the feed reply verbs, `workspace.create`
  does **not** validate before acting — both calls silently created a real,
  empty workspace using defaulted values. Both were immediately closed via
  `workspace.close {workspace_id}` (itself confirmed real/dotted) to restore
  state. No further probing of any RPC method with unconfirmed mutation
  semantics was performed after this; `surface.send_text`/`surface.send_key`
  were *never* invoked live (see below) specifically because of this
  lesson learned.
- `surface.read_text` response is `{"workspace_id","surface_id","text",
  "base64","window_id","window_ref","surface_ref","window_ref"}` — a plain
  `text` field is present alongside `base64` (confirmed byte-for-byte equal
  to `base64.b64decode(base64)` on a real pane) — so `readScreen()`'s
  existing `wrapped.Text` fallback already parses it correctly; no fix
  needed there.
  **`surface_id` is required for reliable targeting.** Passing only
  `workspace_id` (even a bogus one) does **not** scope or error — the server
  silently falls back to some ambient/current surface unrelated to the
  requested workspace (observed: it returned a *different* real user's
  terminal-pane content, not this session's own). Passing a real
  `workspace_id` with a bogus `surface_id` correctly errors: `{"ok":false,
  "error":{"code":"not_found","message":"Surface not found for the given
  surface_id"}}`. `surface_id` alone (no `workspace_id`) returns identical
  content to `workspace_id`+`surface_id` together, confirming `surface_id` is
  the effective identifier. **Consequence**: `cmux.go`'s `StartSession` now
  fails fast with a clear error when no `surface_id` can be resolved (via
  the create response or the hook-session mapper) instead of constructing a
  session that would silently read/act on an unrelated pane.
- `feed.list` real shape (read-only re-probe) matches `feedItem` exactly:
  `{cwd, workstream_id, source, updated_at, kind, created_at, tool_input
  (JSON-encoded string), id, tool_name, title, status}` for `toolUse`/
  telemetry items; no live pending `permissionRequest` item was available to
  re-probe `request_id`/`decision`/`resolved_at` this run, so those fields
  rely on the dated 2026-07-26 capture already in `feed_list.json` /
  `feed_list_resolved.json`.

## RPC validation (bad-value) probes — safe, no real state touched

- `feed.permission.reply` with `{"request_id":"__cc_probe_bogus__","mode":
  "bogus_mode_xyz"}` → `Error: invalid_params: feed.permission.reply requires
  mode ∈ once|always|all|bypass|deny` (validates before touching state; the
  nonexistent request_id was never reached).
- `feed.question.reply` with `{"request_id":"__cc_probe_bogus__"}` (missing
  selections) → `Error: invalid_params: feed.question.reply requires
  selections: [string]`.
- `feed.exit_plan.reply` with `{"request_id":"__cc_probe_bogus__","mode":
  "bogus_mode_xyz"}` → `Error: invalid_params: feed.exit_plan.reply requires
  mode ∈ ultraplan|bypassPermissions|autoAccept|manual|deny` (not used by
  this slice; pinned for completeness — note this differs from the earlier
  static-analysis guess of `Ultraplan/Manual/Auto/Deny`, now corrected to the
  real live-enumerated values).
- `list-workspaces`/`new-workspace`/`read-screen`/`send`/`send-key`: real
  CLI subcommands exist with these exact kebab-case names (confirmed via
  `cmux --help`), but they are **CLI-level** names, not raw RPC method
  names — see the correction above.

`surface.send_text` / `surface.send_key` were **never invoked**, live or
bad-value — the task forbids sending text/keys to live panes, and given
`workspace.create`'s surprising lack of pre-mutation validation there was no
safe way to bad-value-probe them without risk. Their method names are taken
directly from `system.capabilities`'s real enumeration (same `surface.<verb>`
family as the independently-confirmed `surface.read_text`), and their param
shapes (`workspace_id`, `surface_id`, `text`/`key`) follow the symmetric
naming already confirmed for `surface.read_text`'s response (`text`) and the
CLI's positional `send <text>` / `send-key <key>` flags.

No probe sent text, keys, focus, or replies to live feed items. Two
`workspace.create` bad-value probes did create real empty workspaces as a
side effect (see above); both were closed immediately via `workspace.close`.
