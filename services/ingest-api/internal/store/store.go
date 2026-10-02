// Package store is the persistence seam of the ingest write path.
//
// The interface is deliberately small: four methods, one of which is the write. Everything that
// decides *merge* semantics lives in PostgreSQL's ingest.record_event() (db/schema.sql), because
// docs/02-ingest-and-transport.md §6 requires idempotency and the dedup tie-break to be enforced by
// the store's unique constraints rather than by a check in application code. The service does not
// re-implement the ladder; it calls it.
//
// Two implementations:
//
//   - Memory: the test double. It mirrors ingest.record_event()'s documented semantics so the
//     service can be tested without a database, and the integration test checks the mirror against
//     the real stored procedure rather than trusting it.
//   - SQL: database/sql against the real schema. Every statement it issues is a constant in
//     sql.go, and the SQL statement *text* is executed against a live PostgreSQL 17 in the
//     integration test. What is not verified without a wire driver is the database/sql plumbing
//     around those statements; see the package report.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/shadow-ai-capture/device/protocol"

	"github.com/shadow-ai-capture/ingest-api/internal/dedup"
)

// RouteFidelity is one row of ref.route_fidelity. The rank is *stored*, never compiled in: §4.4
// says so explicitly, and ingest.record_event() looks it up before it will accept a route at all.
// Rank is lower-is-better, which is the direction the stored procedure compares.
type RouteFidelity struct {
	Source        string `json:"source"`
	Rank          int    `json:"rank"`
	YieldsContent bool   `json:"yields_content"`
}

// RouteTable is ref.route_fidelity, keyed by source.
type RouteTable map[string]RouteFidelity

// Sources returns the known routes, sorted, for a diagnosable rejection.
func (t RouteTable) Sources() []string {
	out := make([]string, 0, len(t))
	for k := range t {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// PrincipalStatus is ops.tenant + ops.device + ops.device_credential as the write path needs it.
// Credential status is checked at admission and again inside the write transaction (§2.3).
type PrincipalStatus struct {
	TenantKnown       bool
	TenantStatus      string
	IngestEnabled     bool
	TenantRegion      string
	DeviceKnown       bool
	DeviceRevokedAt   *time.Time
	CredentialKnown   bool
	CredentialExpiry  time.Time
	CredentialRevoked *time.Time
}

// Errors the write path distinguishes. Everything else is an infrastructure failure and is
// retryable.
var (
	ErrUnknownTenant      = errors.New("store: tenant unknown")
	ErrTenantSuspended    = errors.New("store: tenant ingest disabled")
	ErrCredentialRevoked  = errors.New("store: device credential revoked")
	ErrCredentialExpired  = errors.New("store: device credential expired")
	ErrCredentialUnknown  = errors.New("store: device credential unknown")
	ErrDeviceRevoked      = errors.New("store: device revoked")
	ErrUnknownRoute       = errors.New("store: route not in ref.route_fidelity")
	ErrSubmissionNotFound = errors.New("store: submission row not found after write")
)

// CheckWritable applies the §2.3 rules to a status. It is shared by both implementations so the
// double cannot be more permissive than the real thing.
func (s PrincipalStatus) CheckWritable(now time.Time) error {
	switch {
	case !s.TenantKnown:
		return ErrUnknownTenant
	case !s.IngestEnabled || s.TenantStatus == "closed":
		return ErrTenantSuspended
	case !s.DeviceKnown:
		return ErrCredentialUnknown
	case s.DeviceRevokedAt != nil:
		return ErrDeviceRevoked
	case !s.CredentialKnown:
		return ErrCredentialUnknown
	case s.CredentialRevoked != nil:
		return ErrCredentialRevoked
	case !s.CredentialExpiry.IsZero() && !s.CredentialExpiry.After(now):
		return ErrCredentialExpired
	}
	return nil
}

// Outcome is what ingest.record_event() returned for one event, plus what the store read back.
//
// The three values are the stored procedure's own: 'inserted' (this observation created the
// logical submission), 'merged' (it folded into an existing one) and 'duplicate' (the event_id was
// already accepted). They are not collapsed: a duplicate is counted and reported, never silently
// dropped (§6).
type Outcome string

const (
	OutcomeInserted  Outcome = "inserted"
	OutcomeMerged    Outcome = "merged"
	OutcomeDuplicate Outcome = "duplicate"
)

// AcceptedEvent is one event that passed validation in memory and is now to be written.
type AcceptedEvent struct {
	Index    int             // position in the batch, for response ordering
	EventID  string          // for the response; already validated as a uuid
	Route    string          // from the envelope, already checked against ref.route_fidelity
	Envelope json.RawMessage // exactly the bytes the device sent
}

// Rejection is one event that failed validation. It is written to ingest.rejected in the same
// transaction as the accepted events (§6 step 5), so a validation failure is never a partial
// commit and never a silent drop.
type Rejection struct {
	Index    int
	EventID  string
	Reason   protocol.ReasonCode
	Detail   *protocol.BatchRejectionDetail
	Presence any // contract.FieldPresence
	Redacted map[string]json.RawMessage
	// Quarantine is false when the live ingest.rejected CHECK has no code for this rejection.
	// The rejection is still reported to the device with its full detail; what is missing is the
	// operator-side row, and that gap is reported rather than papered over with a wrong code.
	Quarantine bool
}

// BatchWrite is one transaction's worth of work: the principal to re-check, the events to record,
// the rejections to quarantine.
type BatchWrite struct {
	TenantID     string
	DeviceID     string
	CredentialID string
	ReceivedAt   time.Time
	Accepted     []AcceptedEvent
	Rejected     []Rejection
}

// EventOutcome is the per-event result of the write.
type EventOutcome struct {
	Index           int
	EventID         string
	Outcome         Outcome
	SubmissionID    string
	FirstReceivedAt *time.Time
	// WonFields reports whether the submission's contested fields are this observation's route's.
	// It is read back from the store's own decision (winning_source / winning_fidelity), not
	// recomputed here, so the response cannot hold a second opinion about the tie-break.
	//
	// The reading is *route*-based and that is deliberate: §4.4's requirement is that "the record
	// must carry which route produced it", and winning_source is that fact. It follows that a
	// re-observation from the route that already holds the fields reports true even though no
	// contested field changed, because the fields the submission carries did come from that route.
	// An observation from a route that does not hold them reports false. Distinguishing "this
	// observation changed a field" would need the row's pre-update winner, which the stored
	// procedure does not return; inventing a second read for a display flag would put a
	// ladder-shaped query back into application code, which §6 forbids.
	WonFields bool
}

// BatchResult is the write outcome, aligned with BatchWrite.Accepted.
type BatchResult struct {
	Outcomes []EventOutcome
}

// Store is the persistence seam.
type Store interface {
	// RouteFidelity returns ref.route_fidelity. §4.4: ranks are stored, not compiled in.
	RouteFidelity(ctx context.Context) (RouteTable, error)
	// PrincipalStatus resolves the authenticated principal to tenant, device and credential state.
	PrincipalStatus(ctx context.Context, tenantID, deviceID, credentialID string) (PrincipalStatus, error)
	// WriteBatch performs the whole batch in one transaction: credential re-check, then one
	// ingest.record_event() call per accepted event, then the rejections. Either all of it
	// commits or none of it does.
	WriteBatch(ctx context.Context, w BatchWrite) (BatchResult, error)
	// Close releases resources.
	Close() error
}

// QuarantineReason maps the §7 wire vocabulary onto the live ingest.rejected.reason_code CHECK.
//
// The two vocabularies now use the same spellings for every fact that can appear on both surfaces,
// which is what the schema's own COMMENT on ingest.rejected asks for ("See
// docs/02-ingest-and-transport.md §7 for the reason codes"). The CHECK used to say device_revoked,
// batch_oversize and schema_version_unsupported; those were renames rather than a second
// vocabulary, and db/schema.sql is aligned to the §7 spellings.
//
// What remains a genuine difference is not a rename but a fact that lives on one surface only:
//
//   - storage/batch-only: malformed_json, internal_error and dedup_key_mismatch describe why a
//     record was held at a layer that has no per-event wire outcome, so no wire code maps to them;
//   - wire-only: duplicate_batch is batch-level by construction (§5.3) and tenant_mismatch is a
//     body disagreeing with the authenticated principal, which must not be filed under this
//     tenant's row under a code that means something else (ingest.rejected.tenant_id is NOT NULL
//     and RLS-scoped, so there is no honest tenant to file a cross-tenant body under).
//
// §7 and protocol.ReasonCode are the wire contract and are closed, so the wire keeps its names and
// this function is the only place the translation exists. The second return value is false when
// there is no representable row; TestQuarantineMappingIsTotal asserts that every wire code is
// either mapped or one of those two documented exceptions.
func QuarantineReason(r protocol.ReasonCode) (string, bool) {
	switch r {
	case protocol.ReasonSchemaViolation:
		return "schema_violation", true
	case protocol.ReasonUnsupportedSchemaVersion:
		return "unsupported_schema_version", true
	case protocol.ReasonUnknownKind:
		return "unknown_kind", true
	case protocol.ReasonUnknownTenant:
		return "unknown_tenant", true
	case protocol.ReasonRevokedDevice:
		return "revoked_device", true
	case protocol.ReasonRegionMismatch:
		return "region_mismatch", true
	case protocol.ReasonModeViolation:
		return "mode_violation", true
	case protocol.ReasonOversize:
		return "oversize", true
	case protocol.ReasonTenantMismatch:
		return "", false
	case protocol.ReasonDuplicateBatch:
		return "", false
	}
	return "", false
}

// TTLFromLabels is a deliberately small mirror of ops.event_ttl_days(): the in-memory store needs
// *a* retention value so expires_at is populated, and the real resolution stays in the database.
// It never invents a policy -- it uses the standard class default.
func TTLFromLabels(defaultDays int, _ json.RawMessage) int {
	if defaultDays <= 0 {
		return 90 // ref.retention_class 'standard' default_ttl_days
	}
	return defaultDays
}

func sortStrings(s []string) { sort.Strings(s) }

// TierOf is a small helper so both implementations describe a tier identically. The service
// reports it; no write decision depends on it.
func TierOf(kind, contentDigest string, hasDigest bool, attachments []dedup.Attachment) dedup.Tier {
	return dedup.TierFor(kind, contentDigest, hasDigest, attachments)
}

// LoadRouteTable reads ref.route_fidelity rows from a JSON file, for the in-memory store.
// The rows are data, never compiled in: §4.4 is explicit that the ranking lives in the table.
// services/ingest-api/testdata/route-fidelity.seed.json holds the rows db/schema.sql seeds.
func LoadRouteTable(path string) (RouteTable, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read route table %s: %w", path, err)
	}
	var rows []RouteFidelity
	if err := json.Unmarshal(b, &rows); err != nil {
		return nil, fmt.Errorf("parse route table %s: %w", path, err)
	}
	table := RouteTable{}
	for _, r := range rows {
		if r.Source == "" {
			return nil, fmt.Errorf("route table %s has a row with no source", path)
		}
		table[r.Source] = r
	}
	if len(table) == 0 {
		return nil, fmt.Errorf("route table %s carries no routes", path)
	}
	return table, nil
}
