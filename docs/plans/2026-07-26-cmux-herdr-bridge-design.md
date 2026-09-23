# cc-connect ⇄ cmux + herdr Deep Integration — Final Design

Date: 2026-07-26. Status: approved for implementation.
Provenance: adversarial pipeline — Codex full design (127KB, compile-verified) ⨯ Opus
cross-examination (86KB, 11 BLOCKER / 17 MAJOR / 9 MINOR, verdict "REDESIGN scoped to the
cmux event/feed layer"). This document is the synthesis: it adopts the surviving
architecture and REPLACES every finding-affected mechanism with the corrected one. Where
this doc and the two source artifacts disagree, THIS DOC WINS.

## 1. Goal & UX

Feishu becomes a first-class remote control room for coding agents running in cmux GUI
workspaces and herdr panes. One Feishu group per external workspace (auto-created).

CUJs:
1. Approve: phone card "⛔ fix-auth: claude wants Bash(npm test)" → tap 允许一次/总是/拒绝
   → decision lands in cmux Feed via `feed.permission.reply` (real approval, not key
   simulation) → agent proceeds; if someone answers in the cmux UI instead, the Feishu
   card updates to "已在 cmux 处理" and the turn continues.
2. herdr blocked: claude stuck on a TUI menu → Feishu card with the screen tail + option
   buttons (existing AskUserQuestion card) → tap → `agent.send_keys` unblocks it.
3. Visibility: in-turn incremental output via the existing streaming preview
   (EventText increments → Feishu card patch); end-of-turn summary; on-demand
   /terminal keyboard card + PNG screenshot + web terminal (all pre-existing).
4. Steering: messages in a workspace's group go to THAT workspace
   (`agent.prompt` / cmux `send`); /watch card lists every workspace with status icons.

## 2. Architecture (survives critique; corrections inlined)

**Asymmetric transports — deliberately NOT one shared reader shape:**
- **herdr**: per-session dedicated `events.subscribe` connection (subscriptions are fixed
  per connection; `pane.agent_status_changed` is per-pane only — a shared subscription
  would need teardown+resubscribe blackouts whenever any session starts/stops).
  `agent.wait` long-poll is both the config-disabled path and the automatic in-turn
  degradation target. Turn truth: `agent_status` transitions + screen-stability fallback.
- **cmux**: ONE process-lifetime `events.stream` reader per socket (categories are global;
  Feed's 120s hook budget makes sub-second delivery matter; durable `(seq, boot_id)`
  cursor). The stream connection NEVER answers RPCs (verified) — RPC uses separate
  connections. **The eventBus is a singleton keyed by socket path** (E1), NOT per-Agent:
  multi-workspace agent cloning must share it.
- **The cmux stream is a TRIGGER, not a state carrier** (A2/A3/A4 — verified on 968 real
  events): frame envelope is `{seq, name, category, workspace_id, surface_id, source,
  occurred_at, payload}` — field `payload` (NOT `data`) contains RAW agent-hook JSON with
  NO item id / request_id / kind / status. And `feed.item.completed` fires ~1ms after
  `received` (hook ack), NOT at decision time. Therefore: any `category=="feed"` frame
  merely debounces a `feed.list` refetch; pending/resolved state comes from DIFFING
  successive `feed.list` snapshots by `item.id`. `feed.item.resolved` (exists; also just
  a trigger) accelerates the refetch. Never decode feed state from frames.
- **Correlation** (B3): feed `workstream_id` is `<agent>-<agent-session-uuid>`, not a
  workspace UUID; permission items carry no session_id/cwd. Join chain:
  `workstream_id → ~/.cmuxterm/{claude,codex}-hook-sessions.json
  (activeSessionsByWorkspace: workspaceUUID → sessionId) → workspaceUUID → cmuxSession`.
  `agent.hook.*` frames route on envelope `workspace_id` directly. Unmatched → slog.Warn
  with tried keys, never dropped silently.
- **Permission truth**: cmux = Feed write-back (`feed.permission.reply {request_id, mode}`,
  mode ∈ once|always|all|bypass|deny — probed). herdr = blocked-status → synthesized
  `EventPermissionRequest{ToolName:"AskUserQuestion", Questions}` reusing the engine's
  existing card + answer path VERBATIM (engine.go:12444 sendAskQuestionPrompt,
  :3564 resolveAskQuestionAnswer, :3480 buildAskQuestionResponse →
  RespondPermission{Behavior:"allow", UpdatedInput}) → adapter maps the picked option to
  `agent.send_keys{target, keys}` (target-addressed — NO pane_id resolution; D2).
- **herdr write path**: `agent.prompt{target, text}`. `agent.send` DOES NOT EXIST in
  protocol 17 (verified live: "unknown variant") — the current shipped adapter's Send()
  is broken against herdr ≥0.7.x; this design fixes it as a bug fix with regression test.
- **Socket discovery (cmux, A1)**: option `socket_path` → `$CMUX_SOCKET_PATH` →
  `~/.local/state/cmux/last-socket-path` (file contains the real path, e.g.
  `cmux-502.sock`) → `~/.local/state/cmux/cmux-<uid>.sock` → legacy
  `~/.config/cmux/cmux.sock`; each candidate validated by dial+`ping`, never bare Stat.

## 3. Core extensions (generic — no platform/agent names in core)

1. `EventPermissionResolved EventType = "permission_resolved"` (message.go) — fields
   reused: RequestID, Content (note). Emitted by adapters when a pending request was
   decided outside cc-connect (cmux UI click, feed timeout, user typed in the pane).
2. `ExternalResolutionNotifier` (interfaces.go) — optional AgentSession capability:
   ```go
   // OnExternalResolution registers notify for a pending request. The session
   // invokes notify(note) at most once if requestID is decided outside
   // cc-connect. The returned cancel deregisters (idempotent, never nil).
   OnExternalResolution(requestID string, notify func(note string)) (cancel func())
   ```
   Engine wiring (C1 — the existing `<-pending.Resolved` wait is NOT rewritten, no event
   draining): after sending the permission prompt, if the session implements the
   capability, register notify = {update card/note via existing reply path; clear
   `state.pending` under the interactive-key locks; unfreeze the stream preview (C3);
   `pending.resolve()`}; `defer cancel()`. Plus an explicit
   `case EventPermissionResolved:` in the interactive event switch for observability (C2).
3. **Background permission policy**: for sessions implementing
   ExternalResolutionNotifier the background handler (engine.go:4769) MUST NOT auto-deny
   (an auto-deny would write a real DENY into the external approval authority). Instead:
   send the permission card to the bound chat; resolution arrives via card click or
   external notify. (pull-adapter "terminalNative" discipline.)
4. **Per-workspace auto-groups** (`SetAutoGroupWorkspaces(interval)`): engine loop
   diffing `agent.ListSessions()`; new external workspace → existing
   `CreateGroupChat → workspaceBindings.Bind → SwitchToAgentSession` sequence
   (executeDirGroup pattern, engine.go:17723). Corrections (C4/C5): dedup on SESSION ID
   (not path) persisted in the binding store; bind BEFORE switch with partial-failure
   retry next tick; goroutine started once (sync.Once), fields mutex-guarded, stops on
   e.ctx.Done(). /watch row button binds on demand as the manual path.
5. i18n: every new user-facing string (blocked-card title/options/descriptions,
   resolved-elsewhere note, feed-timeout note, auto-group announcements, doctor lines)
   gets a MsgKey with EN/ZH/ZH-TW/JA/ES (C7).

## 4. Adapter specs

### 4.1 agent/internal/termdiff (new shared package — sanctioned by AGENTS.md carve-out)
`Normalize(raw string) string` + `ExtractNew(baseline, current string) string` — moved
verbatim from agent/tmux (tests move with them; tmux+herdr both consume; cmux imports).
No `askq` shared package (F4a): the blocked-card builder stays inside agent/herdr.

### 4.2 agent/herdr v2
- client.go: +`agentWait(ctx, target, until []string, timeoutMs)` (one-shot conn,
  arbitrarily long — separate dial timeout from read deadline), +`agentPrompt`,
  fix `agentSendKeys` to target-addressed, DELETE `agent.send` usage + stale comments.
- subscribe.go (new): per-session goroutine owning one `events.subscribe` connection
  (`pane.agent_status_changed{pane_id}` + `pane.exited`); reconnect with backoff; on
  reconnect re-snapshot via `agent.get` and publish status BEFORE resuming frames;
  latest-wins status channel (E5). Falls back to agent.wait loop when disabled/broken.
- session.go: turn watcher consumes status transitions (working/blocked/idle/done);
  blocked → edge-triggered blockedRequest (guard: one active card per turn) →
  EventPermissionRequest w/ Questions (labels MUST NOT parse as integers — D3: use
  "opt-1 ⏎"-style labels or letter prefixes; invariant test); RespondPermission maps
  answer → send_keys, free text → `agent.prompt` (D1); deny → Escape.
  streamOutputLoop: EventText deltas via termdiff, `lastEmitted` seeded from the Send
  baseline (D6), trailing delta emitted in finish(); tickers clamped ≥100ms (E4);
  per-turn goroutines joined via WaitGroup before the next Send (E3).
  Cold blocked recovery: StartSession's "pane exists" branch checks agent_status ==
  blocked and synthesizes the request (critique addendum).
- herdr.go: ListSessions = `agent.list` → AgentSessionInfo{ID: pane name, Summary:
  terminal_title + cwd, ModifiedAt: best-effort}; attach via existing /attach.
- RespondPermission/Events/Close unchanged in shape; teardown sync.Once.

### 4.3 agent/cmux (new)
- client.go: RPC client (one conn per call), socket resolver per §2, optional password.
  Verb param names probed in Phase 0 (workspace list/create, read-screen/send/send-key
  equivalents — CLI verbs confirmed, rpc param shapes captured as fixtures).
- events.go: singleton eventBus per socket path; `events.stream` on a DEDICATED conn;
  ack parsing includes `resume{gap, gap_reason, after_seq}`; `gap := ack.resume.gap ||
  bootID changed`; on gap: `cursor.Seq = resume.AfterSeq` (seq RESTARTS PER BOOT — B2),
  persist, then full `feed.list` resync. Subscribe categories `["feed","agent"]` only
  (B4); cursor persisted atomically on a ticker OUTSIDE the dispatch mutex.
- feed.go: feedBridge holds `known map[itemID]feedItem` + `outstanding map[requestID]`.
  Trigger (any feed frame, debounced ~300ms) → `feed.list` → diff:
  new pending permissionRequest → EventPermissionRequest (dedup against outstanding —
  B5); known-pending now resolved → notify external resolution + EventPermissionResolved
  (carrying decision when present); known-pending ABSENT from a FULL successful listing
  → `missCount++`, treat as resolved at missCount ≥ 2 (B6); failed listing → keep all
  state (never clear). replyPermission: mark-then-delete around the wire call (B7);
  local 120s deadline (`feed_hook_timeout_ms`) with expiry sweeper — after expiry skip
  the wire call and resolve the card into "回落到终端" + keyboard-path hint (D7).
  AskUserQuestion: probe `feed.question.reply{request_id, ...}` shape in Phase 0; if
  selections are supported, route answers there, else permission.reply w/ updated input
  (F5). outstanding purged on session unregister; no close(events)+recover teardown —
  events channel closed only by its owning dispatch goroutine after unregister (E2).
- sessionmap.go: watches `~/.cmuxterm/{claude,codex}-hook-sessions.json` (mtime-based
  reread) → `workstreamID → workspaceUUID`; join failures logged with tried keys.
- session.go: cc session ↔ cmux workspace UUID. Send → agent surface (`send` + Enter);
  StartSession: match existing workspace (by id, then name/cwd fallback after restarts)
  else `new-workspace` when `create_workspaces`; turn end = `agent.hook.Stop` for the
  workspace, screen-stability fallback; EventText deltas from read-screen diffs (D5);
  InjectKey/CaptureBuffer/TerminalAttacher via send-key / read-screen.
- cmux.go: Agent + registry init + WorkspaceAgentOptions; ListSessions from
  workspace list joined with hook-sessions lifecycles; ModifiedAt from feed
  `updated_at` / envelope `occurred_at` where available.
- plugin_agent_cmux.go (`//go:build !no_cmux`), Makefile ALL_AGENTS += cmux,
  config.example.toml section (§6), doctor checks (socket ping, hooks presence,
  Claude-wrapper integration hint).

## 5. Robustness matrix (binding)

| Failure | Behavior |
|---|---|
| cmux/herdr socket down | construction fails fast w/ clear error; runtime: backoff retry, pending state UNTOUCHED, doctor surfaces it |
| cmux restart (boot_id change / seq reset) | gap → cursor reset to resume.AfterSeq → feed.list resync; workspace re-match by name/cwd |
| herdr restart | subscribe reconnect → agent.get re-snapshot before trusting frames; blocked card persists until real state confirms |
| Feed 120s timeout | local deadline sweeper → card degrades to keyboard path; late replies tolerated (delivered:false → log, not crash) |
| Resolved elsewhere vs turn end race | external notify → resolve BEFORE completion handling; feed diff emits resolved before completed-equivalent |
| Duplicate frames/replay | seq ≤ cursor dropped; blocked edge-trigger; resync dedups against outstanding |
| Enhancer read fails | NEVER clears pending/session state (only successful full reads confirm absence) |

## 6. Config surface

cmux options: `socket_path ""`, `password ""`, `events_cursor_file`
(default `~/.cc-connect/cmux_events_cursor.json`), `workspace_filter ""`,
`create_workspaces true`, `work_dir`, `poll_interval_ms 3000`,
`feed_reply_default_mode "once"` (validated against enum),
`feed_hook_timeout_ms 120000`.
herdr additions: `attach_existing true`, `blocked_card true`, `stream_output true`,
`subscribe_events true`.
Engine: `auto_group_workspaces false` + `auto_group_interval_ms 15000` (project-level).

## 7. Implementation slices & gates

Slice A (herdr v2): Phase-0 herdr probes (schema enum fixture; delete stale comments) →
client fixes (agent.prompt/send_keys/agentWait) → subscribe/turn-watcher/blocked flow →
streaming → tests (mock socket incl. streaming subscribe mock; regression
`TestSend_UsesAgentPrompt` for the agent.send bug; blocked-card CUJ additions later).

Slice B (cmux): Phase-0 cmux probes captured into `agent/cmux/testdata/` (one real
feed.item.received/completed/resolved + agent.hook.* frame via jq from
~/.cmuxterm/events.jsonl; events.stream ack with stale after_seq; feed.list snapshot;
bad-value RPC per reply verb + surface/workspace verbs) → client+events+feed+sessionmap+
session per §4.3 → golden-fixture tests (F2) with method-string assertions.

Slice C (core): EventPermissionResolved + ExternalResolutionNotifier + engine wiring
(land `TestEngine_WaitForPermissionResolution_*` regression FIRST — C1) + background
no-auto-deny policy + auto-group loop + /watch row buttons + i18n keys + doctor plumbing.

Gates (owner-run, post-merge of all slices): `go build ./...`, `go test ./...`,
`go test ./core/ -run TestCUJ`, `go vet ./...`; new CUJs:
`TestCUJ_CMUX1_PermissionApproveViaCard`, `TestCUJ_CMUX2_ResolvedElsewhereUpdatesCard`,
`TestCUJ_HERDR1_BlockedCardUnblocksViaButtons`, `TestCUJ_WSGROUP1_AutoGroupPerWorkspace`
(all: real SessionManager+Engine, stub platform/agent, ≥3 real user actions — F3).

## 8. Residual dev-time probes (folded into slice Phase-0 steps)
cmux rpc param names for workspace/surface verbs; feed.question.reply / exit_plan.reply
full shapes; `feed.list` scale behavior (items cap); herdr agent.prompt vs typed-Enter
nuance for multi-line prompts; `surface.read_text` base64 contract.
