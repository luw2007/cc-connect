# Implementation notes — cmux/herdr bridge

## Slice A (herdr adapter v2) — HerdrDev2, resumed session, 2026-07-27

Resumed after a prior run crashed on an unrelated upstream proxy failure. Inventory found
`agent/herdr/{client,herdr,session,herdr_test}.go` modified, `blocked.go`/`subscribe.go`/
`testdata/` new, `agent/internal/termdiff/` new, `agent/tmux/*` already switched to
termdiff, and Makefile/config.example.toml already carrying the herdr option keys —
essentially all of the design doc's §4.2 spec was already implemented and its 7 named
tests already existed and passed (including under `-race`). This session's actual changes
were narrow:

1. **Core symbol wiring (completion, not a deviation).** The inherited code deliberately
   avoided referencing `core.EventPermissionResolved` / `core.ExternalResolutionNotifier`
   before Slice C landed them (used a `core.EventType("permission_resolved")` string
   literal and a structurally-matching-but-untyped `OnExternalResolution` method so the
   package could compile independently). Once `CoreWiring2` confirmed both symbols landed
   (`core/message.go`, `core/interfaces.go`), swapped both call sites in `session.go` to
   the real `core.EventPermissionResolved` constant, updated the matching test assertion
   in `herdr_test.go`, and added `var _ core.ExternalResolutionNotifier =
   (*herdrSession)(nil)` next to `OnExternalResolution` for a compile-time contract check.
   `go build`/`go vet`/`go test -race` all green afterward for
   `agent/herdr`, `agent/internal/termdiff`, `agent/tmux`.

2. **Protocol-17 creation bug fixed.** The captured schema now records the real two-step
   create contract: `tab.create{cwd?,label?,focus:false}` returns
   `tab_created{tab,root_pane}`, then `agent.start{name,kind,pane_id,args?}` launches a
   recognized agent in that root pane. `argv`, `cwd`, `env`, and `focus` are not valid
   `agent.start` fields. `StartSession` maps the executable basename in `init_command` to
   the CLI-confirmed protocol-17 kind enum and passes remaining whitespace-separated
   words as `args`; an unrecognized executable now returns an attach-only error before
   creating a tab. If `agent.start` fails after `tab.create`, the new tab is closed with
   a fresh bounded cleanup context. `TestStartSession_CreateUsesProtocol17Shape` guards
   the exact wire sequence and shape, and the fixture includes both parameter schemas.

   `herdr api schema --json` and `herdr agent start --help` were re-probed on 2026-07-27
   (`protocol:17`; recognized kinds include `claude`, `codex`, and the full fixture list).
   The requested mutating throwaway probe could not run in this managed session:
   `herdr tab create --cwd /tmp --label cc-connect-protocol17-probe --no-focus` failed
   before creation with `Operation not permitted`, and Computer Use disallowed Terminal.
   Therefore no temporary tab/pane was created or left behind; the live create/start/close
   behavior remains environment-blocked rather than claimed as verified.

3. **`AgentSessionInfo.ModifiedAt` left zero-value in `ListSessions` (herdr.go).** Design
   doc §4.2 says "ModifiedAt: best-effort". herdr's `agent.list`/`agentInfo` schema (own
   client.go struct, cross-checked against the live `herdr api schema --json` dump) has
   no timestamp field of any kind — no `updated_at`, no last-activity marker. The sibling
   `agent/tmux` adapter (closest precedent for a poll-based, non-timestamped backend)
   also leaves `ListSessions` entirely stubbed (`return nil, nil`), so there's no existing
   in-repo convention to reuse either. Zero-value is the genuine best effort here, not an
   oversight; recording the decision per CLAUDE.md §18 rather than silently doing nothing.

### Test status (final, this session)
`go build`, `go vet`, `go test -race -count=1` all pass for `agent/herdr`,
`agent/internal/termdiff`, `agent/tmux`. All 7 named tests from the assignment (mock
streaming subscribe server, `TestSend_UsesAgentPrompt`,
`TestBlockedEmitsPermissionRequest_EdgeTriggered`,
`TestRespondPermission_MapsOptionToSendKeys`, `TestOptionLabelsNeverParseAsInt`,
`TestStreamOutput_SeedsBaseline_NoFullScreenDump`,
`TestSubscribeReconnect_ResnapshotsBeforeFrames`, termdiff tests) present and green.
No core/ edits made by this slice.

## Slice B (cmux adapter) — CmuxDev2, resumed session, 2026-07-27

Resumed after a prior run crashed on an unrelated upstream proxy failure. Inventory found
`agent/cmux/{client,cmux,cmux_test,events,feed,session,sessionmap}.go` +
`testdata/{PROBES.md,+10 fixtures}` + `cmd/cc-connect/plugin_agent_cmux.go` + the
Makefile `ALL_AGENTS` entry + a full `config.example.toml` cmux section already on disk —
essentially all of design §4.3 was already drafted, all 8 named tests already existed, and
the reported blocker (`core.EventPermissionResolved`) was confirmed via `go build` failing
on exactly `feed.go:195,202,341,357: undefined: core.EventPermissionResolved`. Waited for
`CoreWiring2` to land both pinned symbols (confirmed via grep + `go build ./core/...`
turning clean), then found and fixed five additional defects the interrupted run never
reached (it stopped at "starting package tests"), all found through live, read-only (plus
two accidental, immediately-reverted) probes against the running cmux daemon on this
machine — **PROBES.md is rewritten in place with the corrected/superseding findings**
(the previous PROBES.md text claimed live validation was blocked by sandbox `EPERM`; this
session's sandbox could reach the socket).

1. **RPC method names were CLI kebab-case aliases, not real wire methods (BLOCKER,
   fixed).** `client.go`'s method constants used the cmux CLI's subcommand names
   (`ping`, `list-workspaces`, `new-workspace`, `read-screen`, `send`, `send-key`) as the
   raw v2 socket RPC method strings. Live `cmux rpc ping '{}'` →
   `method_not_found: Unknown method`; `cmux rpc system.ping '{}'` → `{"pong":true}`. The
   real, live `system.capabilities` enumeration confirms the dotted forms
   (`system.ping`, `workspace.list`, `workspace.create`, `surface.read_text`,
   `surface.send_text`, `surface.send_key`); the four `feed.*` methods and
   `events.stream` were already correctly dotted. Since `resolveSocket()`'s dial+ping
   validation used the same broken `ping` constant, **every socket candidate would have
   looked unresponsive and `New()` would always fail** — this was a total, silent
   startup blocker against any real daemon, invisible to the mock-server-driven unit
   tests (which are self-consistent against symbolic constants regardless of their
   literal value). Fixed all 6 constants in `client.go`.
2. **`workspace.list`/`workspace.create` response shapes did not match the assumed
   `workspaceInfo` struct (BLOCKER, fixed).** Real `workspace.list` items have no
   `name`/`cwd` fields — the real fields are `title`/`current_directory` (confirmed on a
   real, live workspace). This silently broke the design's "match existing workspace by
   id, then name/cwd fallback" (matchWorkspace's fallback branches always saw empty
   strings) and `ListSessions`'s `Summary`/`ProjectPath`. Separately, `workspace.create`'s
   response is an *entirely different, flat* shape (`{workspace_id, surface_id,
   window_id, ...}` plus `*_ref` fields, no `id`, no `name`/`cwd` echo, no `{"workspace":
   {...}}` wrapper) — `newWorkspace()`'s dual direct/wrapped unmarshal attempts both
   silently extracted nothing, which `StartSession`'s existing `workspace.ID == ""` check
   did catch, but meant `create_workspaces` was 100% broken (always errored) against the
   real daemon. Fixed `workspaceInfo`'s JSON tags and rewrote `newWorkspace()` to parse
   the real flat shape, building the result from the known request values (name, cwd)
   plus the returned `workspace_id`/`surface_id`. Added golden-fixture regression tests
   `TestListWorkspaces_ParsesRealResponseShape` / `TestNewWorkspace_ParsesRealResponseShape`
   (fixtures `workspace_list_real.json` / `workspace_create_result.json`, captured live).
   **How the create-response fixture was captured**: the first two live probes assumed
   `workspace.create` validates params before acting (like `feed.permission.reply`'s
   strict mode-enum check) and sent `{}` then `{"name":123}` expecting a safe validation
   error — instead both silently created a real, empty workspace. Both were immediately
   closed via `workspace.close {workspace_id}` (itself confirmed real). No further live
   calls were made against any method without independently confirmed side-effect-free
   semantics; `surface.send_text`/`surface.send_key` were never invoked, live or
   bad-value, for this reason — their method names/param shapes are taken from
   `system.capabilities`'s real enumeration and the confirmed-symmetric
   `surface.read_text` shape, not live invocation.
3. **`surface_id`-less `surface.read_text` silently targets an unrelated ambient
   surface, not the requested workspace (BLOCKER, fixed in cmux.go).** Live-confirmed:
   `surface.read_text` with only a `workspace_id` (even a bogus one) does not error or
   scope by it — it returns *some* current/ambient surface's content (observed: another
   live agent's unrelated terminal pane, not this session's own), while an explicit
   bogus `surface_id` correctly 404s (`"Surface not found for the given surface_id"`).
   `readScreen()`'s own field parsing needed no fix (`text` is present directly,
   confirmed byte-identical to `base64.b64decode(base64)`), but `StartSession`
   previously allowed an unresolved (`""`) `surfaceID` through to `newCmuxSession`
   whenever `workspace.SurfaceID` was empty (always true for `workspace.list`, per #2)
   and the hook-session mapper had no entry for that workspace (e.g. attaching to a
   workspace with no active agent hook session yet). Added a fail-fast check in
   `cmux.go`: `StartSession` now errors clearly instead of constructing a session that
   would silently read from (and, via `Send`, write to) the wrong pane. New regression
   test `TestStartSession_AttachWithoutSurfaceFails`.
4. **Feed-refetch debounce timer permanently dies on a second trigger inside its 300ms
   window (BLOCKER, fixed in feed.go).** `feed.run()`'s debounce logic was
   `else if !debounce.Stop() { ...; debounce.Reset(...) }` — `Timer.Stop()` returns
   `true` when it cancels a still-pending timer, so the *common* case (a second trigger
   arriving before the first fires) took the `Stop()==true` branch and skipped
   `Reset()` entirely, leaving the timer dead and the debounced `feed.list` refetch never
   firing again for that session. This is not an edge case: PROBES.md's own captured
   fixtures show `feed.item.received` and `feed.item.completed` arriving ~1ms apart in
   real cmux traffic, i.e. **every permission request would trigger exactly this bug in
   production**, silently breaking the stream-is-a-trigger-only refetch mechanism (design
   §2, findings A2/A3/A4) for the whole session after the first double-trigger. Found via
   `TestFeedDiff_CompletedFrameIsNotResolution` (one of the assignment's named tests)
   failing with "permission request not emitted" — confirmed with `-run` in isolation
   (not cross-test leakage) before diagnosing. Fixed to the standard Go idiom: always
   `Reset()`, only conditionally drain the channel.
5. **Two pre-existing test-infra-only bugs blocked `go test` from ever completing,
   independent of the missing core symbols — likely why the interrupted run stopped at
   "starting package tests" (fixed).**
   - `newMockCmuxServer`/`TestEventStream_MockStreamsRawFixtureBytes` built socket paths
     via `filepath.Join(t.TempDir(), "cmux.sock")`; on this machine `t.TempDir()`'s
     per-test nesting (`/var/folders/.../T/<TestName><N>/001/cmux.sock`) is 116+ bytes for
     our longer test names, exceeding macOS's ~104-byte `sockaddr_un.sun_path` limit
     (`bind: invalid argument`). Fixed with a `shortSocketPath(t)` helper using
     `os.MkdirTemp("", "cmux")` (prefix only, no test name in the path).
   - `TestGapResetsCursorSeq` constructs a minimal `&eventBus{cursor: ...}` (deliberately
     skipping `client`, matching the design's dumb-cursor-math unit-test intent) and
     panicked with a nil pointer inside `applyAck`'s gap-logging call
     (`b.client.socketPath`) — `client` is only ever nil in this minimal-construction
     test pattern, never in production (`newEventBus` always sets it). Guarded with a nil
     check before the log call.

None of the five are speculative — all were reproduced (live probe, or a failing/panicking
test) before being fixed, and all are re-verified passing after. Added
`var _ core.ExternalResolutionNotifier = (*cmuxSession)(nil)` next to `OnExternalResolution`
for a compile-time contract check, matching the identical pattern already established in
Slice A's `herdr/session.go` (same interface, same recent-landing risk; `KeyInjector`/
`TerminalBufferProvider` left unasserted to match that same precedent, which only asserts
the notifier interface). No core/ edits made by this slice.

### Test status (final, this session)
`go build`, `go vet`, `go test -race -count=1` all pass for `agent/cmux` (12/12 tests,
stress-run 5x clean). All 8 named tests from the assignment
(`TestFeedDiff_PendingToResolved_EmitsResolved`, `TestFeedDiff_CompletedFrameIsNotResolution`,
`TestGapResetsCursorSeq`, `TestResync_NeverClearsOnError`, `TestResync_DedupsOutstanding`,
`TestCorrelation_WorkstreamToWorkspace`, `TestSocketResolver_LastSocketPath`, plus the
method-string-vs-capabilities-enum assertion `TestMethodStrings_MatchCapabilitiesEnum`)
plus the pre-existing `TestEventStream_MockStreamsRawFixtureBytes` and three new regression
tests for defects #2/#3 above are present and green.

## Slice C (core generic extensions) — CoreWiring2, fresh run, 2026-07-27

Fresh run — `core/` was untouched at start (confirmed via `git status`/`git diff --stat`
before any edit). Landed the two pinned symbols first (`core/message.go`
`EventPermissionResolved`, `core/interfaces.go` `ExternalResolutionNotifier`) and pinged
`CmuxDev2`/`HerdrDev2` the moment `go build ./core/` was clean on just those two edits, per
the batch contract ("land message.go + interfaces.go FIRST so their tests can compile").
Both acknowledged and unblocked before the rest of this slice's work began.

### Mechanism: interactive permission wait (C1/C2/C3)

The pinned contract's `OnExternalResolution(requestID string, notify func(note string))
(cancel func())` is a callback-*registration* API, not critique C1's own suggested fix (a
`PermissionResolvedElsewhere() <-chan Event` requiring the engine to multiplex a second
channel into the wait). That distinction matters: because resolution is delivered by the
adapter *calling into* the engine's closure rather than the engine reading a channel, the
existing `<-pending.Resolved` blocking wait needed **zero changes** — no new `select` arm,
no event draining, so C1's actual defect (discarding buffered events during the wait) and
C2's (dropping `stopCh` while spliced into a rewritten wait) cannot occur, because nothing
was rewritten. Implementation: right after the permission/question prompt is sent, if
`state.agentSession` implements `ExternalResolutionNotifier`, register a closure and
`defer cancelExternal()` in the same `case EventPermissionRequest:` block (defer is
function-scoped in Go, so it fires on every return path of `processInteractiveEvents` —
completion, `/stop`, idle/max-turn-time timeout, error — covering "every turn exit path").
The closure: under `state.mu`, clears `state.pending` only if it is still the exact same
`*pendingPermission` for this `RequestID` (guards against a real user reply, or a later
permission in the same turn, already having moved past it), sends an i18n note only in
that case, then unconditionally calls `pending.resolve()` (`sync.Once`-guarded, so a
notify racing a real reply is a safe no-op either way). `sp.unfreeze()` is deliberately
**not** duplicated inside the closure — study of `handlePendingPermission` confirmed the
single existing post-wait `sp.unfreeze()` already fires unconditionally once
`<-pending.Resolved` returns, regardless of *which* caller resolved it, so adding a second
call would only be redundant, not more correct.

Per C3's explicit ask, also added `case EventPermissionResolved:` as its own arm in the
interactive switch (sibling to `case EventPermissionRequest:`, not nested inside its wait)
for observability/defense: because `processInteractiveEvents` is single-threaded and
blocks synchronously on `<-pending.Resolved` the instant a permission becomes pending, this
arm can only ever observe a request that is *already* resolved by the time control returns
to the outer `select` — never the live wait itself. It uses the identical idempotent
guard (match-then-clear-then-resolve) so a backend that sends **both** the notify callback
and an `EventPermissionResolved` event for the same request is safe (second signal is a
no-op), matching the explicit "notify+event double-fire is safe" requirement.

### Mechanism: background permission policy (§3.3, engine.go ~4769)

`runUnsolicitedReader`'s `EventPermissionRequest` case now checks
`agentSession.(ExternalResolutionNotifier)` before the auto-deny branch (guarded by
`!autoApprove` first, so `/yolo` allow-all is checked and taken exactly as before — the new
branch is unreachable when `approveAll` is true, preserving that behavior byte-for-byte).
When it matches, control passes to a new `handleBackgroundExternalPermission`: installs
`state.pending` the same shape the interactive path uses (so a later chat reply / card tap
resolves it through the ordinary `handlePendingPermission` path — no new resolution
mechanism), sends the card via `sendExternalPermissionPrompt` (reused verbatim — it already
exists specifically to avoid re-emitting `HookEventPermissionRequested`), registers the
notifier, and spawns one dedicated goroutine that just waits on
`pending.Resolved`/`ctx.Done()` then calls `cancel()` — kept out of the reader's own hot
loop deliberately, since that loop's own doc comment already establishes "keep reader
iterations fast" as a hard constraint (`stopUnsolicitedReader` bounded-waits on it).

### Mechanism: auto-group dedup + partial-failure recovery (C4/C5)

`WorkspaceBinding` gained two fields: `AgentSessionID` (dedup key) and `Activated` (bool).
`autoGroupEnsure` splits into two phases so a duplicate group is structurally impossible:
phase 1 (only when `LookupBySessionID` finds nothing) calls `CreateGroupChat` then
*immediately* `BindSession` — this is the "record chatID" step, and it happens before any
step that can fail, so a crash/error afterward still leaves the session dedup-safe on the
next sweep. Phase 2 (runs whenever a binding exists but `!Activated`) does
`SwitchToAgentSession` → `ReconstructReplyCtx` → announce → `MarkActivated`; any failure in
phase 2 leaves `Activated=false` and simply retries phase 2 only, next sweep, using the
already-recorded chat — `CreateGroupChat` is never called a second time for the same
session ID. Regression test `TestAutoGroup_DedupsAcrossTicks_AndRecoversPartialFailure`
drives exactly this: sweep 1 (create succeeds, `ReconstructReplyCtx` fails) → binding exists
but unactivated, zero announcements; sweep 2 (still failing) → `CreateGroupChat` call count
stays at 1; sweep 3 (failure cleared) → activates, exactly one announcement; sweep 4 → no
second announcement. A second test, `TestAutoGroup_TwoWorkspacesSameRepoGetTwoGroups`,
guards the actual C4 defect directly (two sessions sharing one `cwd` — worktrees, parallel
agents — must get two groups, not collide), which a path-keyed dedup would have failed.
Goroutine start is `sync.Once`-guarded and `autoGroupEnabled`/`isAutoGroupEnabled()` are
`autoGroupMu`-protected (read from the loop goroutine, the `/watch` button handler, and
`renderWatchCard`) — verified race-free under `go test -race`.

**Deviation from the interrupted draft's own `SetAutoGroupWorkspaces` shape** (not from the
pinned contract, which already specifies the simplified 2-arg signature): the draft's
version took `bindingStorePath`/`ownerUserID` params and critique C4 offered two
alternative fixes — lazily create the manager, or "drop the parameter and fail loudly if
nil". The pinned signature (`enabled bool, interval time.Duration`) has no path/owner
params at all, so failing loudly would leave the feature permanently non-functional for
its primary target shape: cmux/herdr-style agents (one `Agent` instance addressing many
externally-owned workspaces) have no reason to ever call `SetMultiWorkspace`, which is the
*only* other thing that initializes `e.workspaceBindings`. Implemented the lazy-create
alternative instead, deriving the same default path `SetMultiWorkspace` uses
(`data_dir/workspace_bindings.json`) from `e.dataDir` (already set via `SetDataDir` earlier
in `cmd/cc-connect/main.go`'s init sequence) — `SetMultiWorkspace`-then-`SetAutoGroupWorkspaces`
reuses the *same* manager object (no fork), and `SetAutoGroupWorkspaces`-only reuses the
same *path* so the two features can never diverge into two files even if both end up
enabled. `ownerUserID` is passed as `""` to `CreateGroupChat` for the background sweep (no
config key for it — the design doc's own §6 config surface lists only
`auto_group_workspaces`/`auto_group_interval_ms`, no owner key); confirmed safe by reading
`platform/feishu/feishu.go`'s `CreateGroupChat`, which already no-ops `OwnerId(...)` when
empty.

`MsgAutoGroupCreated` is a **new**, separate key from the existing `MsgGroupCreated`
(reused unchanged by `/session-group`/`/dir-group`) — deliberate content differentiation:
the existing key's copy ("Group chat created: %s") reads as a direct response to a user's
own command; the background sweep has no requesting user, so the new key's copy explicitly
says the binding was automatic.

### Mechanism: `/watch` bind button (C6)

`renderWatchCard` appends a third button (`act:/watch bind-groups`,
`MsgWatchBindBtn`) only when `isAutoGroupEnabled()`. `executeCardAction`'s `"/watch"` case
was converted from a single `if EqualFold(...,"stop")` into a `switch` with **one
`strings.EqualFold` comparison per case** (not a blanket `strings.ToLower`) — this is
exactly critique C6's ask: `EqualFold`/`ToLower` are not equivalent for non-ASCII, and the
draft's `switch strings.ToLower(...)` rewrite would have silently changed that behavior.

### Deviations / notes for the record

- Caught and fixed, before yielding, a hard-rule violation in my **own** first draft of the
  `SetAutoGroupWorkspaces` doc comment: it named "cmux/herdr-style agents" as illustrative
  prose. `grep -rni 'cmux|herdr' core/` catches comments as well as identifiers/strings, so
  even explanatory doc comments must stay agent-agnostic in `core/`; reworded to "agents
  shaped this way" and re-verified zero matches.
- `doctor.go` left untouched: nothing in design doc §3/§6/§7 slice C or the batch contract
  asks core-level doctor plumbing for these two features, and per-agent doctor checks
  (socket ping, hooks presence) are explicitly cmux/herdr's own file list in §4.3, not
  core's.
- `auto_group_workspaces`/`auto_group_interval_ms` are wired only at process-start config
  load in `cmd/cc-connect/main.go`, not in the config-reload path (~line 1890, "Reload
  filter_external_sessions" etc.) — matches the existing precedent that
  `SetMultiWorkspace` (an equally structural, background-loop-owning feature) is likewise
  absent from that reload path; simple value/threshold toggles are hot-reloadable in this
  codebase, background-loop features are not.
- Test-harness discovery (not a product bug): calling `processInteractiveEvents` directly
  in a test requires `sendDone` to be a pre-fired buffered channel
  (`make(chan error, 1); sendDone <- nil`), not a permanently-empty one — turn completion
  waits on `pendingSend` internally even after `Done:true`. Confirmed via a throwaway
  diagnostic test that a **bare** `EventResult{Done:true}` with no permission handling at
  all hung the same way with an empty `sendDone`, isolating this from my new code before
  fixing the three affected tests.

### Test status (final, this session)

`go build ./core/` clean. `go vet ./core/` clean. `gofmt -l` clean on every file this slice
touched (pre-existing gofmt drift on ~26 untouched core/ files, e.g. `cron.go`, `hooks.go`,
confirmed present before this session's edits — not mine, left alone). Full package suite
`go test ./core/ -count=1`: **all tests pass** (30.6s), including the five required by name:

```
--- PASS: TestEngine_WaitForPermissionResolution_UnblocksOnExternalNotify
--- PASS: TestEngine_EventPermissionResolved_ClearsMatchingPending
--- PASS: TestEngine_BackgroundPermission_NoAutoDenyForExternalNotifier
--- PASS: TestAutoGroup_DedupsAcrossTicks_AndRecoversPartialFailure
--- PASS: TestWatchCard_BindButtonRow
```

Plus 4 supplementary tests written alongside them (also green, including under
`-race -count=1`): `TestEngine_EventPermissionResolved_IgnoresNonMatchingRequest`,
`TestEngine_BackgroundPermission_ApproveAllStillAppliesForExternalNotifier` (guards the
"/yolo approveAll behavior preserved" requirement explicitly), `TestAutoGroup_TwoWorkspacesSameRepoGetTwoGroups`,
`TestAutoGroup_NoGroupChatCreatorPlatform_NoOp`, `TestAutoGroup_Disabled_SweepIsNoOp`.
`grep -rni 'cmux|herdr' core/` → zero matches (verified after the doc-comment fix above).
`go build ./config/` and `go build ./cmd/cc-connect/` both clean against this session's
`config.go`/`main.go` edits (agent/platform packages compiled together with whatever
sibling-slice state was on disk at verification time; no cross-package errors observed).

No leftovers within this slice's scope. `cmd/cc-connect/plugin_agent_cmux.go` is on disk
but is Slice B's file, not touched here.

### Review fixes — CoreFix, 2026-07-27

Fixed all findings from `ReviewCore`'s FIX-FIRST verdict (3 BLOCKER, 2 MAJOR, 9 MINOR, 3
open questions) on this slice. Landed the pinned `core/message.go`
`PermissionNoteFallbackTimeout` const + `core/i18n.go` `I18n.ResolvePermissionNote`
mapping FIRST and broadcast to `CmuxFix` the moment `go build ./core/` was clean, per the
cross-slice contract, before starting the rest of this pass.

**B1 (background pending overwrite, consent-integrity BLOCKER).**
`handleBackgroundExternalPermission` no longer unconditionally overwrites
`state.pending`: it takes the busy/not-busy decision under `state.mu` first, installs
the new pending (and sends its card) ONLY when the slot is free, and for a displaced
request only registers the `ExternalResolutionNotifier` callback (so the external
authority can still resolve it) and `slog.Warn`s — no second card, no note sent when
the displaced request later resolves (there is no card baseline to reference). Card #1's
buttons keep working throughout because `state.pending` is never touched by request #2.
Regression: `TestEngine_BackgroundPermission_OverlappingRequestsDoNotCrossResolve`
(`core/external_resolution_test.go`) — two overlapping background requests; asserts
card #1's tap still resolves req-1 (`RespondPermission` called exactly once, for req-1)
and req-2's external resolution completes cleanly (notifier cancelled, no note, no
crash) without ever touching `state.pending`. Verified failing pre-fix by reverting just
the busy-check block back to the unconditional `state.pending = pending` +
`sendExternalPermissionPrompt` call: reproduces the reviewer's exact defect (2nd card
sent) 1/1 immediately, then 30/30 clean under `-race` after restoring the fix.

**B2 (concurrent sweep TOCTOU, BLOCKER).** Two independent defects, both fixed:
(1) added `Engine.autoGroupSweepMu sync.Mutex`; `autoGroupSweep` now does
`if !e.autoGroupSweepMu.TryLock() { return }` before touching anything, so the ticker
loop and the `/watch` "bind unbound groups" on-demand goroutine (`executeCardAction`'s
`go e.autoGroupSweep()`) can never run the `LookupBySessionID`→`CreateGroupChat` window
concurrently — a losing concurrent caller is a clean no-op, not a wait. (2)
`WorkspaceBindingManager.LookupBySessionID` now returns a copy (`b2 := *b; return ck,
&b2, true`) instead of the manager's internal pointer, so a caller reading
`binding.Activated`/`binding.ChannelName` outside the manager's lock can never observe a
torn read against `MarkActivated`'s write or a `refreshLocked` map-replacement.
Regression: `TestAutoGroup_ConcurrentSweeps_NeverCreateDuplicateGroups`
(`core/auto_group_test.go`) — two goroutines call `autoGroupSweep()` concurrently
(auto-group state wired directly, bypassing `SetAutoGroupWorkspaces` so the test isolates
exactly the two real trigger sources under test); asserts exactly 1 `CreateGroupChat`
call and exactly one binding. `stubAutoGroupPlatform` gained an opt-in `createDelay`
field (zero by default, only this test sets it) to widen the check-then-act window so the
two sweeps reliably overlap instead of racing to interleave by luck. Verified failing
pre-fix by reverting just the `TryLock`/`Unlock` guard: 5/5 runs reproduced 2
`CreateGroupChat` calls for one session (matches the reviewer's own "5/5 runs" report),
then 5/5 clean under `-race` after restoring. Also added
`TestWorkspaceBindingManager_LookupBySessionID_ReturnsCopy`
(`core/workspace_binding_test.go`) for defect (2): mutates a returned binding, asserts a
second lookup is unaffected; verified failing pre-fix by reverting the copy (`return ck,
b, true`).

**B3 (namespace collision with multi-workspace routing, BLOCKER).**
`autoGroupSweep`'s `projectKey` moved from `"project:"+e.name` to `"autogroup:"+e.name` —
a namespace `lookupEffectiveWorkspaceBinding`/`interactiveKeyForSessionKeyLocked`/`Unbind`
never read, so multi-workspace routing can no longer see (and Unbind-on-missing-directory)
an auto-group binding. `LookupBySessionID`/`BindSession` call sites in `autoGroupSweep`/
`autoGroupEnsure` needed no other change since they already took `projectKey` as a
parameter. Went with the reviewer's minimal fix (own namespace) over the louder
alternative (refuse to enable when `multiWorkspace` is already true): the doc comment on
`SetAutoGroupWorkspaces` already establishes both features may legitimately coexist on one
project, and main.go deliberately wires them independently. Regression:
`TestAutoGroup_MultiWorkspaceEnabled_ForeignProjectPathNeverUnbound`
(`core/auto_group_test.go`) — both features enabled on one engine; a sweep creates an
auto-group binding for a session whose `ProjectPath` doesn't exist on this host; a direct
`lookupEffectiveWorkspaceBinding` call (the exact function multi-workspace message routing
uses) must miss instead of finding-then-unbinding; a second sweep must not re-create a
group. Verified failing pre-fix by reverting just the `projectKey` string back to
`"project:"+e.name`: fails immediately (the test's own namespace-aware lookup times out,
since the reverted code writes under the OLD namespace) — restoring the fix, 5/5 clean.

**M1 (missing regression tests).** Added `TestEngine_AutoGroupSweep_IdempotentAcrossRestart`
(the test named by critique C4 fix item 4): two `Engine`s pointed at the same data dir,
simulating a process restart; the second engine's first sweep must see the persisted,
activated binding via `LookupBySessionID` and must NOT call `CreateGroupChat` again.
`TestCUJ_WSGROUP1_AutoGroupPerWorkspace` (design §7) is explicitly deferred per this
task's own scope ("CUJ tests are a later phase — skip them") — not added. The "one
concurrency regression each for B1 and B2" requirement is satisfied by
`TestEngine_BackgroundPermission_OverlappingRequestsDoNotCrossResolve` and
`TestAutoGroup_ConcurrentSweeps_NeverCreateDuplicateGroups` above.

**M2 (dead `MsgPermissionFeedFellBack`).** Wired via the pinned sentinel: added
`I18n.ResolvePermissionNote(note string) string` (`core/i18n.go`, next to `Tf`) —
`PermissionNoteFallbackTimeout` → `MsgPermissionFeedFellBack`, `""` →
`MsgPermissionResolvedElsewhere`, anything else → returned as-is (already a complete
user-facing string, e.g. from `EventPermissionResolved.Content` or an adapter's own
`notify(note)`). All three note-rendering sites that used to inline the
empty-string-only check (`handleBackgroundExternalPermission`, the interactive
`OnExternalResolution` closure, `case EventPermissionResolved:`) now call this one
helper, so the 3-way mapping is defined in exactly one place. Regression:
`TestI18n_ResolvePermissionNote` (`core/i18n_test.go`), table-checks all three branches.

**Minors — all applied:**
- m1: `defer cancelExternal()` → hoisted `var cancelExternal func()` + a direct call
  right after `<-pending.Resolved` returns. No behavior change (the wait is a hard
  blocking point with no early-return gap between registration and the direct call), just
  stops holding every already-resolved permission's registration open until end of turn.
- m2: fixed the `EventPermissionResolved` doc comment in `core/message.go` — it now
  correctly states the interactive loop DOES special-case the event (defensive
  backstop) while the background/unsolicited reader loop does NOT (adapters must use
  `OnExternalResolution` for background sessions), instead of the old "the engine does
  not special-case it" line that contradicted `processInteractiveEvents`'s own
  `case EventPermissionResolved:`. Comment-only, per this task's scope (the reviewer's
  alternative — adding the case to the reader — was not requested).
- m3: `handleBackgroundExternalPermission` now calls `sendPermissionPrompt` (title,
  `HookEventPermissionRequested` emission) instead of `sendExternalPermissionPrompt`
  (the "[Hook]"-titled, no-hook-emit variant meant for webhook-registered external
  permissions). The recursion concern `sendExternalPermissionPrompt`'s doc comment
  guards against doesn't apply here — this request comes from the adapter's `Events()`
  channel, not a hook.
- m4: documented `auto_group_workspaces`/`auto_group_interval_ms` in
  `config.example.toml` (bilingual, matching the file's convention), in a new section
  right after "Multi-workspace Mode".
- m5: dropped the duplicate `< 5000 → 5000` clamp in `cmd/cc-connect/main.go`; the
  engine's `minAutoGroupInterval` clamp is now the single floor. Reworded the startup
  log key to `configured_interval_ms` so it doesn't misreport an un-clamped value as
  authoritative.
- m6: `runAutoGroupLoop` now sweeps once before entering the ticker `select` loop, so the
  first group doesn't wait a full interval. Safe only because B2 landed first.
- m7: documented the startup-only invariant on `SetAutoGroupWorkspaces`'s doc comment
  instead of adding a new lock — `e.workspaceBindings` is already written unguarded by
  the sibling `SetMultiWorkspace`, and every reader (including `autoGroupSweep`) already
  trusts that same happens-before-via-goroutine-creation ordering; a lock around just
  this one write/read pair would be inconsistent with every other consumer of the field
  and wouldn't close a real gap.
- m8: reverted the unrelated gofmt churn in `core/watch.go` (trailing blank line at EOF)
  and `core/watch_test.go` (stub-method column realignment) back to match `main` byte-
  for-byte, keeping the diff to just the bind-groups button logic and its test.
  `core/i18n.go`'s realignment left untouched, matching the review's own call that it's
  gofmt-mandated by the new key name and unavoidable.
- m9: added a comment on `autoGroupEnsure`'s `sm := e.GetSessions()` explaining why the
  top-level `SessionManager`/`Agent` pair is deliberate even when multi-workspace is also
  enabled: after B3's namespace fix, this channel's binding is invisible to
  `sessionContextForKey`, so future interactive turns in this chat fall back to the same
  top-level pair anyway — consistent, not a bug.

**Open questions — resolved and recorded:**
1. `state.replyCtx`/`state.platform` nil-safety on the background card path: verified by
   reading every `case` in `runUnsolicitedReader`'s event loop (`EventText`,
   `EventToolUse`/`Result`, `EventResult`, the pre-existing auto-deny branch, and now the
   new background-permission branch) — NONE of them guard `p`/`replyCtx` against nil; a
   nil `state.platform` would panic identically in `e.send`'s `p.Name()` call regardless
   of which branch reached it. This is a pre-existing, function-wide invariant
   (`state.platform` is always set by a foreground `processInteractiveMessageWith` call
   before `startUnsolicitedReader` can ever be invoked — both production call sites,
   `engine.go` `drainOrphanedQueue` and the turn-completion path, only run after that),
   not something specific to the permission-card path, so no new guard was added — matching
   the reviewer's own "parity, not regression" read. Flagging here per this task's
   instruction rather than adding an inconsistent one-off check.
2. Shared `workspace_bindings.json` lost-update window: confirmed real but unchanged by
   this pass — every project's `WorkspaceBindingManager` (multi-workspace routing AND now
   auto-group, sharing the SAME file when both are enabled per B3's design) does a full
   mtime/size-gated `refreshLocked` + whole-map `saveLocked` under only its own in-process
   mutex, so two projects (or a project and any other process) writing around the same
   moment can lose one write. Auto-group's periodic sweep does widen this window
   (one more writer, on a timer) but doesn't introduce a new class of bug — the existing
   multi-workspace `/workspace bind` flow already had it. No code change: a real fix needs
   either file-level locking or a single-writer store design, which is out of this
   review's scope and would need its own design pass.
3. Auto-group bindings in `/groups`: decided NOT to wire them in. `cmdGroups` is fully
   gated on `e.multiWorkspace` (returns "empty" otherwise) and reads
   `ListByProject(e.name)` — neither namespace `autoGroupSweep` now writes to
   (`"autogroup:"+e.name`) nor the `"project:"+e.name` multi-workspace uses. Design §3.4
   never mentions `/groups` visibility as a requirement, and auto-group's primary target
   shape (no multi-workspace) never reaches `cmdGroups` at all today (routing goes
   through `SwitchToAgentSession`, not the binding-derived UI). Wiring it in would mean
   either loosening `/groups`'s `multiWorkspace` gate or adding a second namespace to its
   scan — real UX/routing design work belonging to a follow-up, not a review-fix pass.

**Verification.** `go build ./...` clean. `go vet ./...` clean.
`go test ./core/ -race -count=1` green across 5 consecutive full-suite runs, modulo a
**pre-existing** flake in `TestCUJ_A3_ImageReachesAgent`/`TestCUJ_A5_FileReachesAgent`
("TempDir RemoveAll cleanup: directory not empty") reproduced at a similar
intermittent-failure rate by stashing every change in this pass back to bare `main`
and re-running just those two tests directly — unrelated to this slice, not touched. `gofmt -l` clean on
every file this pass touched except the two deliberately-reverted spots in `watch.go`/
`watch_test.go` (m8) and one pre-existing, untouched misalignment in
`core/i18n_test.go`'s `TestDetectLanguage` (also present on bare `main`, confirmed via
`gofmt -l` against `git show main:core/i18n_test.go`). Each BLOCKER's regression test was
confirmed to fail against its own pre-fix logic via a targeted revert-run-restore cycle
(not a full-repo stash, to keep the other landed fixes in place while isolating each
guard) — failure output and run counts recorded above per finding.

Noticed in passing, not part of this review and not touched: `getOrCreateWorkspaceAgent`
(`core/engine.go` ~4230) derives its per-workspace session-file path from
`filepath.Dir(e.sessions.StorePath())`, which is `"."` when a test constructs
`NewEngine(name, ..., "", lang)` (empty store path, common in this package's tests) —
any test that reaches it writes a stray `<name>_ws_<hash>.json` into `core/` itself. Not
new (pre-existing on `main`), not triggered by anything added in this pass, cleaned up
the stray files it left behind during verification.

## Slice B review fixes (CmuxFix, 2026-07-27) — merged from agent/cmux/IMPLEMENTATION_NOTES.md


Scope is intentionally limited to `agent/cmux/` and the cmux block in
`config.example.toml`; the shared bridge notes under `docs/logs/` were not
edited because the assignment's file boundary is narrower.

## Blocker and major findings

- B1: newest hook session now wins per workspace across both stores; the
  authoritative active-session join overrides historical data. The 17-session,
  3-surface fixture failed pre-fix on iteration 1 of a 100-run assertion.
- M1: stability uses `max(10, 5000/intervalMs)`, requires a post-baseline
  screen, and pauses while the workspace owns an outstanding Feed permission.
- M2: generic resolution notes are empty. Timeout notes use the pinned sentinel
  value `permission_fallback_timeout`. The coordinated core symbol
  `core.PermissionNoteFallbackTimeout` is still absent in this checkout, so a
  package-local alias preserves the package build without crossing the core
  ownership boundary; replace it with the core constant when CoreFix lands.
- M3: ordinary RPC calls have a cancellation watchdog and a configurable
  `rpc_timeout_ms` (default 10000, minimum 1000).
- M4: successful replies leave 30-second tombstones which stale list snapshots
  cannot re-emit; the expiry pass purges old tombstones.
- M5: `work_dir` is absolute; restart matching and filtering use only custom
  titles or cwd, never the live animated `title`; `latest_submitted_at` supplies
  list recency. This propagates the live-probe finding that `title` is terminal
  display state, not the name passed to `workspace.create`.
- M6: tests now cover shared-bus refcounts, expiry fallback, reply
  mark-then-delete, AskUserQuestion routing, and debounce re-arming.

## Minor triage (m1-m23)

- Applied: m1 backoff reset; m2 ack deadline; m3/m4 stop serialization and
  stream join; m5 unused frame boot ID removal; m6 activity pruning; m7 unused
  snapshot removal; m8 speculative map branch removal; m9 25ms mapper refresh
  coalescing; m10 fresh retry decode target; m11 empty/deleted-store caching;
  m12 divergent shared-bus warnings; m13 speculative screen fallbacks removal;
  m15 real recency field; m17 real client in gap test; m18 channel ownership;
  m19 serialized cold resync; m20 independent expiry sweeper; m21 absolute cwd;
  m22 completed-frame refetch coverage; m23 guarded fixture response queue.
- Partial m16: replaced `goto` with a labelled drain loop. Skipped a hook epoch
  protocol because it changes event correlation semantics and the report gives
  only a narrow hypothetical race with no epoch present on the live wire.
- Skipped m14 live auth probing: authentication is mutation-sensitive and no
  disposable authenticated cmux endpoint is available. The unverified branch
  remains config-gated and is explicitly documented beside the handshake.

Owner consolidation: the package-local sentinel alias in agent/cmux/feed.go now
references core.PermissionNoteFallbackTimeout directly (landed after this note was
written); the stray per-package notes file was merged here and removed.
