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
