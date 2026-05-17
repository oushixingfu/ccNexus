# Provider Model Routing Design

Date: 2026-05-17
Status: Draft for review

## Context

ccNexus currently treats one endpoint as one routable upstream model. The endpoint stores a single `model` string, the Web UI edits one model field, and proxy request handling overwrites the client request model with the endpoint model before forwarding upstream.

That makes endpoint switching too coarse for providers such as Claude accounts, relay gateways, Kimi, DeepSeek, or Codex token pools. A single provider/account can support multiple useful models, while downstream tools request a specific model. The routing decision should therefore be model-aware without losing the existing endpoint priority and failover behavior.

## Goals

- Treat an endpoint as a provider/account, not as a single model.
- Store and manage multiple models under one endpoint.
- Only route to a model on an endpoint after ccNexus verifies the model with a real minimal upstream call.
- When a downstream request names a model, first filter endpoints to those verified for that model, then preserve existing endpoint priority/failover behavior.
- Keep endpoint ordering, cooldowns, recovered-endpoint policy, token pool selection, runtime availability, and request-local failover semantics.
- Expose only verified routable models through `/v1/models`.
- Keep existing single-model endpoint configurations working after migration.

## Non-Goals

- No strict per-request round-robin across providers.
- No immediate routing to merely discovered models.
- No broad provider-specific model capability taxonomy such as context windows, tool support, or price metadata in the first implementation.
- No dependency changes unless an implementation detail later proves the standard library is insufficient.

## Confirmed Behavior

When a downstream request includes `model`:

1. ccNexus finds endpoints with an enabled, verified model record matching that model.
2. The request plan is restricted to those endpoints.
3. Within that restricted plan, endpoint priority remains the source of truth:
   - the preferred/current endpoint is used when it is eligible and available;
   - failure, quota, cooldown, and token unavailability switch to the next eligible endpoint;
   - when the preferred endpoint recovers, existing recovered-endpoint policy controls return behavior.
4. If no endpoint has verified support for the requested model, ccNexus returns a model availability error instead of trying an unverified upstream.

When the downstream request does not include a model, ccNexus uses the endpoint default model behavior for compatibility.

## Data Model

Keep the existing `endpoints.model` column as the endpoint default model. Add a separate endpoint model table:

```sql
CREATE TABLE endpoint_models (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    endpoint_name TEXT NOT NULL,
    model_id TEXT NOT NULL,
    display_name TEXT DEFAULT '',
    source TEXT NOT NULL DEFAULT 'manual',
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    verification_status TEXT NOT NULL DEFAULT 'unknown',
    upstream_transformer TEXT DEFAULT '',
    failure_kind TEXT DEFAULT '',
    failure_message TEXT DEFAULT '',
    last_verified_at TIMESTAMP NULL,
    verification_expires_at TIMESTAMP NULL,
    last_attempt_at TIMESTAMP NULL,
    next_attempt_at TIMESTAMP NULL,
    sort_order INTEGER DEFAULT 0,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(endpoint_name, model_id)
);
```

Field meanings:

- `endpoint_name`: follows the existing project pattern for endpoint-related tables. Rename/delete logic must update or delete these records together with the endpoint.
- `model_id`: exact upstream model id, trimmed but otherwise case-preserved.
- `source`: `manual`, `discovered`, or `legacy`.
- `enabled`: whether this model is allowed to be exposed and routed once verified.
- `verification_status`: `unknown`, `discovered`, `verifying`, `verified`, or `failed`.
- `upstream_transformer`: transformer that successfully verified the model, such as `claude`, `openai2`, `openai`, `kimi`, `deepseek`, or `gemini`.
- `failure_kind`: normalized verification failure category.
- `verification_expires_at`: TTL boundary after which background verification should refresh the record.

Backfill:

- For every existing endpoint with non-empty `model`, create one `endpoint_models` row with:
  - `model_id = endpoints.model`
  - `source = legacy`
  - `enabled = true`
  - `verification_status = verified`
  - `upstream_transformer = normalized endpoint transformer`
- Legacy rows should be revalidated by the background verifier, but they remain routable immediately to avoid breaking existing installations.

## Model Registry

Introduce a model registry layer used by proxy routing and `/v1/models`.

Responsibilities:

- Load enabled endpoint model records.
- Return verified endpoint candidates for an exact model id.
- Queue background verification for unknown/discovered/expired models.
- Update model verification status and failure metadata.
- Keep an in-memory index for routing hot paths, refreshed when endpoint config or model verification state changes.

The first implementation can keep this inside `internal/proxy` and `internal/storage` rather than creating a new package, as long as the API is small and testable.

## Discovery and Verification

Discovery is not enough for routing. It only creates or updates candidate model records.

Discovery sources:

- Web UI "Fetch Models".
- `/v1/models?refresh=true` when refresh is enabled.
- Manual model entry in the endpoint model UI.
- A downstream request for a model that has no verified candidates. This should enqueue background verification work for plausible endpoints, but the current request still returns `model_not_verified`.
- Optional periodic refresh in a later iteration.

Verification is a real minimal upstream call:

- Claude: send a minimal `/v1/messages` request using the candidate model.
- OpenAI Responses / Codex: send a minimal `/v1/responses` request using the candidate model.
- OpenAI Chat / Kimi / DeepSeek: send a minimal `/v1/chat/completions` request using the candidate model.
- Gemini: send a minimal `generateContent` request using the candidate model.

The verifier must parse a valid assistant/model response, not just accept an HTTP 200 or health endpoint result.

Verification outcomes:

- `verified`: valid response parsed.
- `unsupported_model`: model not found, invalid model, unsupported model, or explicit provider denial for that model.
- `auth_failed`: 401/403 authentication/account issue. This affects endpoint availability but does not prove the model is unsupported.
- `quota_limited`: 429 or provider quota exhaustion. Transient; retry later.
- `network_error`: transport timeout/DNS/connection reset. Transient; retry later.
- `upstream_error`: 5xx or malformed provider error. Transient; retry later.
- `invalid_response`: response did not match expected provider shape.

Default TTLs:

- Verified: 24 hours.
- Unsupported model: 7 days.
- Transient failures: 5 to 30 minutes depending on failure kind.
- Auth failures: retry after credential or endpoint config changes; otherwise use a long cooldown.

Concurrency:

- Run verification in the background.
- Limit concurrency globally and per endpoint, for example 2 global and 1 per endpoint.
- Do not block user traffic waiting for verification.

## Request Routing

Request routing changes only when the client request contains a non-empty `model`.

Current flow:

- Resolve specified endpoint if any.
- Build an endpoint plan.
- Select an upstream transformer.
- Transform and forward the request.

New flow:

1. Extract client model from request body before endpoint planning.
2. If model is empty, keep existing behavior.
3. If a specific endpoint is requested through header/query/model selector:
   - verify that the endpoint has an enabled verified record for that model;
   - if yes, route to that endpoint using the requested model;
   - if no, enqueue background verification if appropriate and return `model_not_verified`.
4. If no specific endpoint is requested:
   - ask the model registry for enabled verified endpoint names for that model;
   - filter the enabled endpoint list to that set;
   - apply existing request-plan ordering, cooldown handling, recovered endpoint policy, routing strategy, and failover inside that filtered list.
5. For the chosen endpoint, build an effective endpoint for this request:
   - `effective.Model = client model`
   - `effective.Transformer = verified upstream_transformer` when present, otherwise the endpoint's selected transformer
   - do not persist this per-request override to the endpoint default model.

No verified candidate:

- Return a structured error without trying unverified providers.
- Queue background verification for plausible endpoint/model pairs so a future request can route after verification succeeds.
- Use HTTP 400 for client-facing compatibility with invalid model errors.
- The response body should match the client format where practical:
  - OpenAI-compatible: `{"error":{"message":"model is not verified or available: <model>","type":"invalid_request_error","code":"model_not_verified"}}`
  - Claude-compatible: include an equivalent JSON error with `model_not_verified`.

## Endpoint Selector Semantics

The existing `@endpoint/model` syntax should become meaningful again:

- `@provider/model-id` chooses the endpoint and requests `model-id`.
- ccNexus must not ignore the model suffix when model registry routing is enabled.
- If the endpoint has verified support for the suffix model, use it.
- If not, return `model_not_verified` for that endpoint.

`@endpoint` without a model keeps the endpoint's default model behavior.

## `/v1/models` Behavior

`/v1/models` should expose routable models only:

- include enabled verified endpoint model records;
- include legacy backfilled models until they expire and fail revalidation;
- exclude merely discovered, unknown, verifying, or failed models.

Each returned model should preserve the existing `endpoint_id` metadata so downstream users can see which provider/account backs the model.

If the same model is verified on multiple endpoints, return one entry per endpoint with `endpoint_id`. This preserves the current endpoint-aware behavior and keeps provider/account observability.

## Web UI

Endpoint modal should treat the endpoint as provider/account settings:

- Name
- API URL
- API key / auth mode
- Default model
- Force stream / enabled state

Add a model management section or detail view:

- Fetch/discover models from upstream.
- Show model rows with status: discovered, verifying, verified, failed.
- Allow enabling/disabling specific models.
- Allow marking one model as the endpoint default.
- Provide "Verify now" for selected models.
- Provide bulk actions for discovered models because upstream lists can be large.

UI should not auto-enable every discovered model by default. The user can explicitly enable desired models, and enabled models still need verification before routing.

## API Surface

Add endpoint-model APIs under the existing Web UI API:

- `GET /api/endpoints/{name}/models`
- `POST /api/endpoints/{name}/models/discover`
- `POST /api/endpoints/{name}/models`
- `PUT /api/endpoints/{name}/models/{model}`
- `POST /api/endpoints/{name}/models/{model}/verify`
- `DELETE /api/endpoints/{name}/models/{model}`

Model ids must be URL-escaped when used in a path. If that becomes awkward for providers that use slash-like model ids, the implementation should use request bodies for model ids while keeping the same endpoint-model resource shape.

## Error Handling

Model registry errors must not be conflated with endpoint health:

- Unsupported model means that endpoint should not be a candidate for that model.
- Auth/quota/network failures should update endpoint or verification cooldowns, but should not permanently mark the model unsupported.
- If an endpoint loses verification due to expiry and background revalidation fails transiently, keep the last verified result until a short grace period expires. This avoids traffic flapping during provider incidents.

## Testing Strategy

Storage:

- Migration creates `endpoint_models`.
- Existing `endpoints.model` values are backfilled.
- Endpoint rename/delete updates model records.

Model registry:

- Exact model lookup returns only enabled verified endpoints.
- Discovered/unverified models are not routable.
- Expired verified models queue background revalidation.

Verifier:

- Claude/OpenAI Responses/OpenAI Chat/Kimi/Gemini probes use the expected paths and minimal payloads.
- Valid assistant response marks verified.
- Unsupported model response marks unsupported.
- Auth/quota/network failures get distinct failure kinds and retry timing.

Routing:

- Request model filters candidate endpoints before failover planning.
- Endpoint priority is preserved inside model candidates.
- Preferred endpoint failure switches to the next verified candidate.
- Preferred endpoint recovery follows existing recovered-endpoint policy.
- Specific `@endpoint/model` succeeds only when that endpoint is verified for the model.
- No verified candidates returns `model_not_verified` without upstream trial.

Models API:

- `/v1/models` exposes only verified enabled models.
- It does not expose merely discovered models.
- Multiple endpoints verified for the same model remain observable through `endpoint_id`.

## Rollback and Compatibility

Rollback safety:

- Keep `endpoints.model` intact.
- Additive table migration only.
- Existing endpoints keep working through legacy verified model backfill.
- If model registry is disabled or unavailable, implementation can fall back to current endpoint default behavior behind a configuration flag during rollout.

Compatibility risks:

- Strict model verification changes behavior for clients that request arbitrary model strings. They may now receive `model_not_verified` instead of being forwarded to a compatible endpoint.
- Verification consumes small amounts of provider quota. Concurrency and TTLs must be conservative.
- Token pool endpoints may verify with one credential while another credential later lacks access. Existing credential failover remains responsible for per-account runtime failures; model verification is endpoint-level in the first implementation.

## Implementation Defaults

- Verified TTL: 24 hours.
- Unsupported-model retry TTL: 7 days.
- Transient retry TTL: 5 to 30 minutes based on failure kind.
- Discovered models are disabled by default for bulk discovery, then enabled by explicit user action.
- Manually added models are enabled by default but not routable until verified.
- No legacy "try compatible endpoint" behavior for model requests after model registry routing is enabled. A temporary compatibility flag can be added only if implementation testing shows migration pain.
- Claude-compatible model errors should use the existing project error writer style while preserving the `model_not_verified` code in the response body.
