package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/shadow-ai-capture/ingest-api/internal/contract"
	"github.com/shadow-ai-capture/ingest-api/internal/dedup"
)

// Memory is the in-memory Store used by the service's tests and by a local run.
//
// It is a mirror, not a second specification: every branch below is a transliteration of
// ingest.record_event() in db/schema.sql, and the DB-gated test in this package runs the same
// ladder table against the real stored procedure so a drift between the two shows up as a test
// failure rather than as a surprise in production. The one place it deliberately does NOT mirror
// the deployed function is the Tier S -> Tier T adopt path, which raises
// submission_exact_key_implies_digest on the live server; see memory.adoptDivergence.
type Memory struct {
	mu           sync.Mutex
	ranks        RouteTable
	principals   map[string]PrincipalStatus
	observations map[string]*memoryObservation
	submissions  map[string]*memorySubmission
	quarantine   []memoryRejection
	// replay is the DPoP jti one-shot memory, keyed tenantID|jti. It mirrors ops.dpop_replay: a
	// bounded window, not an audit log, so an expired entry is forgotten and may legitimately recur.
	replay         map[string]time.Time
	defaultTTLDays int
	now            func() time.Time
}

type memoryObservation struct {
	TenantID    string
	EventID     string
	DeviceID    string
	Tool        string
	Kind        string
	Mode        string
	Source      string
	OccurredAt  time.Time
	ReceivedAt  time.Time
	DedupKey    string
	ContentHash string
	SizeBytes   *int64
	ExpiresAt   time.Time
}

type memorySubmission struct {
	TenantID         string
	SubmissionID     string
	DedupKey         string
	DedupWeakKey     string
	Kind             string
	DeviceID         string
	UserRef          string
	Tool             string
	FirstOccurredAt  time.Time
	LastOccurredAt   time.Time
	ReceivedAt       time.Time
	CollectionMode   string
	SizeBytes        *int64
	ContentDigest    string
	Labels           json.RawMessage
	ClassifierVer    string
	Confidence       string
	PolicyAction     string
	PolicyRuleID     string
	DecidedLocally   *bool
	WinningSource    string
	WinningFidelity  int
	ObservedRoutes   []string
	ObservationCount int
	MergeConfidence  string
	ExpiresAt        time.Time
}

type memoryRejection struct {
	TenantID   string
	DeviceID   string
	ReceivedAt time.Time
	ReasonCode string
	Detail     json.RawMessage
	Presence   json.RawMessage
	Redacted   map[string]json.RawMessage
	ExpiresAt  time.Time
}

// NewMemory builds an in-memory store with ref.route_fidelity supplied by the caller (in
// production it is read from the database; the seed values are in db/schema.sql).
func NewMemory(ranks RouteTable) *Memory {
	if ranks == nil {
		ranks = RouteTable{}
	}
	return &Memory{
		ranks:          ranks,
		principals:     map[string]PrincipalStatus{},
		observations:   map[string]*memoryObservation{},
		submissions:    map[string]*memorySubmission{},
		replay:         map[string]time.Time{},
		defaultTTLDays: 90,
		now:            time.Now,
	}
}

// SetNow overrides the clock. Tests use it to make the replay window and expiry deterministic.
func (m *Memory) SetNow(now func() time.Time) { m.now = now }

// SetPrincipal registers an authenticated principal's lifecycle state.
func (m *Memory) SetPrincipal(tenantID, deviceID, credentialID string, st PrincipalStatus) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.principals[principalKey(tenantID, deviceID, credentialID)] = st
}

// Revoke flips a registered principal to revoked, exactly as an operator action would.
func (m *Memory) Revoke(tenantID, deviceID, credentialID string, at time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := principalKey(tenantID, deviceID, credentialID)
	st := m.principals[k]
	st.CredentialRevoked = &at
	m.principals[k] = st
}

func principalKey(tenantID, deviceID, credentialID string) string {
	return tenantID + "|" + deviceID + "|" + credentialID
}

// RouteFidelity implements Store.
func (m *Memory) RouteFidelity(_ context.Context) (RouteTable, error) {
	out := make(RouteTable, len(m.ranks))
	for k, v := range m.ranks {
		out[k] = v
	}
	return out, nil
}

// PrincipalStatus implements Store.
func (m *Memory) PrincipalStatus(_ context.Context, tenantID, deviceID, credentialID string) (PrincipalStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.principalStatusLocked(tenantID, deviceID, credentialID)
}

func (m *Memory) principalStatusLocked(tenantID, deviceID, credentialID string) (PrincipalStatus, error) {
	if st, ok := m.principals[principalKey(tenantID, deviceID, credentialID)]; ok {
		return st, nil
	}
	if st, ok := m.principals[principalKey(tenantID, deviceID, "")]; ok {
		return st, nil
	}
	return PrincipalStatus{}, nil
}

// Close implements Store.
func (m *Memory) Close() error { return nil }

// DPoPReplaySeen implements Store. A jti already recorded inside its window is a replay; an expired
// entry is forgotten and the new presentation is recorded, which mirrors ops.dpop_replay's
// documented bounded forgetfulness. The opportunistic sweep keeps the map from growing without a
// separate job, the same way the SQL implementation deletes expired rows on the way past.
func (m *Memory) DPoPReplaySeen(_ context.Context, tenantID, jti string, expiresAt time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	key := tenantID + "|" + jti
	if exp, ok := m.replay[key]; ok && exp.After(now) {
		return true, nil
	}
	m.replay[key] = expiresAt
	for k, exp := range m.replay {
		if !exp.After(now) {
			delete(m.replay, k)
		}
	}
	return false, nil
}

// WriteBatch implements Store. One call is one transaction: on any error the state is restored
// before returning, so the caller never observes a partial commit (§6).
func (m *Memory) WriteBatch(ctx context.Context, w BatchWrite) (BatchResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	snapshot := m.clone()

	// §2.3: the credential status is re-checked inside the write transaction, before commit.
	st, err := m.principalStatusLocked(w.TenantID, w.DeviceID, w.CredentialID)
	if err != nil {
		m.restore(snapshot)
		return BatchResult{}, err
	}
	if err := st.CheckWritable(m.now()); err != nil {
		m.restore(snapshot)
		return BatchResult{}, err
	}

	outcomes := make([]EventOutcome, 0, len(w.Accepted))
	for _, ev := range w.Accepted {
		if err := ctx.Err(); err != nil {
			m.restore(snapshot)
			return BatchResult{}, err
		}
		out, err := m.recordEventLocked(w, ev)
		if err != nil {
			m.restore(snapshot)
			return BatchResult{}, err
		}
		outcomes = append(outcomes, out)
	}

	for _, r := range w.Rejected {
		if !r.Quarantine {
			continue
		}
		code, ok := QuarantineReason(r.Reason)
		if !ok {
			continue
		}
		detail, _ := json.Marshal(r.Detail)
		presence, _ := json.Marshal(r.Presence)
		m.quarantine = append(m.quarantine, memoryRejection{
			TenantID:   w.TenantID,
			DeviceID:   w.DeviceID,
			ReceivedAt: w.ReceivedAt,
			ReasonCode: code,
			Detail:     detail,
			Presence:   presence,
			Redacted:   r.Redacted,
			ExpiresAt:  w.ReceivedAt.AddDate(0, 0, 14), // ref.retention_class 'quarantine'
		})
	}

	return BatchResult{Outcomes: outcomes}, nil
}

// recordEventLocked transliterates ingest.record_event().
func (m *Memory) recordEventLocked(w BatchWrite, ev AcceptedEvent) (EventOutcome, error) {
	env, err := contract.DecodeEnvelope(ev.Envelope)
	if err != nil {
		return EventOutcome{}, fmt.Errorf("store: memory: decode envelope: %w", err)
	}
	kind, _ := env.Kind()
	mode, _ := env.Mode()
	source, _ := env.Source()
	tool, _ := env.Tool()
	userRef, _ := env.UserRef()
	eventID, _ := env.EventID()
	occurred, _ := env.OccurredAt()
	digest, hasDigest := env.ContentDigest()
	dedupKey, _ := env.DedupKey()
	var size *int64
	if v, ok := env.SizeBytes(); ok {
		size = &v
	}

	fidelity, known := m.ranks[source]
	if !known {
		return EventOutcome{}, fmt.Errorf("%w: %s", ErrUnknownRoute, source)
	}

	isExact := kind == "prompt" && hasDigest
	weak := dedup.WeakDedupKey(w.TenantID, w.DeviceID, tool, kind, occurred, deref(size))
	ttl := TTLFromLabels(m.defaultTTLDays, nil)
	expires := w.ReceivedAt.AddDate(0, 0, ttl)

	obsKey := w.TenantID + "|" + eventID
	if _, exists := m.observations[obsKey]; exists {
		// The event_id was already accepted: report the duplicate and which submission it
		// belongs to, preferring an exact match over a weak one (record_event's own rule).
		var subID string
		for _, s := range m.submissions {
			if s.TenantID != w.TenantID {
				continue
			}
			if (isExact && s.DedupKey == dedupKey) || s.DedupWeakKey == weak {
				if subID == "" || s.DedupKey != "" {
					subID = s.SubmissionID
				}
			}
		}
		first := m.observations[obsKey].ReceivedAt
		return EventOutcome{
			Index: ev.Index, EventID: eventID, Outcome: OutcomeDuplicate,
			SubmissionID: subID, FirstReceivedAt: &first,
		}, nil
	}

	m.observations[obsKey] = &memoryObservation{
		TenantID: w.TenantID, EventID: eventID, DeviceID: w.DeviceID, Tool: tool,
		Kind: kind, Mode: mode, Source: source, OccurredAt: occurred,
		ReceivedAt: w.ReceivedAt, DedupKey: dedupKey, ContentHash: digest,
		SizeBytes: size, ExpiresAt: expires,
	}

	// Submission resolution: exact key first, then a weak-only row for the same device, tool,
	// bucket and size -- which is what lets an exact observation adopt a weak one.
	var target *memorySubmission
	if isExact {
		for _, s := range m.submissions {
			if s.TenantID == w.TenantID && s.DedupKey == dedupKey {
				target = s
				break
			}
		}
	}
	if target == nil {
		for _, s := range m.submissions {
			if s.TenantID == w.TenantID && s.DedupKey == "" && s.DedupWeakKey == weak {
				target = s
				break
			}
		}
	}

	if target != nil {
		// The tie-break is fidelity: the lower stored rank wins (ref.route_fidelity).
		better := fidelity.Rank < target.WinningFidelity
		wasWeakOnly := target.DedupKey == ""
		if occurred.Before(target.FirstOccurredAt) {
			target.FirstOccurredAt = occurred
		}
		if occurred.After(target.LastOccurredAt) {
			target.LastOccurredAt = occurred
		}
		if !containsString(target.ObservedRoutes, source) {
			target.ObservedRoutes = append(target.ObservedRoutes, source)
		}
		target.ObservationCount++

		// Adoption, mirroring ingest.record_event() exactly: when this observation is exact and the
		// row it matched is still weak-only, the row takes the exact key AND the content digest the
		// key was derived from, in the same breath, because CHECK
		// submission_exact_key_implies_digest requires the two to arrive together. Taking the key
		// alone was the defect fixed in db/schema.sql; the integration test asserts this case
		// against the live function.
		if wasWeakOnly && isExact {
			target.DedupKey = dedupKey
			target.ContentDigest = digest
		}
		if better {
			target.WinningSource = source
			target.WinningFidelity = fidelity.Rank
			target.CollectionMode = mode
			target.SizeBytes = size
			target.ContentDigest = digest
			target.Labels = env.RawOrNil("labels")
			target.ClassifierVer = stringOr(env, "classifier_version")
			target.Confidence = stringOr(env, "confidence")
		}
		// "A row that still has no exact key" is a claim about the row AFTER this update, so an
		// adopted row promotes to 'high' rather than keeping the old flag: counting a submission we
		// did merge among the ones we could not is the same quiet error, in the other direction.
		switch {
		case wasWeakOnly && !isExact:
			target.MergeConfidence = "low"
		case wasWeakOnly && isExact:
			target.MergeConfidence = "high"
		}
		if expires.After(target.ExpiresAt) {
			target.ExpiresAt = expires
		}
		return EventOutcome{
			Index: ev.Index, EventID: eventID, Outcome: OutcomeMerged,
			SubmissionID: target.SubmissionID,
			WonFields:    target.WinningSource == source && target.WinningFidelity == fidelity.Rank,
		}, nil
	}

	sub := &memorySubmission{
		TenantID: w.TenantID, SubmissionID: newUUID(), DedupWeakKey: weak,
		Kind: kind, DeviceID: w.DeviceID, UserRef: userRef, Tool: tool,
		FirstOccurredAt: occurred, LastOccurredAt: occurred, ReceivedAt: w.ReceivedAt,
		CollectionMode: mode, SizeBytes: size, ContentDigest: digest,
		Labels: env.RawOrNil("labels"), ClassifierVer: stringOr(env, "classifier_version"),
		Confidence:    stringOr(env, "confidence"),
		WinningSource: source, WinningFidelity: fidelity.Rank,
		ObservedRoutes: []string{source}, ObservationCount: 1,
		MergeConfidence: "high", ExpiresAt: expires,
	}
	if isExact {
		sub.DedupKey = dedupKey
	} else {
		sub.MergeConfidence = "low"
	}
	if pd := env.RawOrNil("policy_decision"); pd != nil {
		var p struct {
			RuleID         string `json:"rule_id"`
			Action         string `json:"action"`
			DecidedLocally bool   `json:"decided_locally"`
		}
		if err := json.Unmarshal(pd, &p); err == nil {
			sub.PolicyAction, sub.PolicyRuleID = p.Action, p.RuleID
			dl := p.DecidedLocally
			sub.DecidedLocally = &dl
		}
	}
	m.submissions[w.TenantID+"|"+sub.SubmissionID] = sub

	return EventOutcome{
		Index: ev.Index, EventID: eventID, Outcome: OutcomeInserted,
		SubmissionID: sub.SubmissionID, WonFields: true,
	}, nil
}

// --- inspection, for tests and for the local run -------------------------------------------

// SubmissionView is a read-only projection of a submission row.
type SubmissionView struct {
	SubmissionID     string
	DedupKey         string
	DedupWeakKey     string
	Kind             string
	WinningSource    string
	WinningFidelity  int
	ObservedRoutes   []string
	ObservationCount int
	MergeConfidence  string
	Tool             string
	CollectionMode   string
	ExpiresAt        time.Time
}

// Submissions returns every stored submission, sorted by tool then submission id.
func (m *Memory) Submissions() []SubmissionView {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]SubmissionView, 0, len(m.submissions))
	for _, s := range m.submissions {
		routes := make([]string, len(s.ObservedRoutes))
		copy(routes, s.ObservedRoutes)
		out = append(out, SubmissionView{
			SubmissionID: s.SubmissionID, DedupKey: s.DedupKey, DedupWeakKey: s.DedupWeakKey,
			Kind: s.Kind, WinningSource: s.WinningSource, WinningFidelity: s.WinningFidelity,
			ObservedRoutes: routes, ObservationCount: s.ObservationCount,
			MergeConfidence: s.MergeConfidence, Tool: s.Tool, CollectionMode: s.CollectionMode,
			ExpiresAt: s.ExpiresAt,
		})
	}
	sortSubmissions(out)
	return out
}

// ObservationCount returns how many observations are stored (I2a: every accepted observation is a
// row; nothing is discarded).
func (m *Memory) ObservationCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.observations)
}

// Quarantined returns the ingested rejections, for tests that assert nothing is silently dropped.
func (m *Memory) Quarantined() []QuarantineView {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]QuarantineView, 0, len(m.quarantine))
	for _, q := range m.quarantine {
		out = append(out, QuarantineView{
			ReasonCode: q.ReasonCode, Detail: q.Detail, Presence: q.Presence,
			Redacted: q.Redacted, ExpiresAt: q.ExpiresAt,
		})
	}
	return out
}

// QuarantineView is a read-only projection of an ingest.rejected row.
type QuarantineView struct {
	ReasonCode string
	Detail     json.RawMessage
	Presence   json.RawMessage
	Redacted   map[string]json.RawMessage
	ExpiresAt  time.Time
}

// --- snapshot/restore, so one WriteBatch is one transaction ---------------------------------

type memorySnapshot struct {
	observations map[string]*memoryObservation
	submissions  map[string]*memorySubmission
	quarantine   []memoryRejection
}

func (m *Memory) clone() memorySnapshot {
	obs := make(map[string]*memoryObservation, len(m.observations))
	for k, v := range m.observations {
		c := *v
		obs[k] = &c
	}
	subs := make(map[string]*memorySubmission, len(m.submissions))
	for k, v := range m.submissions {
		c := *v
		c.ObservedRoutes = append([]string(nil), v.ObservedRoutes...)
		if v.SizeBytes != nil {
			s := *v.SizeBytes
			c.SizeBytes = &s
		}
		subs[k] = &c
	}
	q := append([]memoryRejection(nil), m.quarantine...)
	return memorySnapshot{observations: obs, submissions: subs, quarantine: q}
}

func (m *Memory) restore(s memorySnapshot) {
	m.observations = s.observations
	m.submissions = s.submissions
	m.quarantine = s.quarantine
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func deref(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

func stringOr(env *contract.Envelope, name string) string {
	s, _ := env.String(name)
	return s
}

func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("store: no entropy for a submission id: " + err.Error())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	buf := make([]byte, 36)
	hex.Encode(buf[0:8], b[0:4])
	buf[8] = '-'
	hex.Encode(buf[9:13], b[4:6])
	buf[13] = '-'
	hex.Encode(buf[14:18], b[6:8])
	buf[18] = '-'
	hex.Encode(buf[19:23], b[8:10])
	buf[23] = '-'
	hex.Encode(buf[24:36], b[10:16])
	return string(buf)
}

func sortSubmissions(s []SubmissionView) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0; j-- {
			a, b := s[j-1], s[j]
			if a.Tool < b.Tool || (a.Tool == b.Tool && a.SubmissionID <= b.SubmissionID) {
				break
			}
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
