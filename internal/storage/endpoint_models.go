package storage

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

type endpointModelExec interface {
	Exec(query string, args ...interface{}) (sql.Result, error)
}

func normalizeEndpointModel(model *EndpointModel) {
	if model == nil {
		return
	}
	model.EndpointName = strings.TrimSpace(model.EndpointName)
	model.ModelID = strings.TrimSpace(model.ModelID)
	model.DisplayName = strings.TrimSpace(model.DisplayName)
	if model.Source == "" {
		model.Source = EndpointModelSourceManual
	}
	if model.VerificationStatus == "" {
		model.VerificationStatus = EndpointModelStatusUnknown
	}
	model.UpstreamTransformer = strings.TrimSpace(model.UpstreamTransformer)
	model.FailureKind = strings.TrimSpace(model.FailureKind)
	model.FailureMessage = strings.TrimSpace(model.FailureMessage)
}

func (s *SQLiteStorage) UpsertEndpointModel(model *EndpointModel) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.upsertEndpointModelLocked(model)
}

func (s *SQLiteStorage) upsertEndpointModelLocked(model *EndpointModel) error {
	return upsertEndpointModelWithExec(s.db, model)
}

func upsertEndpointModelWithExec(exec endpointModelExec, model *EndpointModel) error {
	normalizeEndpointModel(model)
	if model == nil {
		return fmt.Errorf("endpoint model is nil")
	}
	if model.EndpointName == "" || model.ModelID == "" {
		return fmt.Errorf("endpoint name and model id are required")
	}

	_, err := exec.Exec(`
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
		SELECT id, endpoint_name, model_id, COALESCE(display_name, ''), source, enabled, verification_status,
			COALESCE(upstream_transformer, ''), COALESCE(failure_kind, ''), COALESCE(failure_message, ''),
			last_verified_at, verification_expires_at, last_attempt_at, next_attempt_at,
			COALESCE(sort_order, 0), created_at, updated_at
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

	rows, err := s.db.Query(`
		SELECT id, endpoint_name, model_id, COALESCE(display_name, ''), source, enabled, verification_status,
			COALESCE(upstream_transformer, ''), COALESCE(failure_kind, ''), COALESCE(failure_message, ''),
			last_verified_at, verification_expires_at, last_attempt_at, next_attempt_at,
			COALESCE(sort_order, 0), created_at, updated_at
		FROM endpoint_models
		WHERE model_id=? AND enabled=TRUE AND verification_status=?
			AND (verification_expires_at IS NULL OR verification_expires_at > ?)
		ORDER BY sort_order ASC, endpoint_name ASC
	`, strings.TrimSpace(modelID), EndpointModelStatusVerified, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEndpointModels(rows)
}

func (s *SQLiteStorage) GetAllVerifiedEndpointModels() ([]EndpointModel, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query(`
		SELECT id, endpoint_name, model_id, COALESCE(display_name, ''), source, enabled, verification_status,
			COALESCE(upstream_transformer, ''), COALESCE(failure_kind, ''), COALESCE(failure_message, ''),
			last_verified_at, verification_expires_at, last_attempt_at, next_attempt_at,
			COALESCE(sort_order, 0), created_at, updated_at
		FROM endpoint_models
		WHERE enabled=TRUE AND verification_status=?
			AND (verification_expires_at IS NULL OR verification_expires_at > ?)
		ORDER BY endpoint_name ASC, sort_order ASC, model_id ASC
	`, EndpointModelStatusVerified, time.Now().UTC())
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
		var lastVerifiedAt sql.NullTime
		var verificationExpiresAt sql.NullTime
		var lastAttemptAt sql.NullTime
		var nextAttemptAt sql.NullTime
		var createdAt sql.NullTime
		var updatedAt sql.NullTime
		if err := rows.Scan(
			&model.ID,
			&model.EndpointName,
			&model.ModelID,
			&model.DisplayName,
			&model.Source,
			&model.Enabled,
			&model.VerificationStatus,
			&model.UpstreamTransformer,
			&model.FailureKind,
			&model.FailureMessage,
			&lastVerifiedAt,
			&verificationExpiresAt,
			&lastAttemptAt,
			&nextAttemptAt,
			&model.SortOrder,
			&createdAt,
			&updatedAt,
		); err != nil {
			return nil, err
		}
		model.LastVerifiedAt = fromNullTime(lastVerifiedAt)
		model.VerificationExpiresAt = fromNullTime(verificationExpiresAt)
		model.LastAttemptAt = fromNullTime(lastAttemptAt)
		model.NextAttemptAt = fromNullTime(nextAttemptAt)
		if createdAt.Valid {
			model.CreatedAt = createdAt.Time
		}
		if updatedAt.Valid {
			model.UpdatedAt = updatedAt.Time
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

func (s *SQLiteStorage) upsertLegacyEndpointModelLocked(endpoint *Endpoint) error {
	if endpoint == nil || strings.TrimSpace(endpoint.Model) == "" {
		return nil
	}

	expires := time.Now().UTC().Add(24 * time.Hour)
	return s.upsertEndpointModelLocked(&EndpointModel{
		EndpointName:          endpoint.Name,
		ModelID:               endpoint.Model,
		Source:                EndpointModelSourceLegacy,
		Enabled:               true,
		VerificationStatus:    EndpointModelStatusVerified,
		UpstreamTransformer:   endpoint.Transformer,
		VerificationExpiresAt: &expires,
	})
}
