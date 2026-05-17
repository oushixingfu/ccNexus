package storage

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func TestMigrateDeepSeekThinkingDefaultRunsOnce(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ccnexus.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	_, err = db.Exec(`
		CREATE TABLE endpoints (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT UNIQUE NOT NULL,
			api_url TEXT NOT NULL,
			api_key TEXT NOT NULL,
			auth_mode TEXT NOT NULL DEFAULT 'api_key',
			enabled BOOLEAN DEFAULT TRUE,
			transformer TEXT DEFAULT 'claude',
			model TEXT,
			thinking TEXT DEFAULT 'off',
			force_stream BOOLEAN DEFAULT FALSE,
			remark TEXT,
			sort_order INTEGER DEFAULT 0,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);
		CREATE TABLE app_config (
			key TEXT PRIMARY KEY,
			value TEXT,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);
		INSERT INTO endpoints (name, api_url, api_key, auth_mode, enabled, transformer, model, thinking, remark)
		VALUES
			('deepseek-old', 'https://api.deepseek.com', 'key', 'api_key', TRUE, 'deepseek', 'deepseek-chat', 'off', ''),
			('openai-old', 'https://api.openai.com', 'key', 'api_key', TRUE, 'openai', 'gpt-4', 'off', ''),
			('deepseek-high', 'https://api.deepseek.com', 'key', 'api_key', TRUE, 'deepseek', 'deepseek-chat', 'high', '');
	`)
	if err != nil {
		t.Fatalf("seed database: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close seed database: %v", err)
	}

	store, err := NewSQLiteStorage(dbPath)
	if err != nil {
		t.Fatalf("open storage: %v", err)
	}
	endpoints, err := store.GetEndpoints()
	if err != nil {
		t.Fatalf("get endpoints: %v", err)
	}
	thinkingByName := map[string]string{}
	for _, ep := range endpoints {
		thinkingByName[ep.Name] = ep.Thinking
	}
	if got := thinkingByName["deepseek-old"]; got != "" {
		t.Fatalf("expected old DeepSeek off to migrate to provider default, got %q", got)
	}
	if got := thinkingByName["openai-old"]; got != "off" {
		t.Fatalf("expected OpenAI off to stay off, got %q", got)
	}
	if got := thinkingByName["deepseek-high"]; got != "high" {
		t.Fatalf("expected DeepSeek high to stay high, got %q", got)
	}
	if marker, err := store.GetConfig(deepSeekThinkingDefaultMigrationKey); err != nil || marker != "done" {
		t.Fatalf("expected migration marker done, got %q err=%v", marker, err)
	}

	if _, err := store.db.Exec(`UPDATE endpoints SET thinking='off' WHERE name='deepseek-old'`); err != nil {
		t.Fatalf("set explicit off after migration: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close storage: %v", err)
	}

	store, err = NewSQLiteStorage(dbPath)
	if err != nil {
		t.Fatalf("reopen storage: %v", err)
	}
	defer store.Close()
	endpoints, err = store.GetEndpoints()
	if err != nil {
		t.Fatalf("get endpoints after reopen: %v", err)
	}
	for _, ep := range endpoints {
		if ep.Name == "deepseek-old" && ep.Thinking != "off" {
			t.Fatalf("expected explicit DeepSeek off to survive after marker, got %q", ep.Thinking)
		}
	}
}

func TestEndpointRuntimeStatusPersistsAcrossReopen(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ccnexus.db")
	store, err := NewSQLiteStorage(dbPath)
	if err != nil {
		t.Fatalf("open storage: %v", err)
	}

	successAt := time.Date(2026, 5, 8, 9, 10, 11, 0, time.UTC)
	attemptAt := successAt.Add(-time.Second)
	status, err := store.UpsertEndpointRuntimeStatus("Primary", EndpointRuntimeStatusPatch{
		LastSuccessAt: &successAt,
		LastAttemptAt: &attemptAt,
	})
	if err != nil {
		t.Fatalf("upsert success status: %v", err)
	}
	if status.LastSuccessAt == nil || !status.LastSuccessAt.Equal(successAt) {
		t.Fatalf("expected success time %s, got %#v", successAt, status.LastSuccessAt)
	}

	failureAt := successAt.Add(time.Minute)
	reason := "upstream_5xx"
	statusCode := 500
	status, err = store.UpsertEndpointRuntimeStatus("Primary", EndpointRuntimeStatusPatch{
		LastFailureAt:         &failureAt,
		LastFailureReason:     &reason,
		LastFailureStatusCode: &statusCode,
	})
	if err != nil {
		t.Fatalf("upsert failure status: %v", err)
	}
	if status.LastSuccessAt == nil || !status.LastSuccessAt.Equal(successAt) {
		t.Fatalf("expected success time to be preserved, got %#v", status.LastSuccessAt)
	}
	if status.LastFailureAt == nil || !status.LastFailureAt.Equal(failureAt) {
		t.Fatalf("expected failure time %s, got %#v", failureAt, status.LastFailureAt)
	}
	if status.LastFailureReason != reason {
		t.Fatalf("expected failure reason %q, got %q", reason, status.LastFailureReason)
	}
	if status.LastFailureStatusCode != statusCode {
		t.Fatalf("expected failure status code %d, got %d", statusCode, status.LastFailureStatusCode)
	}

	if err := store.Close(); err != nil {
		t.Fatalf("close storage: %v", err)
	}

	store, err = NewSQLiteStorage(dbPath)
	if err != nil {
		t.Fatalf("reopen storage: %v", err)
	}
	defer store.Close()

	statuses, err := store.GetEndpointRuntimeStatuses()
	if err != nil {
		t.Fatalf("get runtime statuses: %v", err)
	}
	status = statuses["Primary"]
	if status == nil {
		t.Fatalf("expected Primary runtime status after reopen")
	}
	if status.LastSuccessAt == nil || !status.LastSuccessAt.Equal(successAt) {
		t.Fatalf("expected persisted success time %s, got %#v", successAt, status.LastSuccessAt)
	}
	if status.LastFailureAt == nil || !status.LastFailureAt.Equal(failureAt) {
		t.Fatalf("expected persisted failure time %s, got %#v", failureAt, status.LastFailureAt)
	}
	if status.LastFailureReason != reason {
		t.Fatalf("expected persisted failure reason %q, got %q", reason, status.LastFailureReason)
	}
	if status.LastFailureStatusCode != statusCode {
		t.Fatalf("expected persisted failure status code %d, got %d", statusCode, status.LastFailureStatusCode)
	}

	nonHTTPFailureAt := failureAt.Add(time.Minute)
	nonHTTPReason := "transient_network_error"
	emptyStatusCode := 0
	status, err = store.UpsertEndpointRuntimeStatus("Primary", EndpointRuntimeStatusPatch{
		LastFailureAt:         &nonHTTPFailureAt,
		LastFailureReason:     &nonHTTPReason,
		LastFailureStatusCode: &emptyStatusCode,
	})
	if err != nil {
		t.Fatalf("upsert non-http failure status: %v", err)
	}
	if status.LastFailureStatusCode != 0 {
		t.Fatalf("expected non-http failure to clear status code, got %d", status.LastFailureStatusCode)
	}
}

func TestClearStatsDeletesDailyStats(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ccnexus.db")
	store, err := NewSQLiteStorage(dbPath)
	if err != nil {
		t.Fatalf("open storage: %v", err)
	}
	defer store.Close()

	stats := []*DailyStat{
		{EndpointName: "A", Date: "2026-05-11", Requests: 3, Errors: 1, InputTokens: 10, OutputTokens: 20, DeviceID: "device-a"},
		{EndpointName: "B", Date: "2026-05-11", Requests: 2, Errors: 0, InputTokens: 7, OutputTokens: 11, DeviceID: "device-b"},
	}
	for _, stat := range stats {
		if err := store.RecordDailyStat(stat); err != nil {
			t.Fatalf("record stat: %v", err)
		}
	}

	total, endpoints, err := store.GetTotalStats()
	if err != nil {
		t.Fatalf("get total stats before clear: %v", err)
	}
	if total != 5 || len(endpoints) != 2 {
		t.Fatalf("expected seeded stats before clear, total=%d endpoints=%d", total, len(endpoints))
	}

	if err := store.ClearStats(); err != nil {
		t.Fatalf("clear stats: %v", err)
	}

	total, endpoints, err = store.GetTotalStats()
	if err != nil {
		t.Fatalf("get total stats after clear: %v", err)
	}
	if total != 0 || len(endpoints) != 0 {
		t.Fatalf("expected empty stats after clear, total=%d endpoints=%d", total, len(endpoints))
	}

	allStats, err := store.GetAllStats()
	if err != nil {
		t.Fatalf("get all stats after clear: %v", err)
	}
	if len(allStats) != 0 {
		t.Fatalf("expected no daily stats after clear, got %d endpoints", len(allStats))
	}
}

func TestUpdateEndpointByNameRenamesReferences(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ccnexus.db")
	store, err := NewSQLiteStorage(dbPath)
	if err != nil {
		t.Fatalf("open storage: %v", err)
	}
	defer store.Close()

	endpoint := &Endpoint{
		Name:        "Primary",
		APIUrl:      "https://primary.example.com",
		APIKey:      "key",
		AuthMode:    "api_key",
		Enabled:     true,
		Transformer: "openai2",
		Model:       "gpt-test",
		SortOrder:   1,
	}
	if err := store.SaveEndpoint(endpoint); err != nil {
		t.Fatalf("save endpoint: %v", err)
	}
	if err := store.SetConfig("current_endpoint", "Primary"); err != nil {
		t.Fatalf("set current endpoint: %v", err)
	}

	credential := &EndpointCredential{
		EndpointName: "Primary",
		ProviderType: "codex",
		AccessToken:  "access-token",
		Status:       "active",
		Enabled:      true,
	}
	if err := store.SaveEndpointCredential(credential); err != nil {
		t.Fatalf("save credential: %v", err)
	}
	if err := store.UpsertCredentialUsage(credential.ID, "Primary", 3, 1, 11, 17, time.Now()); err != nil {
		t.Fatalf("upsert credential usage: %v", err)
	}
	successAt := time.Date(2026, 5, 12, 9, 0, 0, 0, time.UTC)
	if _, err := store.UpsertEndpointRuntimeStatus("Primary", EndpointRuntimeStatusPatch{LastSuccessAt: &successAt}); err != nil {
		t.Fatalf("upsert runtime status: %v", err)
	}
	if err := store.RecordDailyStat(&DailyStat{
		EndpointName: "Primary",
		Date:         "2026-05-12",
		Requests:     4,
		Errors:       1,
		InputTokens:  21,
		OutputTokens: 34,
		DeviceID:     "test-device",
	}); err != nil {
		t.Fatalf("record daily stat: %v", err)
	}

	endpoint.Name = "Renamed"
	endpoint.Model = "gpt-renamed"
	if err := store.UpdateEndpointByName("Primary", endpoint); err != nil {
		t.Fatalf("rename endpoint: %v", err)
	}

	endpoints, err := store.GetEndpoints()
	if err != nil {
		t.Fatalf("get endpoints: %v", err)
	}
	endpointNames := map[string]bool{}
	for _, ep := range endpoints {
		endpointNames[ep.Name] = true
		if ep.Name == "Renamed" && ep.Model != "gpt-renamed" {
			t.Fatalf("expected renamed endpoint model to update, got %q", ep.Model)
		}
	}
	if endpointNames["Primary"] || !endpointNames["Renamed"] {
		t.Fatalf("expected endpoint name to move from Primary to Renamed, got %#v", endpointNames)
	}
	currentEndpoint, err := store.GetConfig("current_endpoint")
	if err != nil {
		t.Fatalf("get current endpoint: %v", err)
	}
	if currentEndpoint != "Renamed" {
		t.Fatalf("expected current endpoint to be Renamed, got %q", currentEndpoint)
	}

	oldCreds, err := store.GetEndpointCredentials("Primary")
	if err != nil {
		t.Fatalf("get old credentials: %v", err)
	}
	newCreds, err := store.GetEndpointCredentials("Renamed")
	if err != nil {
		t.Fatalf("get renamed credentials: %v", err)
	}
	if len(oldCreds) != 0 || len(newCreds) != 1 {
		t.Fatalf("expected credentials to move to Renamed, old=%d new=%d", len(oldCreds), len(newCreds))
	}
	oldUsage, err := store.GetCredentialUsageByEndpoint("Primary")
	if err != nil {
		t.Fatalf("get old credential usage: %v", err)
	}
	newUsage, err := store.GetCredentialUsageByEndpoint("Renamed")
	if err != nil {
		t.Fatalf("get renamed credential usage: %v", err)
	}
	if len(oldUsage) != 0 || len(newUsage) != 1 {
		t.Fatalf("expected credential usage to move to Renamed, old=%d new=%d", len(oldUsage), len(newUsage))
	}
	statuses, err := store.GetEndpointRuntimeStatuses()
	if err != nil {
		t.Fatalf("get runtime statuses: %v", err)
	}
	if statuses["Primary"] != nil || statuses["Renamed"] == nil {
		t.Fatalf("expected runtime status to move to Renamed, statuses=%#v", statuses)
	}
	oldStats, err := store.GetDailyStats("Primary", "2026-05-12", "2026-05-12")
	if err != nil {
		t.Fatalf("get old daily stats: %v", err)
	}
	newStats, err := store.GetDailyStats("Renamed", "2026-05-12", "2026-05-12")
	if err != nil {
		t.Fatalf("get renamed daily stats: %v", err)
	}
	if len(oldStats) != 0 || len(newStats) != 1 || newStats[0].Requests != 4 {
		t.Fatalf("expected daily stats to move to Renamed, old=%d new=%#v", len(oldStats), newStats)
	}
}

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

func TestMigrateEndpointRuntimeFailureStatusCode(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ccnexus.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	_, err = db.Exec(`
		CREATE TABLE endpoint_runtime_status (
			endpoint_name TEXT PRIMARY KEY,
			last_success_at DATETIME,
			last_failure_at DATETIME,
			last_failure_reason TEXT,
			last_attempt_at DATETIME,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);
		INSERT INTO endpoint_runtime_status (endpoint_name, last_failure_reason)
		VALUES ('Primary', 'rate_limited');
	`)
	if err != nil {
		t.Fatalf("create old runtime status schema: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close old storage: %v", err)
	}

	store, err := NewSQLiteStorage(dbPath)
	if err != nil {
		t.Fatalf("open migrated storage: %v", err)
	}
	defer store.Close()

	statuses, err := store.GetEndpointRuntimeStatuses()
	if err != nil {
		t.Fatalf("get migrated runtime statuses: %v", err)
	}
	status := statuses["Primary"]
	if status == nil {
		t.Fatalf("expected migrated Primary runtime status")
	}
	if status.LastFailureStatusCode != 0 {
		t.Fatalf("expected migrated status code default 0, got %d", status.LastFailureStatusCode)
	}

	failureAt := time.Date(2026, 5, 8, 10, 11, 12, 0, time.UTC)
	reason := "rate_limited"
	statusCode := 429
	status, err = store.UpsertEndpointRuntimeStatus("Primary", EndpointRuntimeStatusPatch{
		LastFailureAt:         &failureAt,
		LastFailureReason:     &reason,
		LastFailureStatusCode: &statusCode,
	})
	if err != nil {
		t.Fatalf("upsert migrated failure status: %v", err)
	}
	if status.LastFailureStatusCode != statusCode {
		t.Fatalf("expected migrated status code %d, got %d", statusCode, status.LastFailureStatusCode)
	}
}
