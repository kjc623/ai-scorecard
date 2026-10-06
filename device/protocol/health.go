package protocol

import (
	"errors"
	"fmt"
	"time"
)

// The device health channel. A device reports periodically and the server keeps one row per
// collector, keyed (tenant, device, collector). It is current state, not an event stream.
//
// HealthReport is one collector's row; HealthRequest is the whole report: the device-level fields
// plus the per-collector array.

// HealthSchemaVersion is the document version of the /v1/health exchange.
const HealthSchemaVersion = "1.0"

// SpoolHealth is the device's spool accounting; the drop counter is what makes an undercount
// visible to the operator. The spool's dropped total and the per-provider dropped counter are
// separate facts and are never summed.
type SpoolHealth struct {
	DepthEvents     int64      `json:"depth_events"`
	CapacityEvents  int64      `json:"capacity_events,omitempty"`
	SpoolBytes      int64      `json:"spool_bytes,omitempty"`
	DroppedTotal    uint64     `json:"dropped_total"`
	RejectedTotal   uint64     `json:"rejected_total,omitempty"`
	OldestSpooledAt *time.Time `json:"oldest_spooled_at,omitempty"`
}

// Validate refuses a negative depth or capacity. A negative counter is a defect, not a state.
func (s SpoolHealth) Validate() error {
	if s.DepthEvents < 0 || s.CapacityEvents < 0 || s.SpoolBytes < 0 {
		return errors.New("protocol: health spool depth, capacity and bytes must not be negative")
	}
	return nil
}

// HealthRequest is the body of POST /v1/health. tenant_id and device_id are absent by
// construction: they come from the authenticated credential, exactly as on /v1/events.
type HealthRequest struct {
	SchemaVersion string    `json:"schema_version"`
	ReportedAt    time.Time `json:"reported_at"`
	AgentVersion  string    `json:"agent_version,omitempty"`
	// Hostname is the clear machine name, present only while the tenant's device identity is
	// clear. When it is hashed the device sends nothing here and the enrolment-time
	// hostname_hash stands.
	Hostname string `json:"hostname,omitempty"`
	// CollectionMode is the effective base mode the device resolved from the signed bundle for its
	// own scope: the device override when the bundle names this device, otherwise the tenant
	// default. It is optional so a device whose bundle has not loaded yet can still report health.
	CollectionMode string `json:"collection_mode,omitempty"`
	// ManagedState is the agent's report: managed when the OS shows an Intune enrolment, else
	// unknown.
	ManagedState        string      `json:"managed_state,omitempty"`
	ClockOffsetMS       *int64      `json:"clock_offset_ms,omitempty"`
	PolicyBundleVersion string      `json:"policy_bundle_version,omitempty"`
	SignatureOK         *bool       `json:"signature_ok,omitempty"`
	KillSwitchState     string      `json:"kill_switch_state,omitempty"`
	CredentialNotAfter  *time.Time  `json:"credential_not_after,omitempty"`
	Spool               SpoolHealth `json:"spool"`
	// Collectors is open within a schema version: a new collector may report without a schema
	// change.
	Collectors []HealthReport `json:"collectors"`
}

// Validate checks the report's own shape. Which collectors exist and whether the device may report
// under them is the server's decision against ref.collector.
func (r HealthRequest) Validate() error {
	switch {
	case r.SchemaVersion != HealthSchemaVersion:
		return fmt.Errorf("protocol: health schema_version %q is not %q", r.SchemaVersion, HealthSchemaVersion)
	case r.ReportedAt.IsZero():
		return errors.New("protocol: health report carries no reported_at")
	case len(r.Collectors) == 0:
		return errors.New("protocol: health report names no collectors")
	}
	// A mode or managed state outside the closed set is a defect in the device, and the server
	// refuses the report rather than guessing: a mis-set mode column would describe a collection
	// posture the device may not be in.
	if r.CollectionMode != "" && !CollectionMode(r.CollectionMode).Valid() {
		return fmt.Errorf("protocol: health collection_mode %q is outside the closed set", r.CollectionMode)
	}
	if r.ManagedState != "" && !ManagedState(r.ManagedState).Valid() {
		return fmt.Errorf("protocol: health managed_state %q is outside the closed set", r.ManagedState)
	}
	if err := r.Spool.Validate(); err != nil {
		return err
	}
	for _, c := range r.Collectors {
		if err := c.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// HealthResponse is the 200 body. next_report_after_s carries the cadence so the fleet can be
// slowed without shipping device code.
type HealthResponse struct {
	AckedAt          time.Time `json:"acked_at"`
	ServerTime       time.Time `json:"server_time"`
	NextReportAfterS int       `json:"next_report_after_s"`
	// DeviceIdentity restates the tenant's identity setting so a device sees a change without
	// waiting for its next enrolment. Empty means the server did not state one.
	DeviceIdentity DeviceIdentity `json:"device_identity,omitempty"`
}
