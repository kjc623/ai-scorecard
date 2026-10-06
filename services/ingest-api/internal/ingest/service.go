// Package ingest validates a device's event batch and writes it.
//
// Every event is validated in memory before the write transaction opens, so the transaction holds
// only writes and a device defect is one event's rejection rather than a batch that can never
// commit. Whether an accepted event is new, a merge or a duplicate is decided by
// ingest.record_event() in the database; the response reports what it decided.
package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/shadow-ai-capture/contracts/envelope"
	"github.com/shadow-ai-capture/device/protocol"

	"github.com/shadow-ai-capture/ingest-api/internal/auth"
	"github.com/shadow-ai-capture/ingest-api/internal/contract"
	"github.com/shadow-ai-capture/ingest-api/internal/store"
)

// Store is the persistence the service needs.
type Store interface {
	Routes(ctx context.Context) ([]string, error)
	WriteBatch(ctx context.Context, w store.BatchWrite) ([]store.EventOutcome, error)
}

// Service validates and writes batches.
type Service struct {
	validator *contract.Validator
	store     Store

	mu     sync.Mutex
	routes map[string]bool // ref.route_fidelity, loaded on first use
}

// New returns a service.
func New(validator *contract.Validator, st Store) *Service {
	return &Service{validator: validator, store: st}
}

// Error is a batch-level failure, rendered as the common error envelope. Message is written for
// the device, which is an untrusted reader; Cause carries internal detail that is logged and never
// sent.
type Error struct {
	Status  int
	Code    protocol.ReasonCode
	Detail  *protocol.BatchRejectionDetail
	Message string
	Cause   error
}

func (e *Error) Error() string {
	if e.Message != "" {
		return string(e.Code) + ": " + e.Message
	}
	return string(e.Code)
}

// Unwrap exposes the internal cause.
func (e *Error) Unwrap() error { return e.Cause }

func batchErr(status int, code protocol.ReasonCode, pointer, expected string) *Error {
	return &Error{Status: status, Code: code, Detail: &protocol.BatchRejectionDetail{Pointer: pointer, Expected: expected}}
}

// Submit validates a parsed batch and writes it. A batch whose own shape is valid always yields a
// response, even when every event in it is rejected: the per-event result is the contract. Only the
// batch's shape and write failures produce an *Error.
func (s *Service) Submit(ctx context.Context, p auth.Principal, batch protocol.EventBatch, receivedAt time.Time) (*protocol.EventBatchResponse, error) {
	if err := checkBatch(batch); err != nil {
		return nil, err
	}
	routes, err := s.routeTable(ctx)
	if err != nil {
		return nil, &Error{Status: http.StatusServiceUnavailable, Code: protocol.ReasonSchemaViolation,
			Message: "the route table could not be read; nothing was committed, so the batch is retryable", Cause: err}
	}

	results := make([]protocol.EventResult, len(batch.Events))
	tiers := make([]string, len(batch.Events))
	write := store.BatchWrite{TenantID: p.TenantID, DeviceID: p.DeviceID, CredentialID: p.CredentialID, ReceivedAt: receivedAt}
	for i, raw := range batch.Events {
		v, err := s.validate(p, routes, i, raw)
		if err != nil {
			return nil, err
		}
		if v.rejection != nil {
			results[i] = protocol.EventResult{EventID: v.eventID, Outcome: protocol.OutcomeRejected, Reason: v.rejection.Reason, Detail: v.rejection.Detail}
			// ingest.rejected has no tenant_mismatch code: the body names another tenant, and the row
			// would be filed under the authenticated one.
			if v.rejection.Reason != protocol.ReasonTenantMismatch {
				write.Rejected = append(write.Rejected, *v.rejection)
			}
			continue
		}
		tiers[i] = v.tier
		write.Accepted = append(write.Accepted, store.AcceptedEvent{Index: i, EventID: v.eventID, Route: v.route, Envelope: raw})
	}

	if len(write.Accepted) > 0 || len(write.Rejected) > 0 {
		outcomes, err := s.store.WriteBatch(ctx, write)
		if err != nil {
			return nil, storeError(err)
		}
		for _, out := range outcomes {
			r := protocol.EventResult{EventID: out.EventID, Outcome: protocol.OutcomeAccepted, SubmissionID: out.SubmissionID}
			if out.Outcome == store.OutcomeDuplicate {
				r.Outcome = protocol.OutcomeDuplicate
				r.FirstReceivedAt = out.FirstReceivedAt
			} else {
				won := out.WonFields
				r.WonFields = &won
				r.DedupTier = tiers[out.Index]
			}
			results[out.Index] = r
		}
	}

	resp := &protocol.EventBatchResponse{
		SchemaVersion: batch.SchemaVersion,
		BatchID:       batch.BatchID,
		ReceivedAt:    receivedAt.UTC(),
		ServerTime:    time.Now().UTC(),
		Results:       results,
	}
	for _, r := range results {
		switch r.Outcome {
		case protocol.OutcomeAccepted:
			resp.Counts.Accepted++
		case protocol.OutcomeDuplicate:
			resp.Counts.Duplicate++
		case protocol.OutcomeRejected:
			resp.Counts.Rejected++
		}
	}
	return resp, nil
}

// routeTable returns ref.route_fidelity's sources, read once.
func (s *Service) routeTable(ctx context.Context) (map[string]bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.routes != nil {
		return s.routes, nil
	}
	sources, err := s.store.Routes(ctx)
	if err != nil {
		return nil, err
	}
	if len(sources) == 0 {
		return nil, errors.New("ingest: ref.route_fidelity is empty")
	}
	s.routes = make(map[string]bool, len(sources))
	for _, src := range sources {
		s.routes[src] = true
	}
	return s.routes, nil
}

// checkBatch validates the batch envelope itself.
func checkBatch(batch protocol.EventBatch) error {
	switch {
	case batch.BatchID == "":
		return batchErr(http.StatusBadRequest, protocol.ReasonSchemaViolation, "/batch_id", "required")
	case batch.SchemaVersion != envelope.SchemaVersion:
		e := batchErr(http.StatusBadRequest, protocol.ReasonUnsupportedSchemaVersion, "/schema_version", "a schema version this server supports")
		e.Detail.Supported = []string{envelope.SchemaVersion}
		return e
	case len(batch.Events) < protocol.MinBatchEvents || len(batch.Events) > protocol.MaxBatchEvents:
		return batchErr(http.StatusBadRequest, protocol.ReasonOversize, "/events",
			fmt.Sprintf("between %d and %d events", protocol.MinBatchEvents, protocol.MaxBatchEvents))
	case batch.EventCount != len(batch.Events):
		return batchErr(http.StatusBadRequest, protocol.ReasonSchemaViolation, "/event_count",
			fmt.Sprintf("the number of events carried (%d)", len(batch.Events)))
	}
	for i, raw := range batch.Events {
		if len(raw) > protocol.MaxEnvelopeBytes {
			return batchErr(http.StatusRequestEntityTooLarge, protocol.ReasonOversize, fmt.Sprintf("/events/%d", i),
				fmt.Sprintf("an envelope of at most %d bytes", protocol.MaxEnvelopeBytes))
		}
	}
	return nil
}

// verdict is the outcome of validating one event: an accepted event's route and dedup tier, or
// a rejection.
type verdict struct {
	eventID   string
	route     string
	tier      string
	rejection *store.Rejection
}

// validate checks one event. The most specific codes come first, so an old client is told
// unsupported_schema_version with the supported list and an unknown kind is unknown_kind rather
// than a generic schema violation.
func (s *Service) validate(p auth.Principal, routes map[string]bool, idx int, raw json.RawMessage) (verdict, *Error) {
	at := func(pointer string) string { return fmt.Sprintf("/events/%d%s", idx, pointer) }

	env, err := contract.Parse(raw)
	if err != nil {
		// No object means no event_id to key a per-event result by.
		return verdict{}, batchErr(http.StatusBadRequest, protocol.ReasonSchemaViolation, at(""), "each element of events must be a JSON object")
	}
	eventID := env.String("event_id")
	if eventID == "" {
		return verdict{}, batchErr(http.StatusBadRequest, protocol.ReasonSchemaViolation, at("/event_id"), "required: the per-event result is keyed by event_id")
	}
	v := verdict{eventID: eventID}
	reject := func(reason protocol.ReasonCode, detail *protocol.BatchRejectionDetail) (verdict, *Error) {
		detail.PresenceMap = env.Present()
		v.rejection = &store.Rejection{Reason: reason, Detail: detail, Present: env.Present(), Redacted: s.validator.Redacted(env)}
		return v, nil
	}

	if sv := env.String("schema_version"); sv != "" && sv != envelope.SchemaVersion {
		return reject(protocol.ReasonUnsupportedSchemaVersion, &protocol.BatchRejectionDetail{
			Pointer: at("/schema_version"), Expected: "a schema version this server supports", Supported: []string{envelope.SchemaVersion},
		})
	}
	if k := env.String("kind"); k != "" && !envelope.Kind(k).Valid() {
		return reject(protocol.ReasonUnknownKind, &protocol.BatchRejectionDetail{
			Pointer: at("/kind"), Expected: fmt.Sprintf("one of %v", envelope.AllKinds()),
		})
	}
	if viol := s.validator.Validate(env); viol != nil {
		reason := protocol.ReasonSchemaViolation
		if viol.Mode {
			reason = protocol.ReasonModeViolation
		}
		return reject(reason, &protocol.BatchRejectionDetail{Pointer: at(viol.Pointer), Expected: viol.Expected})
	}

	sub, err := envelope.DecodeDeviceSubmission(raw)
	if err != nil {
		// The schema accepted what the generated types cannot decode: a server defect.
		return verdict{}, &Error{Status: http.StatusInternalServerError, Code: protocol.ReasonSchemaViolation,
			Message: "the batch could not be recorded; nothing was committed, so the batch is retryable",
			Cause:   fmt.Errorf("event %d passed the schema but not the generated decoder: %w", idx, err)}
	}
	core := sub.Core()

	// Tenant and device come from the certificate; the body must agree.
	if !strings.EqualFold(core.TenantID, p.TenantID) {
		return reject(protocol.ReasonTenantMismatch, &protocol.BatchRejectionDetail{Pointer: at("/tenant_id"), Expected: "the authenticated device's tenant"})
	}
	if !strings.EqualFold(core.DeviceID, p.DeviceID) {
		return reject(protocol.ReasonSchemaViolation, &protocol.BatchRejectionDetail{Pointer: at("/device_id"), Expected: "the authenticated device"})
	}
	if !routes[string(core.Source)] {
		return reject(protocol.ReasonSchemaViolation, &protocol.BatchRejectionDetail{Pointer: at("/source"), Expected: "a route this deployment ranks"})
	}

	v.route = string(core.Source)
	v.tier = dedupTier(sub)
	return v, nil
}

// dedupTier is the material the dedup key was derived from, reported as dedup_tier: T for prompt
// content (M1 and above carry a content digest), S for a surrogate of size and timing (M0), R for a
// rollup window, D for a detection.
func dedupTier(sub envelope.DeviceSubmission) string {
	switch sub.(type) {
	case *envelope.DevicePromptM0:
		return "S"
	case *envelope.DeviceUsageRollup:
		return "R"
	case *envelope.DeviceModelDetection:
		return "D"
	default:
		return "T"
	}
}

// storeError renders a write failure. Nothing has committed, so every failure is retryable at
// batch level, and no message is derived from the failure itself.
func storeError(err error) *Error {
	switch {
	case errors.Is(err, store.ErrUnknownTenant):
		return &Error{Status: http.StatusForbidden, Code: protocol.ReasonUnknownTenant, Message: "the tenant is unknown to this deployment"}
	case errors.Is(err, store.ErrTenantSuspended):
		return &Error{Status: http.StatusForbidden, Code: protocol.ReasonUnknownTenant, Message: "the tenant's ingest is disabled"}
	case errors.Is(err, store.ErrCredentialRevoked), errors.Is(err, store.ErrDeviceRevoked),
		errors.Is(err, store.ErrCredentialExpired), errors.Is(err, store.ErrCredentialUnknown):
		return &Error{Status: http.StatusUnauthorized, Code: protocol.ReasonRevokedDevice, Message: "the device credential is no longer valid; nothing was written"}
	default:
		return &Error{Status: http.StatusServiceUnavailable, Code: protocol.ReasonSchemaViolation,
			Message: "the write failed; nothing was committed, so the batch is retryable", Cause: err}
	}
}
