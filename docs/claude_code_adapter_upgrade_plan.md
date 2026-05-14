# Claude Code Adapter Upgrade Plan

## Background

cc-switch can only point Claude Code at one Anthropic-compatible backend. In this deployment, that backend is ccNexus. ccNexus therefore needs to present a stable Claude Code `/v1/messages` surface while selecting the best upstream protocol per endpoint behind the scenes.

## Goals

- Keep cc-switch configured with a single Claude Code provider.
- Let ccNexus adapt Claude Code requests to Claude, OpenAI Chat, OpenAI Responses, DeepSeek, Kimi, or Gemini endpoints.
- Prefer the most compatible upstream protocol for Claude Code traffic.
- Make the selected effective upstream visible in Web UI.
- Keep existing Codex/OpenAI client behavior intact.

## Implementation Checklist

- [x] Step 1: Make Claude Code upstream selection prefer native Claude, then Chat-compatible upstreams, then Responses unless explicitly configured.
- [x] Step 2: Expand protocol fallback so Claude Code requests retry Responses-to-Chat on common gateway protocol errors such as unsupported `max_output_tokens`.
- [x] Step 3: Expose effective upstream protocol fields in endpoint API responses for Claude Code, OpenAI Chat, and OpenAI Responses clients.
- [x] Step 4: Show the Claude Code effective upstream in the server Web UI endpoint table.
- [x] Step 5: Add regression tests for Claude Code effective upstream selection, protocol fallback, and API response metadata.
- [x] Step 6: Run tests, deploy to `http://127.0.0.1:3021/`, and verify endpoint status plus test behavior.

## Progress

- 2026-05-14: Plan created.
- 2026-05-14: Implemented Claude Code capability-aware upstream selection, Responses-to-Chat protocol fallback, API metadata, Web UI upstream display, and regression tests.
- 2026-05-14: `go test ./... -count=1` and `git diff --check` passed; deployed to `http://127.0.0.1:3021/`; verified `/api/endpoints`, Claude Code `/v1/messages` request to `kimi-k2.6`, and Web UI upstream column.

## Acceptance Criteria

- `go test ./... -count=1` passes.
- `git diff --check` passes.
- `/api/endpoints` includes effective upstream fields.
- Web UI endpoint table shows each endpoint's Claude Code upstream.
- Claude Code `/v1/messages` requests can use a Chat-compatible upstream when an endpoint supports both Chat and Responses.
- Existing 3021 service stays healthy after deployment.
