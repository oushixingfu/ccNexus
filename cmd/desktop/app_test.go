package main

import (
	"net/http"
	"testing"
	"time"

	"github.com/lich0821/ccNexus/internal/config"
	"github.com/lich0821/ccNexus/internal/endpointstate"
	"github.com/lich0821/ccNexus/internal/storage"
)

func TestProjectDesktopEndpointRuntimeStatusMarksInactiveTransientFailureUnknown(t *testing.T) {
	failureAt := time.Now().Add(-time.Minute).UTC()
	status := &storage.EndpointRuntimeStatus{
		EndpointName:          "Primary",
		LastFailureAt:         &failureAt,
		LastFailureReason:     "upstream_5xx",
		LastFailureStatusCode: http.StatusBadGateway,
	}

	projected := projectDesktopEndpointRuntimeStatus("Primary", true, status, "")
	if projected.Available || projected.Availability != endpointstate.Unknown || projected.AvailabilityReason != "" || projected.AvailabilityStatusCode != 0 {
		t.Fatalf("expected inactive transient failure to be unknown, got %#v", projected)
	}
}

func TestProjectDesktopEndpointRuntimeStatusKeepsHardUnavailable(t *testing.T) {
	failureAt := time.Now().Add(-time.Minute).UTC()
	status := &storage.EndpointRuntimeStatus{
		EndpointName:          "Primary",
		LastFailureAt:         &failureAt,
		LastFailureReason:     "quota_exhausted",
		LastFailureStatusCode: http.StatusForbidden,
	}

	projected := projectDesktopEndpointRuntimeStatus("Primary", true, status, "")
	if projected.Available || projected.Availability != endpointstate.Unavailable || projected.AvailabilityReason != "quota_exhausted" || projected.AvailabilityStatusCode != http.StatusForbidden {
		t.Fatalf("expected hard unavailable projection, got %#v", projected)
	}
}

func TestProjectEndpointRuntimeStatusesIncludesUntestedEndpoints(t *testing.T) {
	app := &App{
		config: config.DefaultConfig(),
	}
	app.config.UpdateEndpoints([]config.Endpoint{
		{Name: "Primary", Enabled: true},
	})

	projected := app.projectEndpointRuntimeStatuses(nil)
	status, ok := projected["Primary"]
	if !ok {
		t.Fatalf("expected Primary runtime projection, got %#v", projected)
	}
	if status.Available || status.Availability != endpointstate.Unknown {
		t.Fatalf("expected untested endpoint to be unknown, got %#v", status)
	}
}
