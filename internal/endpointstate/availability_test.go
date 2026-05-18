package endpointstate

import (
	"net/http"
	"testing"
	"time"

	"github.com/lich0821/ccNexus/internal/storage"
)

func TestDeriveMarksRuntimeFailureUnavailable(t *testing.T) {
	failureAt := time.Now().UTC()
	status := &storage.EndpointRuntimeStatus{
		LastFailureAt:         &failureAt,
		LastFailureReason:     "quota_exhausted",
		LastFailureStatusCode: http.StatusForbidden,
	}

	projection := Derive(true, status)
	if projection.Available || projection.Availability != Unavailable || projection.Reason != "quota_exhausted" || projection.StatusCode != http.StatusForbidden {
		t.Fatalf("unexpected projection: %#v", projection)
	}
}

func TestDeriveClearsFailureAfterLaterSuccess(t *testing.T) {
	failureAt := time.Now().Add(-time.Minute).UTC()
	successAt := time.Now().UTC()
	status := &storage.EndpointRuntimeStatus{
		LastSuccessAt:         &successAt,
		LastFailureAt:         &failureAt,
		LastFailureReason:     "upstream_5xx",
		LastFailureStatusCode: http.StatusBadGateway,
	}

	projection := Derive(true, status)
	if !projection.Available || projection.Availability != Available || projection.Reason != "" || projection.StatusCode != 0 {
		t.Fatalf("unexpected projection: %#v", projection)
	}
}

func TestDeriveKeepsHardUnavailableFailureAfterLaterSuccess(t *testing.T) {
	failureAt := time.Now().Add(-time.Minute).UTC()
	successAt := time.Now().UTC()
	status := &storage.EndpointRuntimeStatus{
		LastSuccessAt:         &successAt,
		LastFailureAt:         &failureAt,
		LastFailureReason:     "quota_exhausted",
		LastFailureStatusCode: http.StatusForbidden,
	}

	projection := Derive(true, status)
	if projection.Available || projection.Availability != Unavailable || projection.Reason != "quota_exhausted" || projection.StatusCode != http.StatusForbidden {
		t.Fatalf("unexpected projection: %#v", projection)
	}
}

func TestDeriveIgnoresReasonlessFailure(t *testing.T) {
	failureAt := time.Now().UTC()
	status := &storage.EndpointRuntimeStatus{
		LastFailureAt:         &failureAt,
		LastFailureStatusCode: http.StatusBadGateway,
	}

	projection := Derive(true, status)
	if !projection.Available || projection.Availability != Available || projection.Reason != "" || projection.StatusCode != 0 {
		t.Fatalf("unexpected projection: %#v", projection)
	}
}

func TestDeriveDisabledEndpoint(t *testing.T) {
	projection := Derive(false, nil)
	if projection.Available || projection.Availability != Disabled || projection.Reason != "" || projection.StatusCode != 0 {
		t.Fatalf("unexpected projection: %#v", projection)
	}
}
