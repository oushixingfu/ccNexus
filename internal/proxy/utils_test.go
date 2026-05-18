package proxy

import "testing"

func TestRetryReasonDoesNotTreatRequestIDContaining429AsRateLimit(t *testing.T) {
	body := `{"error":{"code":"model_not_found","message":"No available channel (request id: 202605130302429)"}}`

	if got := retryReasonForHTTPStatus(503, body); got != "upstream_5xx" {
		t.Fatalf("expected upstream_5xx, got %q", got)
	}
}

func TestRetryReasonTreatsQuotaConsumeFailureAsQuotaExhausted(t *testing.T) {
	body := `{"error":{"code":"pre_consume_token_quota_failed","message":"用户额度不足, 剩余额度: ＄0.000000"}}`

	if got := retryReasonForHTTPStatus(403, body); got != "quota_exhausted" {
		t.Fatalf("expected quota_exhausted, got %q", got)
	}
}

func TestRetryReasonDoesNotTreatTransientDatabaseLockAsQuotaExhausted(t *testing.T) {
	body := `{"error":{"code":"pre_consume_token_quota_failed","message":"database is locked (5) (SQLITE_BUSY)"}}`

	if got := retryReasonForHTTPStatus(403, body); got == "quota_exhausted" {
		t.Fatalf("expected transient database lock not to be quota_exhausted")
	}
}
