package endpointstate

import (
	"strings"

	"github.com/lich0821/ccNexus/internal/storage"
)

const (
	Available   = "available"
	Unavailable = "unavailable"
	Disabled    = "disabled"
)

type Projection struct {
	Available    bool
	Availability string
	Reason       string
	StatusCode   int
}

func IsHardUnavailableReason(reason string) bool {
	return strings.TrimSpace(reason) == "quota_exhausted"
}

func Derive(enabled bool, status *storage.EndpointRuntimeStatus) Projection {
	if !enabled {
		return Projection{Availability: Disabled}
	}
	if status == nil || strings.TrimSpace(status.LastFailureReason) == "" || status.LastFailureAt == nil {
		return Projection{Available: true, Availability: Available}
	}
	if IsHardUnavailableReason(status.LastFailureReason) {
		return Projection{
			Available:    false,
			Availability: Unavailable,
			Reason:       strings.TrimSpace(status.LastFailureReason),
			StatusCode:   status.LastFailureStatusCode,
		}
	}
	if status.LastSuccessAt != nil && status.LastSuccessAt.After(*status.LastFailureAt) {
		return Projection{Available: true, Availability: Available}
	}
	return Projection{
		Available:    false,
		Availability: Unavailable,
		Reason:       strings.TrimSpace(status.LastFailureReason),
		StatusCode:   status.LastFailureStatusCode,
	}
}
