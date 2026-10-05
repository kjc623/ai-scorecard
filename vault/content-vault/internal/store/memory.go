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
	mu       sync.Mutex
	tenants  map[string]Tenant
	objects  map[string]map[string]ContentObject // tenant -> object
	grants   map[string]map[string]RetrievalGrant
	search   map[string][]SearchUnit // tenant -> units
	// submissions is ingest.submission's metadata, keyed tenant -> submission. The SQL store gets
	// this by joining the table; the double needs a copy to apply the same filters.
	submissions map[string]map[string]SearchSubmission
	audit       []AuditEntry
	receipts    map[string]ErasureReceipt
	closed      bool
}

// NewMemory returns an empty store.
func NewMemory() *Memory {
	return &Memory{
		tenants:     map[string]Tenant{},
		objects:     map[string]map[string]ContentObject{},
		grants:      map[string]map[string]RetrievalGrant{},
		search:      map[string][]SearchUnit{},
		submissions: map[string]map[string]SearchSubmission{},
		receipts:    map[string]ErasureReceipt{},
	}
}

// PutSearchSubmission seeds the submission metadata a filtered search joins to. It stands in for
// the row ingest-api writes; the SQL store reads the real one.
func (m *Memory) PutSearchSubmission(s SearchSubmission) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.submissions[s.TenantID] == nil {
		m.submissions[s.TenantID] = map[string]SearchSubmission{}
	}
	m.submissions[s.TenantID][s.SubmissionID] = s
}

// submission returns the metadata for one submission, or the zero value when none was seeded.
func (m *Memory) submissionLocked(tenantID, submissionID string) SearchSubmission {
	return m.submissions[tenantID][submissionID]
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

// SearchAudited implements Store: the audit row is appended before the query runs, in one lock
// scope, which is the in-process equivalent of the single transaction the SQL implementation uses.
func (m *Memory) SearchAudited(ctx context.Context, q SearchQuery, e AuditEntry) ([]SearchHit, error) {
	if err := m.AppendAudit(ctx, e); err != nil {
		return nil, err
	}
	return m.searchText(ctx, q)
}

// searchText is the un-audited query, private so no caller can reach a read path without a record.
func (m *Memory) searchText(_ context.Context, q SearchQuery) ([]SearchHit, error) {
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
		meta := m.submissionLocked(q.TenantID, u.SubmissionID)
		if !filterMatches(q.Filters, meta) {
			continue
		}
		if q.Cursor != nil && !afterSearchCursor(*q.Cursor, meta.ReceivedAt, u) {
			continue
		}
		snippet, rank, ok := matchUnit(q, u)
		if !ok {
			continue
		}
		hits = append(hits, SearchHit{
			SubmissionID: u.SubmissionID, UnitKind: u.UnitKind, UnitIndex: u.UnitIndex,
			Snippet: snippet, Rank: rank, ReceivedAt: meta.ReceivedAt,
		})
	}
	// Newest first, total: the same ordering the SQL statements use, so a keyset page resumes at
	// the same place against either implementation.
	sort.Slice(hits, func(i, j int) bool {
		a, b := hits[i], hits[j]
		if !a.ReceivedAt.Equal(b.ReceivedAt) {
			return a.ReceivedAt.After(b.ReceivedAt)
		}
		if a.SubmissionID != b.SubmissionID {
			return a.SubmissionID > b.SubmissionID
		}
		if a.UnitKind != b.UnitKind {
			return a.UnitKind > b.UnitKind
		}
		return a.UnitIndex > b.UnitIndex
	})
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, nil
}

// filterMatches applies the five optional filters to one submission. A missing metadata row is the
// zero value, so any filter that names a value excludes it, which is the fail-closed reading.
func filterMatches(f SearchFilters, s SearchSubmission) bool {
	if f.Subject != "" && s.UserRef != f.Subject {
		return false
	}
	if f.Tool != "" && s.ToolFingerprint != f.Tool {
		return false
	}
	if f.Device != "" && s.DeviceID != f.Device {
		return false
	}
	if f.Mode != "" && s.CollectionMode != f.Mode {
		return false
	}
	if !f.ReceivedFrom.IsZero() && s.ReceivedAt.Before(f.ReceivedFrom) {
		return false
	}
	if !f.ReceivedTo.IsZero() && !s.ReceivedAt.Before(f.ReceivedTo) {
		return false
	}
	return true
}

// afterSearchCursor reports whether a unit sorts strictly after the cursor in the descending
// ordering (received_at, submission_id, unit_kind, unit_index).
func afterSearchCursor(c SearchCursor, receivedAt time.Time, u SearchUnit) bool {
	if !receivedAt.Equal(c.ReceivedAt) {
		return receivedAt.Before(c.ReceivedAt)
	}
	if u.SubmissionID != c.SubmissionID {
		return u.SubmissionID < c.SubmissionID
	}
	if u.UnitKind != c.UnitKind {
		return u.UnitKind < c.UnitKind
	}
	return u.UnitIndex < c.UnitIndex
}

// matchUnit applies one of the three closed forms. It returns a bounded highlighted snippet.
//
// The terms form receives the *sanitised* tsquery text the service built (`a <-> b & c`), not the
// analyst's raw input, and this double interprets the two operators that text can contain: `&` is
// conjunction and `<->` is adjacency. That is a deliberate approximation of
// `to_tsvector('simple', body) @@ to_tsquery('simple', text)` — whole-word matching, no stemming,
// no stopwords — and sql_integration_test.go checks the approximation against the real database
// rather than against my reading of it.
func matchUnit(q SearchQuery, u SearchUnit) (string, float64, bool) {
	switch q.Form {
	case FormTerms:
		if strings.TrimSpace(q.Text) == "" {
			return "", 0, false
		}
		parts := strings.Split(q.Text, " & ")
		words := make([]string, 0, len(parts))
		for _, part := range parts {
			p := strings.TrimSpace(part)
			if p == "" {
				continue
			}
			if !matchTsqueryPart(u.Body, p) {
				return "", 0, false
			}
			words = append(words, strings.Split(p, " <-> ")...)
		}
		if len(words) == 0 {
			return "", 0, false
		}
		return highlight(u.Body, words, 240), 1, true
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

// matchTsqueryPart matches one conjunction part: a single term, or a `<->` chain requiring the
// words to appear in order and adjacent.
func matchTsqueryPart(body, part string) bool {
	words := strings.Split(part, " <-> ")
	for i := range words {
		words[i] = strings.TrimSpace(words[i])
		if words[i] == "" {
			return false
		}
	}
	if len(words) == 1 {
		return hasWord(body, words[0])
	}
	for _, pos := range wordPositions(body, words[0]) {
		at := pos + len(words[0])
		ok := true
		for _, w := range words[1:] {
			next := nextWord(body, at)
			if next != w {
				ok = false
				break
			}
			at += len(next)
			// Skip the single separator run between adjacent tokens.
			for at < len(body) && !isWordByte(lowerByte(body[at])) {
				break
			}
			at = skipSeparators(body, at)
		}
		if ok {
			return true
		}
	}
	return false
}

func lowerByte(b byte) byte {
	if b >= 'A' && b <= 'Z' {
		return b + 'a' - 'A'
	}
	return b
}

// skipSeparators advances past one run of non-word bytes, which is what `<->` allows between two
// tokens: "wire-transfer" and "wire transfer" both satisfy `wire <-> transfer`.
func skipSeparators(body string, at int) int {
	// A word boundary must be crossed, and only whitespace or punctuation may be crossed:
	// "wire and transfer" therefore does not satisfy the adjacency operator.
	for at < len(body) && !isWordByte(lowerByte(body[at])) {
		if body[at] == ' ' || body[at] == '\t' || body[at] == '-' || body[at] == '_' || body[at] == '.' {
			at++
			continue
		}
		return at // some other punctuation: stop here rather than skipping it
	}
	return at
}

func isWordByte(b byte) bool {
	return b == '_' || b >= '0' && b <= '9' || b >= 'a' && b <= 'z'
}

func hasWord(body, word string) bool { return len(wordPositions(body, word)) > 0 }

// wordPositions returns the byte offsets at which a word appears as a whole token.
func wordPositions(body, word string) []int {
	lower := strings.ToLower(body)
	var out []int
	for i := 0; i+len(word) <= len(lower); i++ {
		if lower[i:i+len(word)] != word {
			continue
		}
		before := i == 0 || !isWordByte(lower[i-1])
		after := i+len(word) == len(lower) || !isWordByte(lower[i+len(word)])
		if before && after {
			out = append(out, i)
		}
	}
	return out
}

// nextWord returns the token beginning at the next word byte after at, or "" at the end.
func nextWord(body string, at int) string {
	lower := strings.ToLower(body)
	i := at
	for i < len(lower) && !isWordByte(lower[i]) {
		i++
	}
	if i >= len(lower) {
		return ""
	}
	j := i
	for j < len(lower) && isWordByte(lower[j]) {
		j++
	}
	return lower[i:j]
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

// LastReceipt implements Store: the most recent receipt for the tenant, by completion time.
func (m *Memory) LastReceipt(_ context.Context, tenantID string) (ErasureReceipt, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var best ErasureReceipt
	found := false
	for _, r := range m.receipts {
		if r.TenantID != tenantID {
			continue
		}
		if !found || r.CompletedAt.After(best.CompletedAt) {
			best, found = r, true
		}
	}
	if !found {
		return ErasureReceipt{}, fmt.Errorf("%w: %s", ErrNoReceipt, tenantID)
	}
	return best, nil
}

// Close implements Store.
func (m *Memory) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	return nil
}
