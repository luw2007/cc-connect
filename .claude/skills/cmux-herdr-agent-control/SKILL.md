---
name: cmux-herdr-agent-control
description: Inspect existing cmux workspaces and Herdr agents, and safely control supported cmux actions through the cc-connect agent CLI. Use when another agent needs to discover external coding agents, inspect their status or recent terminal output, inspect pending requests, send an exact key to a cmux target, or resolve a supported cmux request.
---

# Cmux and Herdr Agent Control

Use `cc-connect agent` for direct discovery and inspection. It can mutate cmux targets, but direct Herdr control is read-only because Herdr's protocol does not provide an immutable target identity for race-safe writes. Do not open backend sockets directly and do not reconstruct IDs from paths, titles, or terminal text.

## Safe workflow

1. Discover exact targets with `cc-connect agent list --backend <cmux|herdr> --json`.
2. Copy both the returned `id` and opaque `revision` verbatim. Do not derive either from a path, title, name, or terminal text.
3. Inspect context with `tail` and pending interaction with `requests`, passing the same target revision. Copy each pending request's `id` and `revision` verbatim.
4. For cmux only, perform exactly one targeted mutation with those exact references. The CLI re-reads canonical backend state; it does not trust display metadata supplied from a previous JSON result.
5. Re-run `requests` or `tail` to verify the resulting cmux state.

Queries never create a workspace, pane, tab, or agent. An unknown/stale target, missing cmux surface, changed Herdr screen, stale request, or unsupported capability is an error. Stop rather than refreshing or guessing a fallback target.

Direct Herdr control supports only discovery, tail, and request inspection. It does not advertise or execute prompt, key, permission, or question-response mutations. Use Herdr's native control surface or another supported control path when a Herdr target must be mutated.

## Discovery

```bash
cc-connect agent list --backend cmux --json
cc-connect agent list --backend herdr --json
cc-connect agent list --project my-project --json
```

Each row includes `id`, `revision`, `backend`, `kind`, and `capabilities`; `directory` and `status` are optional and may be omitted from JSON. Use only the exact JSON values; do not use abbreviated human-table revisions for actions.

## Read recent output

```bash
cc-connect agent tail --backend cmux --id WORKSPACE_ID --revision TARGET_REVISION --lines 120 --json
cc-connect agent tail --backend herdr --id AGENT_NAME --revision TARGET_REVISION --lines 200 --json
```

cmux output is the current exact terminal-surface capture. Herdr tail output is recent scrollback. Neither is guaranteed to be a complete conversation history.

## Send an exact key (cmux only)

```bash
cc-connect agent key --backend cmux --id WORKSPACE_ID --revision TARGET_REVISION --key Enter
```

Direct prompt submission is intentionally unsupported: cmux exposes text insertion and Enter as separate RPCs, with no proven cross-process atomic submission guarantee. Use the native cmux surface for natural-language prompts. Use `key` only when terminal-state inspection shows a specific required key; keys can alter or interrupt the target process. Direct key injection is unsupported for Herdr; use its native surface or another supported control path instead.

## Inspect pending requests and resolve cmux requests

```bash
cc-connect agent requests --backend cmux --id WORKSPACE_ID --revision TARGET_REVISION --json
cc-connect agent requests --backend herdr --id AGENT_NAME --revision TARGET_REVISION --json
```

Every request returns its own opaque `id` and `revision`. Request output is inspectable for both backends. A Herdr request revision binds the full visible screen returned by Herdr even though request display is clipped. Re-read cmux requests before a mutation; do not retry an ambiguous action.

### cmux permission

```bash
cc-connect agent approve --backend cmux --id WORKSPACE_ID --revision TARGET_REVISION \
  --request REQUEST_ID --request-revision REQUEST_REVISION --mode once
```

`approve` is cmux-only. Allowed modes are returned by the request. Approve only after inspecting the exact request's `tool_name`, `input`, and allowed decisions.

### cmux structured questions

```bash
cc-connect agent answer --backend cmux --id WORKSPACE_ID --revision TARGET_REVISION \
  --request REQUEST_ID --request-revision REQUEST_REVISION --choice Key2
```

For multiple questions, use one indexed answer flag per question:

```bash
cc-connect agent answer --backend cmux --id WORKSPACE_ID --revision TARGET_REVISION \
  --request REQUEST_ID --request-revision REQUEST_REVISION \
  --answer 0:choice-one --answer 1:choice-two
```

Answers are supplied only through `--choice` or indexed `--answer` flags; there is no positional answer placeholder. Direct free-text and multi-select replies are intentionally unsupported because cmux has not proven their wire encodings. Never use conflicting forms for the same question. Only invoke `answer` when a cmux target advertises question support. Direct Herdr question response is unsupported even when `requests` shows an inspectable blocked screen; use Herdr's native surface or another supported control path.

## Project selection

Use `--project NAME` when the configured project identifies the backend and its options. Bare invocation without `--backend` uses config and requires exactly one configured project; with multiple projects, always select one explicitly. Password-protected cmux direct usage must select a config/project carrying the required credentials rather than relying only on `--backend` or `--socket`. For standalone operation without protected cmux credentials, use `--backend` and, if needed, `--socket PATH`.

## Required verification for cmux mutations

After any cmux mutation, run one of:

```bash
cc-connect agent requests --backend cmux --id WORKSPACE_ID --revision TARGET_REVISION --json
cc-connect agent tail --backend cmux --id WORKSPACE_ID --revision TARGET_REVISION --lines 80 --json
```

If that target revision is stale, start again from `list`; do not apply the old action to the replacement target.
