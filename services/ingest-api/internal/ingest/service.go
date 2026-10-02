// Package ingest is the one validating write path (ADR 0001).
//
// It owns, in this order:
//
//  1. the batch envelope's own shape (count and caps, §5.3);
//  2. per-event validation against the contract schema, with §7's reason codes and §4.5's
//     recomputed tier;
//  3. the response, which reports every event's outcome in request order.
//
// It does NOT own the dedup ladder. Whether an event is new, a merge or a duplicate is decided by
// ingest.record_event() through the store interface, and the response reports what the store
// decided -- including which observation won the fidelity tie-break. §6 is explicit that no service
// reads-then-writes to decide whether an event is new.
//
// Validation is performed entirely in memory before the write transaction opens, so the
// transaction contains only writes (§6).
package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/shadow-ai-capture/device/protocol"
	"shadow-ai-capture.invalid/contracts/generated/go/envelope"

	"github.com/shadow-ai-capture/ingest-api/internal/auth"
	"github.com/shadow-ai-capture/ingest-api/internal/contract"
	"github.com/shadow-ai-capture/ingest-api/internal/dedup"
	"github.com/shadow-ai-capture/ingest-api/internal/store"
)

// Config is the service's tunable surface. The zero value is the restrictive value: identity is
// enforced, the dedup_key diagnostic is off, and the caps come from the protocol package.
type Config struct {
	// SupportedSchemaVersions is the set the server advertises; an envelope naming anything else
	// is rejected `unsupported_schema_version` with the list in `detail.supported` (§5 versioning).
	SupportedSchemaVersions []string
	// VerifyDedupKey turns on a §4.5 diagnostic: the device-supplied dedup_key is recomputed and
	// compared. It is OFF by default and it is not a wire contract, because §7's closed reason
	// set has no code for the case; a mismatch therefore reports as `schema_violation` with the
	// derivation in `expected`. The database stores the device's key verbatim, so turning this on
	// is a deliberate, visible tightening rather than a silent second opinion.
	VerifyDedupKey bool
	// AllowBodyIdentityMismatch disables the check that the body's device_id is the authenticated
	// device's. The zero value enforces it. It exists so a deployment can be explicit if it ever
	// needs to, not because there is a case for it today: §5.3 puts device identity in the
	// credential.
	AllowBodyIdentityMismatch bool
	// ReplayWindow bounds the `duplicate_batch` guard. §5.3 does not state a value; the guard is
	// owned by the transport (`internal/batchguard`), and this field documents the assumption.
	ReplayWindow time.Duration
}

// DefaultConfig returns the serving configuration.
func DefaultConfig() Config {
	return Config{
		SupportedSchemaVersions: []string{"1.0"},
		ReplayWindow:            24 * time.Hour,
	}
}

// Service validates and writes batches.
type Service struct {
	schema *contract.Schema
	store  store.Store
	cfg    Config

	kinds    map[string]bool
	kindList []string
	versions map[string]bool
	routes   store.RouteTable
}

// New builds the service and loads the stored route ranking (§4.4: ranks are data).
func New(schema *contract.Schema, st store.Store, cfg Config) (*Service, error) {
	if cfg.SupportedSchemaVersions == nil {
		cfg.SupportedSchemaVersions = []string{"1.0"}
	}
	s := &Service{
		schema:   schema,
		store:    st,
		cfg:      cfg,
		kinds:    map[string]bool{},
		versions: map[string]bool{},
	}
	// The closed kind registry comes from the generated contract types, not from a list here: a new
	// kind is an ADR that regenerates them, and this loop is what makes that structural (D8/R7).
	for _, k := range envelope.AllKinds() {
		s.kinds[string(k)] = true
		s.kindList = append(s.kindList, string(k))
	}
	sort.Strings(s.kindList)
	for _, v := range cfg.SupportedSchemaVersions {
		s.versions[v] = true
	}
	if err := s.RefreshRoutes(context.Background()); err != nil {
		return nil, err
	}
	return s, nil
}

// RefreshRoutes reloads ref.route_fidelity. It is called at start-up and may be called on a timer;
// a route that disappears from the table must stop being accepted rather than be compiled in.
func (s *Service) RefreshRoutes(ctx context.Context) error {
	routes, err := s.store.RouteFidelity(ctx)
	if err != nil {
		return fmt.Errorf("ingest: load route fidelity: %w", err)
	}
	if len(routes) == 0 {
		return errors.New("ingest: ref.route_fidelity is empty; refusing to serve because no route would be accepted")
	}
	s.routes = routes
	return nil
}

// Routes returns the loaded route ranking.
func (s *Service) Routes() store.RouteTable { return s.routes }

// Error is a batch-level failure, rendered by the transport as §5's common error envelope:
// { "error": { "code", "detail", "server_time" } }.
type Error struct {
	Status  int
	Code    protocol.ReasonCode
	Detail  *protocol.BatchRejectionDetail
	Message string
}

func (e *Error) Error() string {
	if e.Message != "" {
		return string(e.Code) + ": " + e.Message
	}
	return string(e.Code)
}

func batchErr(status int, code protocol.ReasonCode, pointer, expected string) *Error {
	d := &protocol.BatchRejectionDetail{}
	if pointer != "" {
		d.Pointer = pointer
	}
	if expected != "" {
		d.Expected = expected
	}
	return &Error{Status: status, Code: code, Detail: d}
}

// Submit validates a parsed batch and writes it.
//
// A batch that parses always returns a response, even when every event inside it is rejected: the
// per-event outcome is the contract (§5.3). Only the batch's own shape, the credential and write
// failures produce an *Error.
func (s *Service) Submit(ctx context.Context, p auth.Principal, batch protocol.EventBatch, receivedAt time.Time) (*protocol.EventBatchResponse, error) {
	if err := s.checkBatchShape(batch); err != nil {
		return nil, err
	}

	required := s.schema.RequiredFields()

	results := make([]protocol.EventResult, len(batch.Events))
	accepted := make([]store.AcceptedEvent, 0, len(batch.Events))
	rejected := make([]store.Rejection, 0)
	acceptedAt := make([]int, 0, len(batch.Events))
	tiers := make([]dedup.Tier, len(batch.Events))

	for i, raw := range batch.Events {
		v, err := s.validateOne(p, i, raw, required)
		if err != nil {
			return nil, err
		}
		if v.rejection != nil {
			rejected = append(rejected, *v.rejection)
			results[i] = protocol.EventResult{
				EventID: v.eventID,
				Outcome: protocol.OutcomeRejected,
				Reason:  v.rejection.Reason,
				Detail:  v.rejection.Detail,
			}
			continue
		}
		tiers[i] = v.tier
		acceptedAt = append(acceptedAt, i)
		accepted = append(accepted, store.AcceptedEvent{
			Index: i, EventID: v.eventID, Route: v.route, Envelope: json.RawMessage(raw),
		})
	}

	if len(accepted) > 0 || len(rejected) > 0 {
		res, err := s.store.WriteBatch(ctx, store.BatchWrite{
			TenantID:     p.TenantID,
			DeviceID:     p.DeviceID,
			CredentialID: p.CredentialID,
			ReceivedAt:   receivedAt,
			Accepted:     accepted,
			Rejected:     rejected,
		})
		if err != nil {
			return nil, mapStoreError(err)
		}
		for _, out := range res.Outcomes {
			if out.Index < 0 || out.Index >= len(results) {
				return nil, &Error{Status: 500, Code: protocol.ReasonSchemaViolation,
					Message: fmt.Sprintf("store returned an outcome for index %d, outside the batch", out.Index)}
			}
			res := protocol.EventResult{
				EventID:      out.EventID,
				Outcome:      outcomeOf(out.Outcome),
				SubmissionID: out.SubmissionID,
				DedupTier:    tiers[out.Index].WireTier(),
			}
			switch out.Outcome {
			case store.OutcomeDuplicate:
				res.DedupTier = "" // §5.3's duplicate example carries the submission and first receipt
				res.FirstReceivedAt = out.FirstReceivedAt
			case store.OutcomeInserted, store.OutcomeMerged:
				won := out.WonFields
				res.WonFields = &won
			}
			results[out.Index] = res
		}
	}

	resp := &protocol.EventBatchResponse{
		SchemaVersion: batch.SchemaVersion,
		BatchID:       batch.BatchID,
		ReceivedAt:    receivedAt.UTC(),
		ServerTime:    time.Now().UTC(),
		Results:       results,
	}
	for i := range results {
		switch results[i].Outcome {
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

func outcomeOf(o store.Outcome) protocol.Outcome {
	switch o {
	case store.OutcomeDuplicate:
		return protocol.OutcomeDuplicate
	default:
		return protocol.OutcomeAccepted
	}
}

// checkBatchShape validates the batch envelope itself (§5.3). It uses the protocol package's own
// cap constants so there is one definition of "500 events" and "256 KiB".
func (s *Service) checkBatchShape(batch protocol.EventBatch) error {
	if batch.BatchID == "" {
		return batchErr(400, protocol.ReasonSchemaViolation, "/batch_id", "required: idempotency depends on it")
	}
	if batch.SchemaVersion == "" {
		return batchErr(400, protocol.ReasonSchemaViolation, "/schema_version", "required")
	}
	if !s.versions[batch.SchemaVersion] {
		e := batchErr(400, protocol.ReasonUnsupportedSchemaVersion, "/schema_version",
			"one of the schema versions this server advertises")
		e.Detail.Supported = append([]string(nil), s.cfg.SupportedSchemaVersions...)
		return e
	}
	if batch.EventCount != len(batch.Events) {
		return batchErr(400, protocol.ReasonSchemaViolation, "/event_count",
			fmt.Sprintf("must equal the number of events carried (%d)", len(batch.Events)))
	}
	if len(batch.Events) < protocol.MinBatchEvents || len(batch.Events) > protocol.MaxBatchEvents {
		// §5.3 lists the count outside 1-500 under 400; §7 calls the condition `oversize`, and the
		// closed code is what the device acts on.
		return batchErr(400, protocol.ReasonOversize, "/events",
			fmt.Sprintf("between %d and %d events", protocol.MinBatchEvents, protocol.MaxBatchEvents))
	}
	for i, raw := range batch.Events {
		if len(raw) > protocol.MaxEnvelopeBytes {
			return batchErr(413, protocol.ReasonOversize, fmt.Sprintf("/events/%d", i),
				fmt.Sprintf("a single envelope must be at most %d bytes", protocol.MaxEnvelopeBytes))
		}
	}
	// The protocol package's own validation is the authority for the batch shape; if it disagrees
	// with the checks above, the batch is reported as a schema violation rather than guessed at.
	if err := batch.Validate(); err != nil {
		return batchErr(400, protocol.ReasonSchemaViolation, "", err.Error())
	}
	return nil
}

// verdict is the outcome of validating one event.
type verdict struct {
	eventID   string
	route     string
	tier      dedup.Tier
	rejection *store.Rejection
}

// validateOne applies §7's checks in a fixed order and returns either an accepted event's
// metadata or a rejection. The order is deliberate: the codes a device can act on most
// specifically are checked before the general schema walk, so `unknown_kind` does not arrive as
// `schema_violation` and `mode_violation` carries the pointer of the offending field.
func (s *Service) validateOne(p auth.Principal, idx int, raw json.RawMessage, required []string) (verdict, *Error) {
	ptr := func(field string) string {
		if field == "" {
			return fmt.Sprintf("/events/%d", idx)
		}
		return fmt.Sprintf("/events/%d/%s", idx, field)
	}

	env, err := contract.DecodeEnvelope(raw)
	if err != nil {
		// An element that is not a JSON object has no field vocabulary to validate and cannot
		// carry the event_id the per-event result contract is keyed by, so it is a batch-level
		// defect rather than a per-event outcome (§5.3: 400 when the batch envelope itself is
		// unparseable). §8's poison-payload rule is what the device does with it.
		return verdict{}, batchErr(400, protocol.ReasonSchemaViolation, ptr(""),
			"each element of events must be a JSON object")
	}

	eventID, ok := env.EventID()
	if !ok || eventID == "" {
		return verdict{}, batchErr(400, protocol.ReasonSchemaViolation, ptr("event_id"),
			"required: the per-event result is keyed by event_id")
	}
	v := verdict{eventID: eventID}

	// 1. schema_version must be one the server advertises. Checked before the schema walk so an
	//    old client gets `unsupported_schema_version` and the supported list, not a const failure.
	if sv, ok := env.SchemaVersion(); ok && !s.versions[sv] {
		e := batchErr(400, protocol.ReasonUnsupportedSchemaVersion, ptr("schema_version"),
			"one of the schema versions this server advertises")
		e.Detail.Supported = append([]string(nil), s.cfg.SupportedSchemaVersions...)
		rej := s.rejection(idx, eventID, env, protocol.ReasonUnsupportedSchemaVersion, e.Detail, required)
		v.rejection = &rej
		return v, nil
	}

	// 2. kind is a closed registry (D8). This is the mechanism that makes R7 structural: a
	//    collector defect cannot start shipping raw process telemetry, because no kind exists.
	if kind, ok := env.Kind(); ok && !s.kinds[kind] {
		detail := &protocol.BatchRejectionDetail{
			Pointer:  ptr("kind"),
			Expected: "one of " + strings.Join(s.kindList, " | "),
		}
		rej := s.rejection(idx, eventID, env, protocol.ReasonUnknownKind, detail, required)
		v.rejection = &rej
		return v, nil
	}

	// 3. The mode boundary, restated where it can be diagnosed field by field (§7 mode_violation).
	if detail := s.modeViolation(env, ptr); detail != nil {
		rej := s.rejection(idx, eventID, env, protocol.ReasonModeViolation, detail, required)
		v.rejection = &rej
		return v, nil
	}

	// 4. The contract itself: the generated types decode it, and the schema walk re-derives the
	//    same verdict while supplying the JSON Pointer and violated constraint §7 asks for.
	if viol, err := s.schema.ValidateEnvelope(env); err != nil {
		return verdict{}, batchErr(400, protocol.ReasonSchemaViolation, ptr(""), err.Error())
	} else if viol != nil {
		detail := &protocol.BatchRejectionDetail{
			Pointer:  ptr(strings.TrimPrefix(viol.Pointer, "/")),
			Expected: viol.Expected,
		}
		rej := s.rejection(idx, eventID, env, protocol.ReasonSchemaViolation, detail, required)
		v.rejection = &rej
		return v, nil
	}

	// 5. Identity: the body must agree with the authenticated principal, and the principal wins.
	if tenantID, _ := env.TenantID(); tenantID != p.TenantID {
		detail := &protocol.BatchRejectionDetail{
			Pointer:  ptr("tenant_id"),
			Expected: "the authenticated principal's tenant",
		}
		rej := s.rejection(idx, eventID, env, protocol.ReasonTenantMismatch, detail, required)
		v.rejection = &rej
		return v, nil
	}
	if !s.cfg.AllowBodyIdentityMismatch {
		if deviceID, _ := env.DeviceID(); deviceID != p.DeviceID {
			// There is no `device_mismatch` in §7's closed set. The condition is the same class as
			// tenant_mismatch -- the body disagreeing with the credential -- so it is reported as a
			// schema violation against the authenticated value rather than under a code that would
			// misname it. Flagged to the Lead: the closed set has no code for this case.
			detail := &protocol.BatchRejectionDetail{
				Pointer:  ptr("device_id"),
				Expected: "the authenticated principal's device",
			}
			rej := s.rejection(idx, eventID, env, protocol.ReasonSchemaViolation, detail, required)
			v.rejection = &rej
			return v, nil
		}
	}

	// 6. §4.4: the route must exist in ref.route_fidelity, because the store's tie-break needs its
	//    rank and ingest.record_event() raises rather than guess. Validated here, before the
	//    transaction, so it is a per-event rejection instead of an aborted batch.
	source, _ := env.Source()
	if _, known := s.routes[source]; !known {
		var sources []string
		sources = append(sources, s.routes.Sources()...)
		detail := &protocol.BatchRejectionDetail{
			Pointer:  ptr("source"),
			Expected: "a route present in ref.route_fidelity: " + strings.Join(sources, " | "),
		}
		rej := s.rejection(idx, eventID, env, protocol.ReasonSchemaViolation, detail, required)
		v.rejection = &rej
		return v, nil
	}
	v.route = source

	// 7. §4.5: the tier is recomputed here because it is not a wire field, and reported in the
	//    response. It is not used to decide a merge; the store decides that.
	kind, _ := env.Kind()
	digest, hasDigest := env.ContentDigest()
	v.tier = dedup.TierFor(kind, digest, hasDigest, attachmentsOf(env))

	// 8. Optional §4.5 diagnostic (off by default; see Config.VerifyDedupKey).
	if s.cfg.VerifyDedupKey {
		if detail := s.dedupKeyMismatch(env, p, ptr); detail != nil {
			rej := s.rejection(idx, eventID, env, protocol.ReasonSchemaViolation, detail, required)
			v.rejection = &rej
			return v, nil
		}
	}

	return v, nil
}

// modeViolation implements §7's mode_violation row and the contract's if/then branches, with the
// pointer of the offending field. It is checked before the schema walk so the device is told which
// field broke the mode boundary rather than that "the schema failed".
func (s *Service) modeViolation(env *contract.Envelope, ptr func(string) string) *protocol.BatchRejectionDetail {
	kind, _ := env.Kind()
	mode, _ := env.Mode()
	if kind != "prompt" || !isMode(mode) {
		return nil
	}
	switch mode {
	case "m0":
		// §7: a content-derived field on an M0 record is evidence that the device read something
		// it was not permitted to read, which is why it is rejected rather than ignored.
		for _, f := range []string{"content_digest", "labels", "classifier_version", "confidence", "content_excerpt", "attachments"} {
			if env.Has(f) {
				return &protocol.BatchRejectionDetail{
					Pointer:  ptr(f),
					Expected: "absent when collection_mode is m0",
				}
			}
		}
	case "m1", "m2", "m3":
		for _, f := range []string{"content_digest", "labels", "classifier_version", "confidence"} {
			if !env.Has(f) {
				return &protocol.BatchRejectionDetail{
					Pointer:  ptr(f),
					Expected: fmt.Sprintf("required when collection_mode is %s (the classifier's output must be recorded)", mode),
				}
			}
		}
		if mode == "m2" && !env.Has("content_excerpt") {
			return &protocol.BatchRejectionDetail{
				Pointer:  ptr("content_excerpt"),
				Expected: "required when collection_mode is m2",
			}
		}
		if mode == "m3" && env.Has("content_excerpt") {
			// M3's content path is the grant-bound retrieval path, not the wire.
			return &protocol.BatchRejectionDetail{
				Pointer:  ptr("content_excerpt"),
				Expected: "absent when collection_mode is m3: content moves only on an explicit per-event grant",
			}
		}
	}
	return nil
}

func isMode(m string) bool {
	switch m {
	case "m0", "m1", "m2", "m3":
		return true
	}
	return false
}

// dedupKeyMismatch recomputes §4.5's dedup_key from material the envelope carries and compares it
// with what the device sent. It exists so canonicalisation divergence is *visible*; the database
// stores the device's key verbatim, so this is a diagnostic, not a second authority.
func (s *Service) dedupKeyMismatch(env *contract.Envelope, p auth.Principal, ptr func(string) string) *protocol.BatchRejectionDetail {
	kind, _ := env.Kind()
	device, _ := env.DeviceID()
	tool, _ := env.Tool()
	direction, _ := env.Direction()
	source, _ := env.Source()
	occurred, ok := env.OccurredAt()
	if !ok {
		return nil
	}
	digest, hasDigest := env.ContentDigest()
	tier := dedup.TierFor(kind, digest, hasDigest, attachmentsOf(env))

	var material string
	switch tier {
	case dedup.TierTA, dedup.TierTB:
		material = dedup.MaterialForContent(digest)
	case dedup.TierS:
		size, _ := env.SizeBytes()
		names := make([]string, 0, 3)
		names = append(names, env.AttachmentNames()...)
		material = dedup.MaterialForSurrogate(source, size, dedup.NamesDigest(names))
	case dedup.TierR:
		ws, _ := env.WindowStart()
		we, _ := env.WindowEnd()
		material = dedup.MaterialForRollup(ws, we)
	case dedup.TierD:
		basis, _ := env.DetectionBasis()
		material = dedup.MaterialForDetection(basis)
	}

	derived := dedup.DedupKey(p.TenantID, device, tool, direction, kind, dedup.BucketStart(occurred), tier, material)
	sent, _ := env.DedupKey()
	if sent == derived {
		return nil
	}
	return &protocol.BatchRejectionDetail{
		Pointer:  ptr("dedup_key"),
		Expected: "the §4.5 derivation " + derived + " (dedup_key is what stops one submission being counted twice across routes)",
	}
}

// rejection builds the store-side rejection record and the §7 detail. Nothing that reaches here is
// dropped: it is reported to the device and, where the live quarantine table has a code for it,
// written to ingest.rejected.
func (s *Service) rejection(idx int, eventID string, env *contract.Envelope, reason protocol.ReasonCode, detail *protocol.BatchRejectionDetail, required []string) store.Rejection {
	if detail != nil && len(detail.PresenceMap) == 0 {
		presence := env.PresenceOf(required)
		detail.PresenceMap = presence.Present
	}
	_, quarantine := store.QuarantineReason(reason)
	return store.Rejection{
		Index:      idx,
		EventID:    eventID,
		Reason:     reason,
		Detail:     detail,
		Presence:   env.PresenceOf(required),
		Redacted:   env.Redacted(),
		Quarantine: quarantine,
	}
}

// mapStoreError turns a write failure into the §5 common error. A write error is retryable at
// batch level and nothing has committed (§6), so the device never observes a partial batch.
func mapStoreError(err error) *Error {
	switch {
	case errors.Is(err, store.ErrUnknownTenant):
		return batchErr(403, protocol.ReasonUnknownTenant, "", "the authenticated principal's tenant is unknown to this deployment")
	case errors.Is(err, store.ErrTenantSuspended):
		return batchErr(403, protocol.ReasonUnknownTenant, "", "the tenant's ingest gate is shut")
	case errors.Is(err, store.ErrCredentialRevoked), errors.Is(err, store.ErrDeviceRevoked):
		return batchErr(401, protocol.ReasonRevokedDevice, "", "the device credential was revoked while the batch was being written; nothing was written")
	case errors.Is(err, store.ErrCredentialExpired):
		return batchErr(401, protocol.ReasonRevokedDevice, "", "the device credential expired")
	case errors.Is(err, store.ErrCredentialUnknown):
		return batchErr(401, protocol.ReasonRevokedDevice, "", "the device credential is not known to this deployment")
	case errors.Is(err, store.ErrUnknownRoute):
		return batchErr(503, protocol.ReasonSchemaViolation, "", "ref.route_fidelity changed under the request; retry")
	default:
		return &Error{Status: 503, Code: protocol.ReasonSchemaViolation, Message: "write failure: " + err.Error()}
	}
}

func attachmentsOf(env *contract.Envelope) []dedup.Attachment {
	views := env.Attachments()
	out := make([]dedup.Attachment, 0, len(views))
	for _, a := range views {
		out = append(out, dedup.Attachment{
			Name: a.Name, MediaType: a.MediaType, SizeBytes: a.SizeBytes,
			ContentDigest: a.ContentDigest, Readable: a.Readable,
		})
	}
	return out
}

// SortedCodes is a convenience for diagnostics: the closed reason set, sorted.
func SortedCodes() []string {
	out := make([]string, 0, len(protocol.AllReasonCodes))
	for _, c := range protocol.AllReasonCodes {
		out = append(out, string(c))
	}
	sort.Strings(out)
	return out
}
