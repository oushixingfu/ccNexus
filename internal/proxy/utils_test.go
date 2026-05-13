package proxy

import "testing"

func TestRetryReasonDoesNotTreatRequestIDContaining429AsRateLimit(t *testing.T) {
	body := `{"error":{"code":"model_not_found","message":"No available channel (request id: 202605130302429)"}}`

	if got := retryReasonForHTTPStatus(503, body); got != "upstream_5xx" {
		t.Fatalf("expected upstream_5xx, got %q", got)
	}
}
