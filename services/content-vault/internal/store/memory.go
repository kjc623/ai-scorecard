package store

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Memory is the in-process store: the test double, and the implementation a local development run
// uses when no PostgreSQL is reachable.
//
// **It is deliberately more permissive than the database.** It does not enforce the schema's check
// constraints — a tenant with `full_text` and `customer_held` is stored happily — because the
// service is required to refuse that combination *even when the database does not* (ADR 0014's
// invariant, enforced at the service boundary in case the constraint is ever dropped, bypassed by
// a migration, or served by a replica that predates it). If this double enforced it too, deleting
// the service's own check would keep every test green.
//
// What it does mirror exactly is the *shape* of each operation: a claim that is atomic against
// concurrency, a shred that destroys the wrapped key in the same call, a delete that reports how
// many rows it removed, and a search that returns snippets rather than bodies.
type Memory struct {
	mu        sync.Mutex
	tenants   map[string]Tenant
	objects   map[string]map[string]ContentObject // tenant -> object
	grants    map[string]map[string]RetrievalGrant
	search    map[string][]SearchUnit // tenant -> units
	audit     []AuditEntry
	receipts  map[string]ErasureReceipt
	closed    bool
}

// NewMemory returns an empty store.
func NewMemory() *Memory {
	return &Memory{
		tenants:  map[string]Tenant{},
		objects:  map[string]map[string]ContentObject{},
		grants:   map[string]map[string]RetrievalGrant{},
		search:   map[string][]SearchUnit{},
		receipts: map[string]ErasureReceipt{},
	}
}

// PutTenant seeds a tenant. It exists for tests and local development; the SQL implementation
// reads ops.tenant, which only control-api writes.
func (m *Memory) PutTenant(t Tenant) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tenants[t.TenantID] = t
}

// PutSearchUnitRaw seeds an index row without going through the service. Tests use it to plant the
// cross-tenant canary §7.1 asks for.
func (m *Memory) PutSearchUnitRaw(u SearchUnit) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, existing := range m.search[u.TenantID] {
		if existing.SubmissionID == u.SubmissionID && existing.UnitKind == u.UnitKind && existing.UnitIndex == u.UnitIndex {
			m.search[u.TenantID][i] = u
			return
		}
	}
	m.search[u.TenantID] = append(m.search[u.TenantID], u)
}

// Tenant implements Store.
func (m *Memory) Tenant(_ context.Context, tenantID string) (Tenant, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tenants[tenantID]
	if !ok {
		return Tenant{}, fmt.Errorf("%w: %s", ErrUnknownTenant, tenantID)
	}
	return t, nil
}

// PutContentObject implements Store.
func (m *Memory) PutContentObject(_ context.Context, obj ContentObject) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.objects[obj.TenantID] == nil {
		m.objects[obj.TenantID] = map[string]ContentObject{}
	}
	m.objects[obj.TenantID][obj.ObjectID] = obj
	return nil
}

// ContentObject implements Store.
func (m *Memory) ContentObject(_ context.Context, tenantID, objectID string) (ContentObject, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	obj, ok := m.objects[tenantID][objectID]
	if !ok {
		return ContentObject{}, fmt.Errorf("%w: %s/%s", ErrObjectNotFound, tenantID, objectID)
	}
	return obj, nil
}

// ObjectForEvent implements Store.
func (m *Memory) ObjectForEvent(_ context.Context, tenantID, eventID string) (ContentObject, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, obj := range m.objects[tenantID] {
		if obj.EventID == eventID {
			return obj, nil
		}
	}
	return ContentObject{}, fmt.Errorf("%w: event %s", ErrObjectNotFound, eventID)
}

// ObjectsForTenant implements Store.
func (m *Memory) ObjectsForTenant(_ context.Context, tenantID string) ([]ContentObject, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ContentObject, 0, len(m.objects[tenantID]))
	for _, obj := range m.objects[tenantID] {
		out = append(out, obj)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ObjectID < out[j].ObjectID })
	return out, nil
}

// RewrapObject implements Store, including the version guard.
func (m *Memory) RewrapObject(_ context.Context, tenantID, objectID string, wrapped []byte, kekVersion, expectVersion string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	obj, ok := m.objects[tenantID][objectID]
	if !ok {
		return fmt.Errorf("%w: %s/%s", ErrObjectNotFound, tenantID, objectID)
	}
	if obj.KEKVersion != expectVersion {
		return fmt.Errorf("store: object %s is on key version %s, expected %s", objectID, obj.KEKVersion, expectVersion)
	}
	if obj.State == StateShredded {
		return fmt.Errorf("store: object %s is shredded and is not re-wrapped", objectID)
	}
	obj.WrappedDEK = append([]byte(nil), wrapped...)
	obj.KEKVersion = kekVersion
	m.objects[tenantID][objectID] = obj
	return nil
}

// ShredObject implements Store: the wrapped key is destroyed in the same operation that records
// the state, so a shredded object never keeps a usable key.
func (m *Memory) ShredObject(_ context.Context, tenantID, objectID, reason string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	obj, ok := m.objects[tenantID][objectID]
	if !ok {
		return fmt.Errorf("%w: %s/%s", ErrObjectNotFound, tenantID, objectID)
	}
	obj.State = StateShredded
	obj.ShreddedReason = reason
	obj.ShreddedAt = at
	// Destroyed, not flagged: a single byte of non-key material, exactly as the SQL statement
	// overwrites wrapped_dek.
	obj.WrappedDEK = []byte{0x00}
	m.objects[tenantID][objectID] = obj
	return nil
}

// PutSearchUnit implements Store.
func (m *Memory) PutSearchUnit(ctx context.Context, unit SearchUnit) error {
	m.PutSearchUnitRaw(unit)
	return nil
}

// DeleteSearchText implements Store.
func (m *Memory) DeleteSearchText(_ context.Context, tenantID, submissionID string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	kept := make([]SearchUnit, 0, len(m.search[tenantID]))
	var removed int64
	for _, u := range m.search[tenantID] {
		if submissionID == "" || u.SubmissionID == submissionID {
			removed++
			continue
		}
		kept = append(kept, u)
	}
	m.search[tenantID] = kept
	return removed, nil
}

// SearchText implements Store. It approximates PostgreSQL's `simple` configuration: no stemming,
// no stopwords, whole-term matching, with the same three forms the SQL statements serve.
func (m *Memory) SearchText(_ context.Context, q SearchQuery) ([]SearchHit, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	limit := q.Limit
	if limit <= 0 {
		limit = 20
	}
	var hits []SearchHit
	for _, u := range m.search[q.TenantID] {
		if q.UnitKind != "" && u.UnitKind != q.UnitKind {
			continue
		}
		snippet, rank, ok := matchUnit(q, u)
		if !ok {
			continue
		}
		hits = append(hits, SearchHit{SubmissionID: u.SubmissionID, UnitKind: u.UnitKind, UnitIndex: u.UnitIndex, Snippet: snippet, Rank: rank})
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Rank != hits[j].Rank {
			return hits[i].Rank > hits[j].Rank
		}
		return hits[i].SubmissionID < hits[j].SubmissionID
	})
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, nil
}

// matchUnit applies one of the three closed forms. It returns a bounded highlighted snippet.
func matchUnit(q SearchQuery, u SearchUnit) (string, float64, bool) {
	switch q.Form {
	case FormTerms:
		terms := queryTerms(q.Text)
		if len(terms) == 0 {
			return "", 0, false
		}
		lower := strings.ToLower(u.Body)
		matched := 0
		for _, t := range terms {
			if strings.Contains(lower, t) {
				matched++
			}
		}
		if matched != len(terms) {
			return "", 0, false
		}
		return highlight(u.Body, terms, 240), float64(matched) / float64(len(terms)), true
	case FormSubstring:
		if q.Text == "" || !strings.Contains(strings.ToLower(u.Body), strings.ToLower(q.Text)) {
			return "", 0, false
		}
		return highlight(u.Body, []string{strings.ToLower(q.Text)}, 240), 1, true
	case FormFuzzy:
		sim := similarity(strings.ToLower(u.Body), strings.ToLower(q.Text))
		if sim < q.MinSimilar {
			return "", 0, false
		}
		return u.Body, sim, true
	default:
		return "", 0, false
	}
}

// queryTerms splits a match expression into terms, honouring double-quoted phrases. PostgreSQL's
// to_tsquery syntax is richer; the vault builds the SQL query text from the same terms, so the
// double and the database see the same shape (§15.3's "term or phrase" row).
func queryTerms(q string) []string {
	var out []string
	var cur strings.Builder
	inQuote := false
	flush := func() {
		t := strings.TrimSpace(cur.String())
		cur.Reset()
		if t != "" && !strings.EqualFold(t, "and") && !strings.EqualFold(t, "or") {
			out = append(out, strings.ToLower(t))
		}
	}
	for i := 0; i < len(q); i++ {
		c := q[i]
		switch {
		case c == '"':
			if inQuote {
				flush()
				inQuote = false
			} else {
				flush()
				inQuote = true
			}
		case (c == ' ' || c == '\t') && !inQuote:
			flush()
		default:
			cur.WriteByte(c)
		}
	}
	flush()
	return out
}

// highlight returns a bounded window of the body with the matched terms marked, mirroring
// ts_headline's job: a snippet, never the whole body (§6.3: search returns bounded snippets).
func highlight(body string, terms []string, max int) string {
	if len(body) <= max {
		return markTerms(body, terms)
	}
	lower := strings.ToLower(body)
	at := -1
	for _, t := range terms {
		if i := strings.Index(lower, t); i >= 0 && (at < 0 || i < at) {
			at = i
		}
	}
	if at < 0 {
		at = 0
	}
	start := at - max/3
	if start < 0 {
		start = 0
	}
	end := start + max
	if end > len(body) {
		end = len(body)
		start = end - max
		if start < 0 {
			start = 0
		}
	}
	return markTerms(body[start:end], terms)
}

func markTerms(s string, terms []string) string {
	out := s
	for _, t := range terms {
		if t == "" {
			continue
		}
		lower := strings.ToLower(out)
		var b strings.Builder
		for {
			i := strings.Index(lower, t)
			if i < 0 {
				b.WriteString(out)
				break
			}
			b.WriteString(out[:i])
			b.WriteString("<em>")
			b.WriteString(out[i : i+len(t)])
			b.WriteString("</em>")
			out = out[i+len(t):]
			lower = lower[i+len(t):]
		}
		out = b.String()
	}
	return out
}

// similarity is the trigram Dice coefficient pg_trgm uses: |shared trigrams| / |union|. The double
// mirrors it so the fuzzy form's threshold behaves the same way in tests and in the database.
func similarity(a, b string) float64 {
	ga, gb := trigrams(a), trigrams(b)
	if len(ga) == 0 || len(gb) == 0 {
		return 0
	}
	shared := 0
	for g, n := range ga {
		if m, ok := gb[g]; ok {
			if n < m {
				shared += n
			} else {
				shared += m
			}
		}
	}
	union := 0
	for _, n := range ga {
		union += n
	}
	for _, n := range gb {
		union += n
	}
	return float64(shared) / float64(union)
}

func trigrams(s string) map[string]int {
	out := map[string]int{}
	r := []rune("  " + s + " ")
	for i := 0; i+2 < len(r); i++ {
		out[string(r[i:i+3])]++
	}
	return out
}

// AppendAudit implements Store.
func (m *Memory) AppendAudit(_ context.Context, e AuditEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.audit = append(m.audit, e)
	return nil
}

// Audit returns the rows written so far, for tests that assert audit-before-serve.
func (m *Memory) Audit() []AuditEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]AuditEntry, len(m.audit))
	copy(out, m.audit)
	return out
}

// PutRetrievalGrant implements Store.
func (m *Memory) PutRetrievalGrant(_ context.Context, g RetrievalGrant) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.grants[g.TenantID] == nil {
		m.grants[g.TenantID] = map[string]RetrievalGrant{}
	}
	if _, exists := m.grants[g.TenantID][g.GrantID]; exists {
		return fmt.Errorf("store: retrieval grant %s already exists", g.GrantID)
	}
	m.grants[g.TenantID][g.GrantID] = g
	return nil
}

// RetrievalGrant implements Store.
func (m *Memory) RetrievalGrant(_ context.Context, tenantID, grantID string) (RetrievalGrant, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	g, ok := m.grants[tenantID][grantID]
	if !ok {
		return RetrievalGrant{}, fmt.Errorf("%w: %s", ErrGrantNotFound, grantID)
	}
	return g, nil
}

// ClaimRetrievalGrant implements Store atomically: the check and the write happen under one lock,
// which is what the SQL implementation's conditional UPDATE gives in one statement.
func (m *Memory) ClaimRetrievalGrant(_ context.Context, tenantID, grantID, principal string, now time.Time) (RetrievalGrant, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	g, ok := m.grants[tenantID][grantID]
	if !ok {
		return RetrievalGrant{}, fmt.Errorf("%w: %s", ErrGrantNotFound, grantID)
	}
	if g.Used() {
		return g, fmt.Errorf("%w: %s", ErrGrantAlreadyUsed, grantID)
	}
	g.UsedAt = now
	g.UsedBy = principal
	m.grants[tenantID][grantID] = g
	return g, nil
}

// PutErasureReceipt implements Store.
func (m *Memory) PutErasureReceipt(_ context.Context, r ErasureReceipt) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.receipts[r.ReceiptID] = r
	return nil
}

// ErasureReceipt reads a receipt back, for tests.
func (m *Memory) ErasureReceipt(receiptID string) (ErasureReceipt, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.receipts[receiptID]
	return r, ok
}

// Close implements Store.
func (m *Memory) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	return nil
}
