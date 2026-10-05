// Package health implements POST /v1/health, the device health channel of
// docs/02-ingest-and-transport.md §5.4 and §9.
//
// It is deliberately not an event: health is current state, and one row per device per collector is
// upserted by key (D5, ADR 0011; docs/01-collectors.md §4.3). The service owns the two decisions the
// transport must not make: which collector names are permitted, and that a stale report does not
// overwrite a newer one.
package health

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/shadow-ai-capture/device/protocol"

	"github.com/shadow-ai-capture/control-api/internal/apierr"
	"github.com/shadow-ai-capture/control-api/internal/store"
)

// Store is the persistence seam the health path needs. It is the subset of store.Store this service
// uses, so a test can supply a double.
type Store interface {
	Tenant(ctx context.Context, tenantID string) (store.Tenant, error)
	RecordHealth(ctx context.Context, tenantID, deviceID string, at time.Time, reports []store.CollectorState, dev store.DeviceHealth) error
}

// Config tunes the response's cadence hint.
type Config struct {
	// NextReportAfterS is the cadence the device is told to use (docs/02 §5.4: cadence is
	// server-driven so the fleet can be slowed without shipping device code). Zero defaults to 900 s
	// (the document's 15-minute assumption).
	NextReportAfterS int
}

// Service validates and writes health reports.
type Service struct {
	store Store
	now   func() time.Time
	next  int
}

// New builds the service.
func New(st Store, cfg Config) (*Service, error) {
	if st == nil {
		return nil, errors.New("health: a store is required")
	}
	next := cfg.NextReportAfterS
	if next <= 0 {
		next = 900
	}
	return &Service{store: st, now: time.Now, next: next}, nil
}

// SetClock overrides the clock, for tests.
func (s *Service) SetClock(now func() time.Time) { s.now = now }

// Report validates one health request and writes it. Tenant and device come from the authenticated
// credential, never from the body (docs/02 §5.4).
func (s *Service) Report(ctx context.Context, tenantID, deviceID string, req protocol.HealthRequest) (protocol.HealthResponse, error) {
	if err := req.Validate(); err != nil {
		return protocol.HealthResponse{}, apierr.New(http.StatusBadRequest, apierr.CodeSchemaViolation, err.Error())
	}
	if tenantID == "" || deviceID == "" {
		return protocol.HealthResponse{}, apierr.New(http.StatusUnauthorized, apierr.CodeRevokedDevice,
			"a health report must be authenticated as a device")
	}
	now := s.now().UTC()
	// The tenant's identity setting gates what is stored and is restated to the device (ADR 0021).
	// Reading it here, not trusting the body, is what makes the setting authoritative.
	tenant, err := s.store.Tenant(ctx, tenantID)
	if err != nil {
		if errors.Is(err, store.ErrUnknownTenant) {
			return protocol.HealthResponse{}, apierr.New(http.StatusForbidden, apierr.CodeUnknownTenant,
				"the authenticated tenant is unknown to this deployment")
		}
		return protocol.HealthResponse{}, apierr.Internal(fmt.Errorf("tenant: %w", err))
	}
	dev := store.DeviceHealth{
		AgentVersion:   req.AgentVersion,
		CollectionMode: req.CollectionMode,
		ManagedState:   req.ManagedState,
	}
	if tenant.DeviceIdentity == protocol.DeviceIdentityClear {
		dev.Hostname = req.Hostname
	}
	rows := make([]store.CollectorState, 0, len(req.Collectors))
	for _, c := range req.Collectors {
		detail, err := deviceDetail(req, c)
		if err != nil {
			return protocol.HealthResponse{}, apierr.Internal(err)
		}
		rows = append(rows, store.CollectorState{
			Collector:         c.Collector,
			State:             string(c.State),
			Version:           c.Version,
			Permissions:       marshalPermissions(c.Permissions),
			LastSuccess:       c.LastSuccess,
			SpoolDepth:        int64Ptr(req.Spool.DepthEvents),
			SpoolCapacity:     optionalInt64(req.Spool.CapacityEvents),
			SpoolDroppedTotal: int64(req.Spool.DroppedTotal),
			ErrorCode:         string(c.Detail),
			Detail:            detail,
		})
	}
	if err := s.store.RecordHealth(ctx, tenantID, deviceID, now, rows, dev); err != nil {
		if errors.Is(err, store.ErrUnknownCollector) {
			return protocol.HealthResponse{}, apierr.New(http.StatusBadRequest, apierr.CodeSchemaViolation,
				"a report named a collector this deployment does not know; a coverage path with no name is refused rather than stored")
		}
		return protocol.HealthResponse{}, apierr.Internal(err)
	}
	return protocol.HealthResponse{
		AckedAt:          now,
		ServerTime:       now,
		NextReportAfterS: s.next,
		DeviceIdentity:   tenant.DeviceIdentity,
	}, nil
}

// deviceDetail is the row's `detail` jsonb: the counters (the closed seven, docs/01 §4.3) plus the
// device-level fields that have no dedicated column (docs/02 §9). It is per-row because the schema
// keys collector_state by collector; the device-level values are repeated on each row.
func deviceDetail(req protocol.HealthRequest, c protocol.HealthReport) (json.RawMessage, error) {
	doc := map[string]any{
		"agent_version": req.AgentVersion,
		"reported_at":   req.ReportedAt.UTC().Format(time.RFC3339Nano),
		"counters":      countersMap(c.Counters),
		"spool": map[string]any{
			"bytes":             req.Spool.SpoolBytes,
			"rejected_total":    req.Spool.RejectedTotal,
			"oldest_spooled_at": optionalTime(req.Spool.OldestSpooledAt),
		},
	}
	if req.ClockOffsetMS != nil {
		doc["clock_offset_ms"] = *req.ClockOffsetMS
	}
	if req.PolicyBundleVersion != "" {
		doc["policy_bundle_version"] = req.PolicyBundleVersion
	}
	if req.SignatureOK != nil {
		doc["signature_ok"] = *req.SignatureOK
	}
	if req.KillSwitchState != "" {
		doc["kill_switch_state"] = req.KillSwitchState
	}
	if req.CredentialNotAfter != nil {
		doc["credential_not_after"] = req.CredentialNotAfter.UTC().Format(time.RFC3339Nano)
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("health: marshal detail: %w", err)
	}
	return raw, nil
}

// countersMap renders the closed counter set in a stable, closed shape. A nil map becomes the seven
// zeroes, so a missing counter is never confused with one that is genuinely zero (protocol.
// NewHealthReport's own rule).
func countersMap(in map[protocol.Counter]uint64) map[string]uint64 {
	out := make(map[string]uint64, len(protocol.AllCounters))
	for _, c := range protocol.AllCounters {
		out[string(c)] = in[c]
	}
	return out
}

func marshalPermissions(in map[string]string) json.RawMessage {
	if len(in) == 0 {
		return json.RawMessage(`{}`)
	}
	raw, err := json.Marshal(in)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return raw
}

func int64Ptr(v int64) *int64 { return &v }

func optionalInt64(v int64) *int64 {
	if v == 0 {
		return nil
	}
	return &v
}

func optionalTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC().Format(time.RFC3339Nano)
}
