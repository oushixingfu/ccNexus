package api

import (
	"testing"
	"time"

	"github.com/lich0821/ccNexus/internal/storage"
)

func TestDeriveEndpointAvailabilityUsesRuntimeFailure(t *testing.T) {
	failureAt := time.Now().UTC()
	status := &storage.EndpointRuntimeStatus{
		LastFailureAt:         &failureAt,
		LastFailureReason:     "quota_exhausted",
		LastFailureStatusCode: 403,
	}

	available, availability, reason, statusCode := deriveEndpointAvailability(true, status)
	if available || availability != "unavailable" || reason != "quota_exhausted" || statusCode != 403 {
		t.Fatalf("expected unavailable quota status, got available=%t availability=%q reason=%q statusCode=%d", available, availability, reason, statusCode)
	}
}

func TestDeriveEndpointAvailabilityClearsAfterLaterSuccess(t *testing.T) {
	failureAt := time.Now().Add(-time.Minute).UTC()
	successAt := time.Now().UTC()
	status := &storage.EndpointRuntimeStatus{
		LastSuccessAt:         &successAt,
		LastFailureAt:         &failureAt,
		LastFailureReason:     "upstream_5xx",
		LastFailureStatusCode: 503,
	}

	available, availability, reason, statusCode := deriveEndpointAvailability(true, status)
	if !available || availability != "available" || reason != "" || statusCode != 0 {
		t.Fatalf("expected available after later success, got available=%t availability=%q reason=%q statusCode=%d", available, availability, reason, statusCode)
	}
}

func TestDeriveEndpointAvailabilityDisabled(t *testing.T) {
	available, availability, reason, statusCode := deriveEndpointAvailability(false, nil)
	if available || availability != "disabled" || reason != "" || statusCode != 0 {
		t.Fatalf("expected disabled status, got available=%t availability=%q reason=%q statusCode=%d", available, availability, reason, statusCode)
	}
}
