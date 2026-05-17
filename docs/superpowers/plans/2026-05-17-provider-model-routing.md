# Provider Model Routing Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make endpoints represent provider/accounts with multiple verified models, and route downstream model requests through verified providers while preserving endpoint priority, failover, and recovery behavior.

**Architecture:** Add an additive `endpoint_models` storage layer and a proxy-side model registry/verifier. Request routing filters endpoint plans by verified model support, then reuses the existing endpoint-priority and failover machinery. Web UI/API gain endpoint-model management and background verification status.

**Tech Stack:** Go 1.24, SQLite via `modernc.org/sqlite`, existing `internal/proxy` HTTP proxy, existing vanilla JS Web UI, existing Go `testing`/`httptest`.

## Progress / Acceptance Log

2026-05-17:

- [x] Provider model routing implementation is present through storage, registry, routing, verifier, `/v1/models`, Web API, and Web UI surfaces.
  - Acceptance: `go test ./internal/storage ./internal/proxy ./cmd/server/webui/api -count=1` passed.
  - Acceptance: `go test ./... -count=1` passed.
- [x] Endpoint test now records runtime success/failure, clears stale failure state after a successful manual test, and persists OpenAI Responses -> Chat protocol fallback only for specific gateway/protocol failures.
  - Acceptance: `go test ./cmd/server/webui/api ./internal/proxy -count=1` passed.
- [x] Dashboard and endpoint-list status now share the same backend availability payload and frontend formatter, and realtime SSE payloads include endpoint availability so both views stay consistent.
  - Acceptance: `TestRealtimeEventPayloadIncludesEndpointAvailability` covers the realtime payload using the same endpoint availability derivation as `/api/endpoints`.
  - Acceptance: `node --check cmd/server/webui/ui/js/main.js`, `node --check cmd/server/webui/ui/js/components/dashboard.js`, `node --check cmd/server/webui/ui/js/components/endpoints.js`, and `node --check cmd/server/webui/ui/js/utils/endpointStatus.js` passed.
- [x] Formatting and diff hygiene checked.
  - Acceptance: `gofmt` completed for touched Go files.
  - Acceptance: `git diff --check` passed.
- [ ] Repository-wide `go vet ./...` has pre-existing warnings outside this change in `internal/service/*` where `config.Config` is copied despite containing `sync.RWMutex`.
  - Acceptance result: command exits 1 with existing lock-copy warnings in `backup_local.go`, `backup_s3.go`, `settings.go`, and `webdav.go`; not introduced by this change.

---

## File Structure

- Modify `internal/storage/interface.go`: add `EndpointModel`, verification constants, and storage interface methods.
- Modify `internal/storage/sqlite.go`: create/migrate `endpoint_models`, backfill legacy `endpoints.model`, and keep model rows in sync on endpoint rename/delete.
- Create `internal/storage/endpoint_models.go`: CRUD/upsert methods for endpoint model rows.
- Modify `internal/storage/sqlite_test.go`: storage migration, backfill, rename, delete, and query tests.
- Create `internal/proxy/model_registry.go`: in-memory/indexed access to verified endpoint candidates and queueing hooks.
- Create `internal/proxy/model_registry_test.go`: registry candidate filtering and unverified behavior tests.
- Create `internal/proxy/model_verifier.go`: background verification worker and provider-specific probe helpers.
- Create `internal/proxy/model_verifier_test.go`: `httptest` provider probe tests for Claude/OpenAI Responses/OpenAI Chat/Kimi/Gemini and failure classification.
- Modify `internal/proxy/proxy.go`: filter request endpoint plans by verified model support and use request model as per-request effective model.
- Modify `internal/proxy/request.go`: stop overriding a verified model request with endpoint default when the registry supplies a per-request model override.
- Modify `internal/proxy/endpoint_resolver.go`: make `@endpoint/model` suffix usable for routing.
- Modify `internal/proxy/models.go`: expose only verified enabled endpoint models in `/v1/models`, while preserving `endpoint_id`.
- Modify existing proxy tests: `request_test.go` for request routing, `models_test.go` for `/v1/models`, and `request_local_fallback_test.go` only if model filtering changes existing request-local fallback expectations.
- Modify `cmd/server/webui/api/handler.go`: route endpoint-model API paths.
- Create `cmd/server/webui/api/endpoint_models.go`: endpoint-model Web API handlers.
- Create `cmd/server/webui/api/endpoint_models_test.go`: handler tests.
- Modify `cmd/server/webui/ui/js/api.js`: endpoint-model API client methods.
- Modify `cmd/server/webui/ui/js/components/endpoints.js`: endpoint model management UI.
- Modify `cmd/server/webui/ui/js/i18n/en.js` and `cmd/server/webui/ui/js/i18n/zh-CN.js`: model status/action labels.

## Task 1: Storage Schema and EndpointModel CRUD

**Files:**
- Modify: `internal/storage/interface.go`
- Modify: `internal/storage/sqlite.go`
- Create: `internal/storage/endpoint_models.go`
- Modify: `internal/storage/sqlite_test.go`

- [ ] **Step 1: Add failing storage tests**

Append these tests to `internal/storage/sqlite_test.go`:

```go
func TestEndpointModelsBackfillLegacyEndpointModel(t *testing.T) {
	store, err := NewSQLiteStorage(filepath.Join(t.TempDir(), "ccnexus-test.db"))
	if err != nil {
		t.Fatalf("new storage: %v", err)
	}
	defer store.Close()

	endpoint := &Endpoint{
		Name:        "Claude",
		APIUrl:      "https://api.anthropic.com",
		APIKey:      "test-key",
		AuthMode:    "api_key",
		Enabled:     true,
		Transformer: "claude",
		Model:       "claude-sonnet-4-5-20250929",
	}
	if err := store.SaveEndpoint(endpoint); err != nil {
		t.Fatalf("save endpoint: %v", err)
	}

	models, err := store.GetEndpointModels("Claude")
	if err != nil {
		t.Fatalf("get endpoint models: %v", err)
	}
	if len(models) != 1 {
		t.Fatalf("expected one backfilled model, got %#v", models)
	}
	model := models[0]
	if model.ModelID != "claude-sonnet-4-5-20250929" || model.VerificationStatus != EndpointModelStatusVerified || !model.Enabled {
		t.Fatalf("unexpected backfilled model: %#v", model)
	}
}

func TestEndpointModelsRenameAndDeleteWithEndpoint(t *testing.T) {
	store, err := NewSQLiteStorage(filepath.Join(t.TempDir(), "ccnexus-test.db"))
	if err != nil {
		t.Fatalf("new storage: %v", err)
	}
	defer store.Close()

	endpoint := &Endpoint{Name: "Primary", APIUrl: "https://api.example.com", APIKey: "test-key", AuthMode: "api_key", Enabled: true, Transformer: "openai", Model: "gpt-4.1"}
	if err := store.SaveEndpoint(endpoint); err != nil {
		t.Fatalf("save endpoint: %v", err)
	}
	if err := store.UpsertEndpointModel(&EndpointModel{EndpointName: "Primary", ModelID: "gpt-5.5", Source: EndpointModelSourceManual, Enabled: true, VerificationStatus: EndpointModelStatusVerified, UpstreamTransformer: "openai2"}); err != nil {
		t.Fatalf("upsert model: %v", err)
	}

	endpoint.Name = "Renamed"
	if err := store.UpdateEndpointByName("Primary", endpoint); err != nil {
		t.Fatalf("rename endpoint: %v", err)
	}
	models, err := store.GetEndpointModels("Renamed")
	if err != nil {
		t.Fatalf("get renamed models: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("expected two renamed models, got %#v", models)
	}

	if err := store.DeleteEndpoint("Renamed"); err != nil {
		t.Fatalf("delete endpoint: %v", err)
	}
	models, err = store.GetEndpointModels("Renamed")
	if err != nil {
		t.Fatalf("get deleted models: %v", err)
	}
	if len(models) != 0 {
		t.Fatalf("expected endpoint model rows to be deleted, got %#v", models)
	}
}
```

- [ ] **Step 2: Run storage tests and verify failure**

Run:

```bash
go test ./internal/storage -run 'TestEndpointModelsBackfillLegacyEndpointModel|TestEndpointModelsRenameAndDeleteWithEndpoint' -count=1
```

Expected: FAIL because `EndpointModel`, constants, and storage methods do not exist.

- [ ] **Step 3: Add storage types and constants**

Add to `internal/storage/interface.go` after `type Endpoint`:

```go
const (
	EndpointModelSourceManual     = "manual"
	EndpointModelSourceDiscovered = "discovered"
	EndpointModelSourceLegacy     = "legacy"

	EndpointModelStatusUnknown    = "unknown"
	EndpointModelStatusDiscovered = "discovered"
	EndpointModelStatusVerifying  = "verifying"
	EndpointModelStatusVerified   = "verified"
	EndpointModelStatusFailed     = "failed"
)

type EndpointModel struct {
	ID                    int64      `json:"id"`
	EndpointName          string     `json:"endpointName"`
	ModelID               string     `json:"modelId"`
	DisplayName           string     `json:"displayName"`
	Source                string     `json:"source"`
	Enabled               bool       `json:"enabled"`
	VerificationStatus    string     `json:"verificationStatus"`
	UpstreamTransformer   string     `json:"upstreamTransformer"`
	FailureKind           string     `json:"failureKind"`
	FailureMessage        string     `json:"failureMessage"`
	LastVerifiedAt        *time.Time `json:"lastVerifiedAt,omitempty"`
	VerificationExpiresAt *time.Time `json:"verificationExpiresAt,omitempty"`
	LastAttemptAt         *time.Time `json:"lastAttemptAt,omitempty"`
	NextAttemptAt         *time.Time `json:"nextAttemptAt,omitempty"`
	SortOrder             int        `json:"sortOrder"`
	CreatedAt             time.Time  `json:"createdAt"`
	UpdatedAt             time.Time  `json:"updatedAt"`
}
```

- [ ] **Step 4: Add schema and migration**

In `internal/storage/sqlite.go`, add `endpoint_models` to `initSchema()` after `endpoints`:

```sql
CREATE TABLE IF NOT EXISTS endpoint_models (
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
	last_verified_at DATETIME,
	verification_expires_at DATETIME,
	last_attempt_at DATETIME,
	next_attempt_at DATETIME,
	sort_order INTEGER DEFAULT 0,
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
	updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
	UNIQUE(endpoint_name, model_id)
);
```

At the end of schema initialization, add a helper call:

```go
if err := s.backfillEndpointModelsFromLegacyModel(); err != nil {
	return err
}
```

- [ ] **Step 5: Create endpoint model storage methods**

Create `internal/storage/endpoint_models.go`:

```go
package storage

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

func normalizeEndpointModel(model *EndpointModel) {
	if model == nil {
		return
	}
	model.EndpointName = strings.TrimSpace(model.EndpointName)
	model.ModelID = strings.TrimSpace(model.ModelID)
	if model.Source == "" {
		model.Source = EndpointModelSourceManual
	}
	if model.VerificationStatus == "" {
		model.VerificationStatus = EndpointModelStatusUnknown
	}
	model.UpstreamTransformer = strings.TrimSpace(model.UpstreamTransformer)
}

func (s *SQLiteStorage) UpsertEndpointModel(model *EndpointModel) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.upsertEndpointModelLocked(model)
}

func (s *SQLiteStorage) upsertEndpointModelLocked(model *EndpointModel) error {
	normalizeEndpointModel(model)
	if model == nil {
		return fmt.Errorf("endpoint model is nil")
	}
	if model.EndpointName == "" || model.ModelID == "" {
		return fmt.Errorf("endpoint name and model id are required")
	}
	_, err := s.db.Exec(`
		INSERT INTO endpoint_models (
			endpoint_name, model_id, display_name, source, enabled, verification_status,
			upstream_transformer, failure_kind, failure_message, last_verified_at,
			verification_expires_at, last_attempt_at, next_attempt_at, sort_order
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(endpoint_name, model_id) DO UPDATE SET
			display_name=excluded.display_name,
			source=excluded.source,
			enabled=excluded.enabled,
			verification_status=excluded.verification_status,
			upstream_transformer=excluded.upstream_transformer,
			failure_kind=excluded.failure_kind,
			failure_message=excluded.failure_message,
			last_verified_at=excluded.last_verified_at,
			verification_expires_at=excluded.verification_expires_at,
			last_attempt_at=excluded.last_attempt_at,
			next_attempt_at=excluded.next_attempt_at,
			sort_order=excluded.sort_order,
			updated_at=CURRENT_TIMESTAMP
	`, model.EndpointName, model.ModelID, model.DisplayName, model.Source, model.Enabled, model.VerificationStatus,
		model.UpstreamTransformer, model.FailureKind, model.FailureMessage, model.LastVerifiedAt,
		model.VerificationExpiresAt, model.LastAttemptAt, model.NextAttemptAt, model.SortOrder)
	return err
}

func (s *SQLiteStorage) GetEndpointModels(endpointName string) ([]EndpointModel, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.Query(`
		SELECT id, endpoint_name, model_id, display_name, source, enabled, verification_status,
			COALESCE(upstream_transformer, ''), COALESCE(failure_kind, ''), COALESCE(failure_message, ''),
			last_verified_at, verification_expires_at, last_attempt_at, next_attempt_at,
			sort_order, created_at, updated_at
		FROM endpoint_models
		WHERE endpoint_name=?
		ORDER BY sort_order ASC, model_id ASC
	`, strings.TrimSpace(endpointName))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEndpointModels(rows)
}

func (s *SQLiteStorage) GetVerifiedEndpointModels(modelID string) ([]EndpointModel, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	now := time.Now().UTC()
	rows, err := s.db.Query(`
		SELECT id, endpoint_name, model_id, display_name, source, enabled, verification_status,
			COALESCE(upstream_transformer, ''), COALESCE(failure_kind, ''), COALESCE(failure_message, ''),
			last_verified_at, verification_expires_at, last_attempt_at, next_attempt_at,
			sort_order, created_at, updated_at
		FROM endpoint_models
		WHERE model_id=? AND enabled=TRUE AND verification_status=?
			AND (verification_expires_at IS NULL OR verification_expires_at > ?)
		ORDER BY sort_order ASC, endpoint_name ASC
	`, strings.TrimSpace(modelID), EndpointModelStatusVerified, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEndpointModels(rows)
}

func (s *SQLiteStorage) DeleteEndpointModel(endpointName string, modelID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM endpoint_models WHERE endpoint_name=? AND model_id=?`, strings.TrimSpace(endpointName), strings.TrimSpace(modelID))
	return err
}

func scanEndpointModels(rows *sql.Rows) ([]EndpointModel, error) {
	models := []EndpointModel{}
	for rows.Next() {
		var model EndpointModel
		if err := rows.Scan(&model.ID, &model.EndpointName, &model.ModelID, &model.DisplayName, &model.Source,
			&model.Enabled, &model.VerificationStatus, &model.UpstreamTransformer, &model.FailureKind,
			&model.FailureMessage, &model.LastVerifiedAt, &model.VerificationExpiresAt, &model.LastAttemptAt,
			&model.NextAttemptAt, &model.SortOrder, &model.CreatedAt, &model.UpdatedAt); err != nil {
			return nil, err
		}
		models = append(models, model)
	}
	return models, rows.Err()
}

func (s *SQLiteStorage) backfillEndpointModelsFromLegacyModel() error {
	rows, err := s.db.Query(`
		SELECT name, model, transformer
		FROM endpoints
		WHERE COALESCE(model, '') <> ''
	`)
	if err != nil {
		return err
	}
	defer rows.Close()

	type legacyModel struct {
		endpointName string
		modelID      string
		transformer  string
	}
	legacy := []legacyModel{}
	for rows.Next() {
		var item legacyModel
		if err := rows.Scan(&item.endpointName, &item.modelID, &item.transformer); err != nil {
			return err
		}
		legacy = append(legacy, item)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	expires := time.Now().UTC().Add(24 * time.Hour)
	for _, item := range legacy {
		model := &EndpointModel{
			EndpointName:          item.endpointName,
			ModelID:               item.modelID,
			Source:                EndpointModelSourceLegacy,
			Enabled:               true,
			VerificationStatus:    EndpointModelStatusVerified,
			UpstreamTransformer:   item.transformer,
			VerificationExpiresAt: &expires,
		}
		if err := s.upsertEndpointModelLocked(model); err != nil {
			return err
		}
	}
	return nil
}
```

- [ ] **Step 6: Sync endpoint rename/delete**

Modify `UpdateEndpointByName()` rename queries in `internal/storage/sqlite.go` to include:

```go
`UPDATE endpoint_models SET endpoint_name=? WHERE endpoint_name=?`,
```

Modify `DeleteEndpoint()` before deleting credentials:

```go
if _, err := s.db.Exec(`DELETE FROM endpoint_models WHERE endpoint_name=?`, name); err != nil {
	return err
}
```

- [ ] **Step 7: Run storage tests**

Run:

```bash
go test ./internal/storage -count=1
```

Expected: PASS.

- [ ] **Step 8: Commit storage layer**

```bash
git add internal/storage/interface.go internal/storage/sqlite.go internal/storage/endpoint_models.go internal/storage/sqlite_test.go
git commit -m "feat: add endpoint model storage"
```

## Task 2: Proxy Model Registry

**Files:**
- Create: `internal/proxy/model_registry.go`
- Create: `internal/proxy/model_registry_test.go`
- Modify: `internal/proxy/proxy.go`

- [ ] **Step 1: Add failing registry tests**

Create `internal/proxy/model_registry_test.go`:

```go
package proxy

import (
	"testing"
	"time"

	"github.com/lich0821/ccNexus/internal/storage"
)

type fakeEndpointModelStore struct {
	models []storage.EndpointModel
}

func (s *fakeEndpointModelStore) GetVerifiedEndpointModels(modelID string) ([]storage.EndpointModel, error) {
	out := []storage.EndpointModel{}
	now := time.Now().UTC()
	for _, model := range s.models {
		if model.ModelID != modelID || !model.Enabled || model.VerificationStatus != storage.EndpointModelStatusVerified {
			continue
		}
		if model.VerificationExpiresAt != nil && model.VerificationExpiresAt.Before(now) {
			continue
		}
		out = append(out, model)
	}
	return out, nil
}

func TestModelRegistryReturnsVerifiedEndpointNames(t *testing.T) {
	expires := time.Now().UTC().Add(time.Hour)
	registry := newModelRegistry(&fakeEndpointModelStore{models: []storage.EndpointModel{
		{EndpointName: "A", ModelID: "claude-sonnet-4-5-20250929", Enabled: true, VerificationStatus: storage.EndpointModelStatusVerified, VerificationExpiresAt: &expires, UpstreamTransformer: "claude"},
		{EndpointName: "B", ModelID: "claude-sonnet-4-5-20250929", Enabled: false, VerificationStatus: storage.EndpointModelStatusVerified, VerificationExpiresAt: &expires},
		{EndpointName: "C", ModelID: "claude-sonnet-4-5-20250929", Enabled: true, VerificationStatus: storage.EndpointModelStatusDiscovered, VerificationExpiresAt: &expires},
	}})

	candidates, err := registry.verifiedCandidates("claude-sonnet-4-5-20250929")
	if err != nil {
		t.Fatalf("verified candidates: %v", err)
	}
	if len(candidates) != 1 || candidates[0].EndpointName != "A" || candidates[0].UpstreamTransformer != "claude" {
		t.Fatalf("unexpected candidates: %#v", candidates)
	}
}
```

- [ ] **Step 2: Run registry test and verify failure**

Run:

```bash
go test ./internal/proxy -run TestModelRegistryReturnsVerifiedEndpointNames -count=1
```

Expected: FAIL because `newModelRegistry` and `verifiedCandidates` do not exist.

- [ ] **Step 3: Implement model registry**

Create `internal/proxy/model_registry.go`:

```go
package proxy

import (
	"strings"

	"github.com/lich0821/ccNexus/internal/storage"
)

type endpointModelReader interface {
	GetVerifiedEndpointModels(modelID string) ([]storage.EndpointModel, error)
}

type modelRegistry struct {
	store endpointModelReader
}

func newModelRegistry(store endpointModelReader) *modelRegistry {
	return &modelRegistry{store: store}
}

func (r *modelRegistry) verifiedCandidates(modelID string) ([]storage.EndpointModel, error) {
	if r == nil || r.store == nil {
		return nil, nil
	}
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return nil, nil
	}
	return r.store.GetVerifiedEndpointModels(modelID)
}

func endpointModelCandidateMap(candidates []storage.EndpointModel) map[string]storage.EndpointModel {
	byEndpoint := make(map[string]storage.EndpointModel, len(candidates))
	for _, candidate := range candidates {
		byEndpoint[candidate.EndpointName] = candidate
	}
	return byEndpoint
}
```

- [ ] **Step 4: Wire registry into Proxy**

Modify `internal/proxy/proxy.go` `type Proxy` to add:

```go
modelRegistry *modelRegistry
```

In `New(...)`, after storage is available:

```go
if storage != nil {
	p.modelRegistry = newModelRegistry(storage)
}
```

If `New` returns a struct literal, set the field in that literal.

- [ ] **Step 5: Run registry tests**

Run:

```bash
go test ./internal/proxy -run TestModelRegistryReturnsVerifiedEndpointNames -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit registry skeleton**

```bash
git add internal/proxy/model_registry.go internal/proxy/model_registry_test.go internal/proxy/proxy.go
git commit -m "feat: add model registry"
```

## Task 3: Model-Aware Request Routing

**Files:**
- Modify: `internal/proxy/proxy.go`
- Modify: `internal/proxy/request.go`
- Modify: `internal/proxy/endpoint_resolver.go`
- Modify: `internal/proxy/request_test.go`
- Modify: `internal/proxy/request_local_fallback_test.go`

- [ ] **Step 1: Add failing routing test for endpoint priority inside model candidates**

Append to `internal/proxy/request_test.go`:

```go
func TestHandleProxyFiltersByVerifiedModelThenPreservesEndpointPriority(t *testing.T) {
	primaryHits := 0
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryHits++
		var payload map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode primary payload: %v", err)
		}
		if payload["model"] != "claude-sonnet-4-5-20250929" {
			t.Fatalf("expected requested model to be preserved, got %#v", payload["model"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_primary","type":"message","role":"assistant","content":[{"type":"text","text":"primary ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer primary.Close()

	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("fallback should not be used while primary supports requested model")
	}))
	defer fallback.Close()

	cfg := config.DefaultConfig()
	cfg.UpdateEndpoints([]config.Endpoint{
		{Name: "Primary", APIUrl: primary.URL, APIKey: "test-key", AuthMode: config.AuthModeAPIKey, Enabled: true, Transformer: "claude", Model: "legacy-default"},
		{Name: "Fallback", APIUrl: fallback.URL, APIKey: "test-key", AuthMode: config.AuthModeAPIKey, Enabled: true, Transformer: "claude", Model: "legacy-default"},
	})
	store := &fakeRoutingModelStore{models: []storage.EndpointModel{
		{EndpointName: "Primary", ModelID: "claude-sonnet-4-5-20250929", Enabled: true, VerificationStatus: storage.EndpointModelStatusVerified, UpstreamTransformer: "claude"},
		{EndpointName: "Fallback", ModelID: "claude-sonnet-4-5-20250929", Enabled: true, VerificationStatus: storage.EndpointModelStatusVerified, UpstreamTransformer: "claude"},
	}}
	p := newModelRoutingTestProxy(cfg, primary.Client(), store)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-5-20250929","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	p.handleProxy(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%q", rec.Code, rec.Body.String())
	}
	if primaryHits != 1 {
		t.Fatalf("expected primary hit once, got %d", primaryHits)
	}
}
```

Add helpers in the same file:

```go
type fakeRoutingModelStore struct {
	models []storage.EndpointModel
}

func (s *fakeRoutingModelStore) GetVerifiedEndpointModels(modelID string) ([]storage.EndpointModel, error) {
	out := []storage.EndpointModel{}
	for _, model := range s.models {
		if model.ModelID == modelID && model.Enabled && model.VerificationStatus == storage.EndpointModelStatusVerified {
			out = append(out, model)
		}
	}
	return out, nil
}

func newModelRoutingTestProxy(cfg *config.Config, client *http.Client, store *fakeRoutingModelStore) *Proxy {
	return &Proxy{
		config:                  cfg,
		configEndpointsSnapshot: cloneEndpoints(cfg.GetEndpoints()),
		stats:                   NewStats(&noopStatsStorage{}, "test-device"),
		httpClient:              client,
		activeRequests:          make(map[string]int),
		endpointCtx:             make(map[string]context.Context),
		endpointCancel:          make(map[string]context.CancelFunc),
		currentIndex:            0,
		resolver:                NewEndpointResolverWithFunc(cfg.GetEndpoints),
		retrySleep:              func(time.Duration) {},
		endpointCooldowns:       make(map[string]endpointCooldown),
		runtimeBlockedEndpoints: make(map[string]string),
		modelRegistry:           newModelRegistry(store),
	}
}
```

Add these imports to `internal/proxy/request_test.go` if they are not already present:

```go
import (
	"context"
	"time"

	"github.com/lich0821/ccNexus/internal/storage"
)
```

- [ ] **Step 2: Run routing test and verify failure**

Run:

```bash
go test ./internal/proxy -run TestHandleProxyFiltersByVerifiedModelThenPreservesEndpointPriority -count=1
```

Expected: FAIL because current routing ignores endpoint model registry and still overwrites model with endpoint default.

- [ ] **Step 3: Add filtering helper**

In `internal/proxy/proxy.go`, add:

```go
func filterEndpointsByVerifiedModel(endpoints []config.Endpoint, candidates []storage.EndpointModel) ([]config.Endpoint, map[string]storage.EndpointModel) {
	candidateMap := endpointModelCandidateMap(candidates)
	filtered := make([]config.Endpoint, 0, len(endpoints))
	for _, endpoint := range endpoints {
		if _, ok := candidateMap[endpoint.Name]; ok {
			filtered = append(filtered, endpoint)
		}
	}
	return filtered, candidateMap
}
```

- [ ] **Step 4: Integrate filtering into handleProxy**

In `handleProxy`, after endpoints are loaded and before `requestEndpoints := endpoints`, add logic:

```go
	clientModelForRouting := strings.TrimSpace(streamReq.Model)
	modelCandidatesByEndpoint := map[string]storage.EndpointModel{}
	if clientModelForRouting != "" && !strings.HasPrefix(clientModelForRouting, "@") && p.modelRegistry != nil {
		candidates, err := p.modelRegistry.verifiedCandidates(clientModelForRouting)
		if err != nil {
			WriteError(w, http.StatusInternalServerError, "Failed to load model routing candidates")
			return
		}
		var filtered []config.Endpoint
		filtered, modelCandidatesByEndpoint = filterEndpointsByVerifiedModel(endpoints, candidates)
		if len(filtered) == 0 {
			p.enqueueModelVerification(clientModelForRouting, endpoints)
			WriteError(w, http.StatusBadRequest, "model_not_verified")
			return
		}
		endpoints = filtered
	}
```

Add this queue method in `model_registry.go`. It intentionally does nothing until Task 4 wires the verifier, but it fixes the public call site and keeps request routing deterministic:

```go
func (p *Proxy) enqueueModelVerification(modelID string, endpoints []config.Endpoint) {
	if p == nil || p.modelRegistry == nil {
		return
	}
}
```

- [ ] **Step 5: Preserve requested model per endpoint attempt**

Inside the retry loop after `upstreamEndpoint := endpointForClientFormat(clientFormat, endpoint)`, add:

```go
if modelCandidate, ok := modelCandidatesByEndpoint[endpoint.Name]; ok {
	upstreamEndpoint.Model = modelCandidate.ModelID
	if strings.TrimSpace(modelCandidate.UpstreamTransformer) != "" {
		upstreamEndpoint.Transformer = modelCandidate.UpstreamTransformer
	}
}
```

This keeps `enforceEndpointModelInPayload()` useful while changing the effective model from endpoint default to request model.

- [ ] **Step 6: Re-enable `@endpoint/model` suffix**

In `handleProxy`, when `requestedModelSuffix != ""`, use it as `clientModelForRouting` for specific endpoint validation. Replace the debug-only behavior:

```go
if requestedModelSuffix != "" {
	logger.Debug("[%s] Ignoring model suffix from endpoint selector due endpoint model priority: %s", endpoint.Name, requestedModelSuffix)
}
```

with:

```go
if requestedModelSuffix != "" {
	logger.Debug("[%s] Using endpoint selector model suffix: %s", endpoint.Name, requestedModelSuffix)
}
```

And ensure `upstreamEndpoint.Model` is set to `requestedModelSuffix` only after verifying the selected endpoint has that model.

- [ ] **Step 7: Run routing tests**

Run:

```bash
go test ./internal/proxy -run 'TestHandleProxyFiltersByVerifiedModelThenPreservesEndpointPriority|TestHandleProxyEndpointSelectorModelSuffixDoesNotOverrideEndpointModel' -count=1
```

Expected: new test PASS; old endpoint selector test must be updated or replaced because suffix should now override when verified.

- [ ] **Step 8: Update endpoint selector test**

Replace `TestHandleProxyEndpointSelectorModelSuffixDoesNotOverrideEndpointModel` with:

```go
func TestHandleProxyEndpointSelectorModelSuffixUsesVerifiedModel(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		if payload["model"] != "deepseek-v4-pro" {
			t.Fatalf("expected selector suffix model, got %#v", payload["model"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-test","choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer upstream.Close()

	cfg := config.DefaultConfig()
	cfg.UpdateEndpoints([]config.Endpoint{{Name: "1052-2nd", APIUrl: upstream.URL, APIKey: "test-key", AuthMode: config.AuthModeAPIKey, Enabled: true, Transformer: "deepseek", Model: "fallback-model"}})
	store := &fakeRoutingModelStore{models: []storage.EndpointModel{{EndpointName: "1052-2nd", ModelID: "deepseek-v4-pro", Enabled: true, VerificationStatus: storage.EndpointModelStatusVerified, UpstreamTransformer: "deepseek"}}}
	p := newModelRoutingTestProxy(cfg, upstream.Client(), store)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"@1052-2nd/deepseek-v4-pro","stream":false,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	p.handleProxy(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d body=%q", rec.Code, rec.Body.String())
	}
}
```

- [ ] **Step 9: Run proxy package tests**

Run:

```bash
go test ./internal/proxy -count=1
```

Expected: PASS.

- [ ] **Step 10: Commit model-aware routing**

```bash
git add internal/proxy/proxy.go internal/proxy/request.go internal/proxy/endpoint_resolver.go internal/proxy/request_test.go internal/proxy/request_local_fallback_test.go internal/proxy/model_registry.go internal/proxy/model_registry_test.go
git commit -m "feat: route requests by verified model support"
```

## Task 4: Background Model Verifier

**Files:**
- Create: `internal/proxy/model_verifier.go`
- Create: `internal/proxy/model_verifier_test.go`
- Modify: `internal/proxy/model_registry.go`

- [ ] **Step 1: Add verifier tests**

Create `internal/proxy/model_verifier_test.go`:

```go
package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lich0821/ccNexus/internal/config"
	"github.com/lich0821/ccNexus/internal/storage"
)

func TestVerifyEndpointModelKimiUsesChatCompletionProbe(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("expected chat completions path, got %s", r.URL.Path)
		}
		var body map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["model"] != "kimi-k2.6" {
			t.Fatalf("expected probe model kimi-k2.6, got %#v", body["model"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-test","choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer upstream.Close()

	verifier := newModelVerifier(upstream.Client())
	result := verifier.verifyEndpointModel(config.Endpoint{Name: "kimi", APIUrl: upstream.URL, APIKey: "test-key", AuthMode: config.AuthModeAPIKey, Transformer: "kimi"}, "kimi-k2.6")
	if result.Status != storage.EndpointModelStatusVerified || result.UpstreamTransformer != "kimi" {
		t.Fatalf("unexpected verification result: %#v", result)
	}
}

func TestVerifyEndpointModelClassifiesUnsupportedModel(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"model not found","code":"model_not_found"}}`))
	}))
	defer upstream.Close()

	verifier := newModelVerifier(upstream.Client())
	result := verifier.verifyEndpointModel(config.Endpoint{Name: "openai", APIUrl: upstream.URL, APIKey: "test-key", AuthMode: config.AuthModeAPIKey, Transformer: "openai"}, "missing-model")
	if result.FailureKind != "unsupported_model" || result.Status != storage.EndpointModelStatusFailed {
		t.Fatalf("unexpected unsupported result: %#v", result)
	}
}
```

- [ ] **Step 2: Run verifier tests and verify failure**

Run:

```bash
go test ./internal/proxy -run 'TestVerifyEndpointModelKimiUsesChatCompletionProbe|TestVerifyEndpointModelClassifiesUnsupportedModel' -count=1
```

Expected: FAIL because verifier does not exist.

- [ ] **Step 3: Implement verifier result and constructor**

Create `internal/proxy/model_verifier.go`:

```go
package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/lich0821/ccNexus/internal/config"
	"github.com/lich0821/ccNexus/internal/providercompat"
	"github.com/lich0821/ccNexus/internal/storage"
)

type modelVerificationResult struct {
	Status              string
	UpstreamTransformer string
	FailureKind         string
	FailureMessage      string
	VerifiedTTL         time.Duration
	RetryTTL            time.Duration
}

type modelVerifier struct {
	client *http.Client
}

func newModelVerifier(client *http.Client) *modelVerifier {
	if client == nil {
		client = http.DefaultClient
	}
	return &modelVerifier{client: client}
}
```

- [ ] **Step 4: Implement probe selection**

Add to `model_verifier.go`:

```go
func (v *modelVerifier) verifyEndpointModel(endpoint config.Endpoint, modelID string) modelVerificationResult {
	transformer := providercompat.NormalizeTransformer(endpoint.Transformer)
	if transformer == "auto" || transformer == "" {
		transformer = providercompat.InferEndpointTransformer(endpoint.APIUrl, modelID, endpoint.Transformer)
	}
	switch transformer {
	case providercompat.TransformerClaude:
		return v.verifyClaude(endpoint, modelID)
	case providercompat.TransformerOpenAI2:
		return v.verifyOpenAIResponses(endpoint, modelID, transformer)
	case providercompat.TransformerOpenAI, providercompat.TransformerDeepSeek, providercompat.TransformerKimi:
		return v.verifyOpenAIChat(endpoint, modelID, transformer)
	case providercompat.TransformerGemini:
		return v.verifyGemini(endpoint, modelID)
	default:
		return modelVerificationResult{Status: storage.EndpointModelStatusFailed, FailureKind: "unsupported_transformer", FailureMessage: transformer, RetryTTL: 7 * 24 * time.Hour}
	}
}
```

- [ ] **Step 5: Implement OpenAI Chat/Kimi probe**

Add:

```go
func (v *modelVerifier) verifyOpenAIChat(endpoint config.Endpoint, modelID string, transformer string) modelVerificationResult {
	payload := []byte(fmt.Sprintf(`{"model":%q,"stream":false,"messages":[{"role":"user","content":"ping"}],"max_tokens":1}`, modelID))
	target := providercompat.JoinBaseURLAndPath(endpoint.APIUrl, providercompat.OpenAIChatTargetPath(transformer, endpoint.APIUrl))
	req, err := http.NewRequest(http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		return transientVerificationFailure("network_error", err.Error())
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(endpoint.APIKey))
	resp, err := v.client.Do(req)
	if err != nil {
		return transientVerificationFailure("network_error", err.Error())
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return classifyVerificationHTTPFailure(resp.StatusCode)
	}
	var body struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || len(body.Choices) == 0 {
		return transientVerificationFailure("invalid_response", "missing chat choices")
	}
	return modelVerificationResult{Status: storage.EndpointModelStatusVerified, UpstreamTransformer: transformer, VerifiedTTL: 24 * time.Hour}
}
```

- [ ] **Step 6: Implement remaining probes**

Implement `verifyClaude`, `verifyOpenAIResponses`, and `verifyGemini` with the same pattern:

```go
func (v *modelVerifier) verifyClaude(endpoint config.Endpoint, modelID string) modelVerificationResult {
	payload := []byte(fmt.Sprintf(`{"model":%q,"max_tokens":1,"messages":[{"role":"user","content":"ping"}]}`, modelID))
	target := providercompat.JoinBaseURLAndPath(endpoint.APIUrl, "/v1/messages")
	req, err := http.NewRequest(http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		return transientVerificationFailure("network_error", err.Error())
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", strings.TrimSpace(endpoint.APIKey))
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(endpoint.APIKey))
	resp, err := v.client.Do(req)
	if err != nil {
		return transientVerificationFailure("network_error", err.Error())
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return classifyVerificationHTTPFailure(resp.StatusCode)
	}
	var body struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || len(body.Content) == 0 {
		return transientVerificationFailure("invalid_response", "missing claude content")
	}
	return modelVerificationResult{Status: storage.EndpointModelStatusVerified, UpstreamTransformer: providercompat.TransformerClaude, VerifiedTTL: 24 * time.Hour}
}
```

Add the OpenAI Responses probe:

```go
func (v *modelVerifier) verifyOpenAIResponses(endpoint config.Endpoint, modelID string, transformer string) modelVerificationResult {
	payload := []byte(fmt.Sprintf(`{"model":%q,"stream":false,"input":"ping","max_output_tokens":1}`, modelID))
	target := providercompat.JoinBaseURLAndPath(endpoint.APIUrl, "/v1/responses")
	req, err := http.NewRequest(http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		return transientVerificationFailure("network_error", err.Error())
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(endpoint.APIKey))
	resp, err := v.client.Do(req)
	if err != nil {
		return transientVerificationFailure("network_error", err.Error())
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return classifyVerificationHTTPFailure(resp.StatusCode)
	}
	var body struct {
		Output []struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || len(body.Output) == 0 {
		return transientVerificationFailure("invalid_response", "missing responses output")
	}
	return modelVerificationResult{Status: storage.EndpointModelStatusVerified, UpstreamTransformer: transformer, VerifiedTTL: 24 * time.Hour}
}
```

Add the Gemini probe:

```go
func (v *modelVerifier) verifyGemini(endpoint config.Endpoint, modelID string) modelVerificationResult {
	payload := []byte(`{"contents":[{"role":"user","parts":[{"text":"ping"}]}],"generationConfig":{"maxOutputTokens":1}}`)
	target := providercompat.JoinBaseURLAndPath(endpoint.APIUrl, fmt.Sprintf("/v1beta/models/%s:generateContent", modelID))
	req, err := http.NewRequest(http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		return transientVerificationFailure("network_error", err.Error())
	}
	req.Header.Set("Content-Type", "application/json")
	q := req.URL.Query()
	q.Set("key", strings.TrimSpace(endpoint.APIKey))
	req.URL.RawQuery = q.Encode()
	resp, err := v.client.Do(req)
	if err != nil {
		return transientVerificationFailure("network_error", err.Error())
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return classifyVerificationHTTPFailure(resp.StatusCode)
	}
	var body struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || len(body.Candidates) == 0 {
		return transientVerificationFailure("invalid_response", "missing gemini candidates")
	}
	return modelVerificationResult{Status: storage.EndpointModelStatusVerified, UpstreamTransformer: providercompat.TransformerGemini, VerifiedTTL: 24 * time.Hour}
}
```

- [ ] **Step 7: Add failure classification**

Add:

```go
func classifyVerificationHTTPFailure(statusCode int) modelVerificationResult {
	switch statusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return modelVerificationResult{Status: storage.EndpointModelStatusFailed, FailureKind: "auth_failed", RetryTTL: 24 * time.Hour}
	case http.StatusTooManyRequests:
		return modelVerificationResult{Status: storage.EndpointModelStatusFailed, FailureKind: "quota_limited", RetryTTL: 30 * time.Minute}
	case http.StatusBadRequest, http.StatusNotFound:
		return modelVerificationResult{Status: storage.EndpointModelStatusFailed, FailureKind: "unsupported_model", RetryTTL: 7 * 24 * time.Hour}
	default:
		if statusCode >= 500 {
			return modelVerificationResult{Status: storage.EndpointModelStatusFailed, FailureKind: "upstream_error", RetryTTL: 10 * time.Minute}
		}
		return modelVerificationResult{Status: storage.EndpointModelStatusFailed, FailureKind: "invalid_response", RetryTTL: 30 * time.Minute}
	}
}

func transientVerificationFailure(kind string, message string) modelVerificationResult {
	return modelVerificationResult{Status: storage.EndpointModelStatusFailed, FailureKind: kind, FailureMessage: message, RetryTTL: 10 * time.Minute}
}
```

- [ ] **Step 8: Run verifier tests**

Run:

```bash
go test ./internal/proxy -run 'TestVerifyEndpointModel' -count=1
```

Expected: PASS.

- [ ] **Step 9: Commit verifier**

```bash
git add internal/proxy/model_verifier.go internal/proxy/model_verifier_test.go internal/proxy/model_registry.go
git commit -m "feat: verify endpoint models with probes"
```

## Task 5: `/v1/models` Uses Verified Endpoint Models

**Files:**
- Modify: `internal/proxy/models.go`
- Modify: `internal/proxy/models_test.go`

- [ ] **Step 1: Add failing models API test**

Append to `internal/proxy/models_test.go`:

```go
func TestLoadModelsReturnsOnlyVerifiedEndpointModels(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.UpdateEndpoints([]config.Endpoint{
		{Name: "ClaudeA", APIUrl: "https://a.example.com", APIKey: "test-key", AuthMode: config.AuthModeAPIKey, Enabled: true, Transformer: "claude", Model: "legacy-default"},
		{Name: "ClaudeB", APIUrl: "https://b.example.com", APIKey: "test-key", AuthMode: config.AuthModeAPIKey, Enabled: true, Transformer: "claude", Model: "legacy-default"},
	})
	store := &fakeRoutingModelStore{models: []storage.EndpointModel{
		{EndpointName: "ClaudeA", ModelID: "claude-sonnet-4-5-20250929", Enabled: true, VerificationStatus: storage.EndpointModelStatusVerified, UpstreamTransformer: "claude"},
		{EndpointName: "ClaudeB", ModelID: "claude-sonnet-4-5-20250929", Enabled: true, VerificationStatus: storage.EndpointModelStatusDiscovered, UpstreamTransformer: "claude"},
	}}
	p := &Proxy{config: cfg, modelsCache: NewModelsCache(30), modelRegistry: newModelRegistry(store)}

	models := p.loadModelsForResponse(true)

	if len(models) != 1 {
		t.Fatalf("expected one verified model, got %#v", models)
	}
	if models[0].ID != "claude-sonnet-4-5-20250929" || models[0].EndpointID != "ClaudeA" {
		t.Fatalf("unexpected verified model response: %#v", models[0])
	}
}
```

Add `internal/storage` import.

- [ ] **Step 2: Run test and verify failure**

Run:

```bash
go test ./internal/proxy -run TestLoadModelsReturnsOnlyVerifiedEndpointModels -count=1
```

Expected: FAIL because `loadModelsForResponse` still uses endpoint default model.

- [ ] **Step 3: Add registry method for all verified models**

Extend `endpointModelReader` and `modelRegistry`:

```go
type endpointModelReader interface {
	GetVerifiedEndpointModels(modelID string) ([]storage.EndpointModel, error)
	GetEndpointModels(endpointName string) ([]storage.EndpointModel, error)
}

func (r *modelRegistry) verifiedModelsForEndpoint(endpointName string) ([]storage.EndpointModel, error) {
	if r == nil || r.store == nil {
		return nil, nil
	}
	models, err := r.store.GetEndpointModels(endpointName)
	if err != nil {
		return nil, err
	}
	out := []storage.EndpointModel{}
	for _, model := range models {
		if model.Enabled && model.VerificationStatus == storage.EndpointModelStatusVerified {
			out = append(out, model)
		}
	}
	return out, nil
}
```

Update test fakes to implement `GetEndpointModels`.

- [ ] **Step 4: Modify `getModelsForEndpoint`**

At the top of `getModelsForEndpoint(ep config.Endpoint)`:

```go
if p.modelRegistry != nil {
	verified, err := p.modelRegistry.verifiedModelsForEndpoint(ep.Name)
	if err == nil && len(verified) > 0 {
		models := make([]ModelInfo, 0, len(verified))
		for _, model := range verified {
			models = append(models, ModelInfo{
				ID:         model.ModelID,
				Object:     "model",
				Created:    time.Now().Unix(),
				OwnedBy:    providercompat.Owner(model.UpstreamTransformer),
				EndpointID: model.EndpointName,
			})
		}
		return models, true
	}
}
```

Keep legacy fallback for endpoints without registry rows.

- [ ] **Step 5: Run models tests**

Run:

```bash
go test ./internal/proxy -run 'TestLoadModels' -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit models API behavior**

```bash
git add internal/proxy/models.go internal/proxy/models_test.go internal/proxy/model_registry.go
git commit -m "feat: expose verified endpoint models"
```

## Task 6: Web API for Endpoint Models

**Files:**
- Modify: `cmd/server/webui/api/handler.go`
- Create: `cmd/server/webui/api/endpoint_models.go`
- Create: `cmd/server/webui/api/endpoint_models_test.go`

- [ ] **Step 1: Add handler tests**

Create `cmd/server/webui/api/endpoint_models_test.go`:

```go
package api

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lich0821/ccNexus/internal/config"
	"github.com/lich0821/ccNexus/internal/storage"
)

func TestEndpointModelsAPIAddsAndListsModel(t *testing.T) {
	store, err := storage.NewSQLiteStorage(filepath.Join(t.TempDir(), "ccnexus-test.db"))
	if err != nil {
		t.Fatalf("new storage: %v", err)
	}
	defer store.Close()
	if err := store.SaveEndpoint(&storage.Endpoint{Name: "Claude", APIUrl: "https://api.anthropic.com", APIKey: "test-key", AuthMode: config.AuthModeAPIKey, Enabled: true, Transformer: "claude"}); err != nil {
		t.Fatalf("save endpoint: %v", err)
	}
	cfg := config.DefaultConfig()
	cfg.BasicAuthEnabled = false
	handler := NewHandler(cfg, nil, store)

	req := httptest.NewRequest(http.MethodPost, "/api/endpoints/Claude/models", strings.NewReader(`{"modelId":"claude-sonnet-4-5-20250929","enabled":true}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected add model 200, got %d body=%q", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/endpoints/Claude/models", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "claude-sonnet-4-5-20250929") {
		t.Fatalf("expected listed model, status=%d body=%q", rec.Code, rec.Body.String())
	}
}
```

- [ ] **Step 2: Run handler test and verify failure**

Run:

```bash
go test ./cmd/server/webui/api -run TestEndpointModelsAPIAddsAndListsModel -count=1
```

Expected: FAIL because endpoint model routes do not exist.

- [ ] **Step 3: Add route dispatch**

In `handler.go`, before generic `handleEndpointByName`, route model paths:

```go
if strings.HasPrefix(path, "/api/endpoints/") && strings.Contains(path, "/models") {
	authMiddleware(http.HandlerFunc(h.handleEndpointModels)).ServeHTTP(w, r)
	return
}
```

- [ ] **Step 4: Implement endpoint model handlers**

Create `cmd/server/webui/api/endpoint_models.go`:

```go
package api

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"github.com/lich0821/ccNexus/internal/storage"
)

func (h *Handler) handleEndpointModels(w http.ResponseWriter, r *http.Request) {
	endpointName, modelID, ok := parseEndpointModelPath(strings.TrimPrefix(r.URL.Path, "/api/endpoints/"))
	if !ok {
		WriteError(w, http.StatusNotFound, "Invalid endpoint model path")
		return
	}
	switch {
	case modelID == "" && r.Method == http.MethodGet:
		models, err := h.storage.GetEndpointModels(endpointName)
		if err != nil {
			WriteError(w, http.StatusInternalServerError, "Failed to load endpoint models")
			return
		}
		WriteSuccess(w, map[string]interface{}{"models": models})
	case modelID == "" && r.Method == http.MethodPost:
		var req struct {
			ModelID string `json:"modelId"`
			Enabled bool   `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			WriteError(w, http.StatusBadRequest, "Invalid request body")
			return
		}
		model := &storage.EndpointModel{EndpointName: endpointName, ModelID: req.ModelID, Source: storage.EndpointModelSourceManual, Enabled: req.Enabled, VerificationStatus: storage.EndpointModelStatusUnknown}
		if err := h.storage.UpsertEndpointModel(model); err != nil {
			WriteError(w, http.StatusInternalServerError, "Failed to save endpoint model")
			return
		}
		WriteSuccess(w, map[string]interface{}{"model": model})
	default:
		WriteError(w, http.StatusMethodNotAllowed, "Method not allowed")
	}
}

func parseEndpointModelPath(path string) (string, string, bool) {
	parts := strings.Split(path, "/")
	if len(parts) < 2 || parts[1] != "models" {
		return "", "", false
	}
	endpointName, err := url.PathUnescape(parts[0])
	if err != nil || strings.TrimSpace(endpointName) == "" {
		return "", "", false
	}
	if len(parts) == 2 {
		return endpointName, "", true
	}
	modelID, err := url.PathUnescape(strings.Join(parts[2:], "/"))
	if err != nil {
		return "", "", false
	}
	return endpointName, modelID, true
}
```

- [ ] **Step 5: Run API test**

Run:

```bash
go test ./cmd/server/webui/api -run TestEndpointModelsAPIAddsAndListsModel -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit API handlers**

```bash
git add cmd/server/webui/api/handler.go cmd/server/webui/api/endpoint_models.go cmd/server/webui/api/endpoint_models_test.go
git commit -m "feat: add endpoint model API"
```

## Task 7: Web UI Model Management

**Files:**
- Modify: `cmd/server/webui/ui/js/api.js`
- Modify: `cmd/server/webui/ui/js/components/endpoints.js`
- Modify: `cmd/server/webui/ui/js/i18n/en.js`
- Modify: `cmd/server/webui/ui/js/i18n/zh-CN.js`

- [ ] **Step 1: Add API client methods**

In `cmd/server/webui/ui/js/api.js` after `fetchModels(...)`:

```js
    async getEndpointModels(name) {
        return this.request('GET', `/endpoints/${encodeURIComponent(name)}/models`);
    }

    async addEndpointModel(name, data) {
        return this.request('POST', `/endpoints/${encodeURIComponent(name)}/models`, data);
    }

    async deleteEndpointModel(name, modelId) {
        return this.request('DELETE', `/endpoints/${encodeURIComponent(name)}/models/${encodeURIComponent(modelId)}`);
    }

    async verifyEndpointModel(name, modelId) {
        return this.request('POST', `/endpoints/${encodeURIComponent(name)}/models/${encodeURIComponent(modelId)}/verify`);
    }
```

- [ ] **Step 2: Add model management UI entry**

In `endpoints.js`, add a button beside each endpoint row:

```js
`<button class="btn btn-secondary btn-sm" data-action="models" data-name="${this.escapeHtml(endpoint.name)}">${t('endpoints.models')}</button>`
```

Wire the click handler to:

```js
if (action === 'models') {
    this.showEndpointModelsModal(name);
}
```

- [ ] **Step 3: Add modal implementation**

Add method to `Endpoints`:

```js
    async showEndpointModelsModal(name) {
        const modalContainer = document.getElementById('modal-container');
        const result = await api.getEndpointModels(name);
        const models = result.models || [];
        modalContainer.innerHTML = `
            <div class="modal-overlay">
                <div class="modal" style="max-width: 760px;">
                    <div class="modal-header">
                        <h3 class="modal-title">${this.escapeHtml(name)} ${t('endpoints.models')}</h3>
                        <button class="modal-close" id="close-modal">×</button>
                    </div>
                    <div class="modal-body">
                        <div class="form-group">
                            <label class="form-label">${t('endpoints.addModel')}</label>
                            <div style="display:flex; gap:8px;">
                                <input class="form-input" id="endpoint-model-input" placeholder="${t('endpoints.modelPlaceholder')}">
                                <button class="btn btn-primary" id="add-endpoint-model">${t('common.add')}</button>
                            </div>
                        </div>
                        <table class="table">
                            <thead><tr><th>${t('endpoints.model')}</th><th>${t('common.status')}</th><th>${t('common.enabled')}</th><th>${t('common.actions')}</th></tr></thead>
                            <tbody>
                                ${models.map(model => `
                                    <tr>
                                        <td>${this.escapeHtml(model.modelId)}</td>
                                        <td>${this.escapeHtml(model.verificationStatus || 'unknown')}</td>
                                        <td>${model.enabled ? t('common.yes') : t('common.no')}</td>
                                        <td>
                                            <button class="btn btn-secondary btn-sm verify-model" data-model="${this.escapeHtml(model.modelId)}">${t('endpoints.verifyModel')}</button>
                                        </td>
                                    </tr>
                                `).join('')}
                            </tbody>
                        </table>
                    </div>
                    <div class="modal-footer">
                        <button class="btn btn-secondary" id="close-btn">${t('common.close')}</button>
                    </div>
                </div>
            </div>
        `;
        document.getElementById('close-modal').addEventListener('click', () => this.closeModal());
        document.getElementById('close-btn').addEventListener('click', () => this.closeModal());
        document.getElementById('add-endpoint-model').addEventListener('click', async () => {
            const input = document.getElementById('endpoint-model-input');
            const modelId = input.value.trim();
            if (!modelId) return;
            await api.addEndpointModel(name, { modelId, enabled: true });
            await this.showEndpointModelsModal(name);
        });
        document.querySelectorAll('.verify-model').forEach(button => {
            button.addEventListener('click', async () => {
                await api.verifyEndpointModel(name, button.dataset.model);
                await this.showEndpointModelsModal(name);
            });
        });
    }
```

- [ ] **Step 4: Add i18n strings**

In both locale files under `endpoints`, add keys:

```js
models: 'Models',
addModel: 'Add Model',
verifyModel: 'Verify',
```

Chinese:

```js
models: '模型',
addModel: '添加模型',
verifyModel: '验证',
```

Add `yes/no/actions` under `common` if missing:

```js
yes: 'Yes',
no: 'No',
actions: 'Actions',
```

Chinese:

```js
yes: '是',
no: '否',
actions: '操作',
```

- [ ] **Step 5: Run focused API and proxy tests**

Run:

```bash
go test ./cmd/server/webui/api ./internal/proxy -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit UI model management**

```bash
git add cmd/server/webui/ui/js/api.js cmd/server/webui/ui/js/components/endpoints.js cmd/server/webui/ui/js/i18n/en.js cmd/server/webui/ui/js/i18n/zh-CN.js
git commit -m "feat: manage endpoint models in web ui"
```

## Task 8: Final Integration and Verification

**Files:**
- Review all changed files from Tasks 1-7.
- Modify docs only if implementation diverges from spec.

- [ ] **Step 1: Run formatting**

Run:

```bash
gofmt -w internal/storage/interface.go internal/storage/sqlite.go internal/storage/endpoint_models.go internal/proxy/model_registry.go internal/proxy/model_verifier.go internal/proxy/proxy.go internal/proxy/request.go internal/proxy/endpoint_resolver.go internal/proxy/models.go cmd/server/webui/api/handler.go cmd/server/webui/api/endpoint_models.go
```

Expected: command exits 0.

- [ ] **Step 2: Run focused package tests**

Run:

```bash
go test ./internal/storage ./internal/proxy ./cmd/server/webui/api -count=1
```

Expected: PASS.

- [ ] **Step 3: Run diff checks**

Run:

```bash
git diff --check
git status --short --branch
```

Expected: no whitespace errors; only intended implementation files are modified.

- [ ] **Step 4: Optional full test gate**

Ask for explicit confirmation before running:

```bash
go test ./... -count=1
```

Expected if approved: PASS, except for any already-known repository-wide limitations that must be reported.

- [ ] **Step 5: Final implementation commit**

If Step 3 shows uncommitted integration fixes, inspect `git status --short`, stage the listed files explicitly, and commit them. For example, if only docs and tests changed:

```bash
git add docs/superpowers/specs/2026-05-17-provider-model-routing-design.md docs/superpowers/plans/2026-05-17-provider-model-routing.md internal/proxy/models_test.go
git commit -m "test: cover provider model routing"
```

Expected: working tree clean after final commit. If Step 3 shows no uncommitted files, skip this commit step.

## Self-Review Checklist

- Storage task covers new table, backfill, rename, delete, and CRUD.
- Registry task covers verified candidate lookup and excludes unverified models.
- Routing task covers model filtering before failover and preserves endpoint priority.
- Verifier task covers real provider probes and failure classification.
- Models API task exposes only verified enabled models.
- Web API/UI task lets users manage endpoint model rows.
- Final task includes formatting, focused tests, and an explicit gate before full `go test ./...`.
