// Package storetest is an in-memory store.Store for tests. A transaction works on a copy of the
// state and replaces it only on commit, so a test can observe rollback. Only tests import it.
package storetest

import (
	"context"
	"errors"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/shadow-ai-capture/content-vault/internal/store"
)

// Submission is the part of ingest.submission the fake needs.
type Submission struct {
	TenantID     string
	SubmissionID string
	PromptKind   string
	UserRef      string
	Tool         string
	DeviceID     string
	Mode         string
	ReceivedAt   time.Time
	// ContentState is not_captured when empty.
	ContentState string
}

// Fake is the in-memory store.
type Fake struct {
	mu    sync.Mutex
	state state
	// RetentionDays is the content retention UploadContext reports; 30 when zero.
	RetentionDays int
	// FailAudit makes every audit write fail, to test that nothing commits without its audit row.
	FailAudit bool
	// Commits counts committed transactions.
	Commits int
}

type state struct {
	tenants     map[string]store.Tenant
	grants      map[string]store.Grant // tenant/grant
	submissions map[string]Submission  // tenant/submission
	content     map[string]store.Content
	search      map[string]store.SearchUnit // tenant/submission/kind/index
	retrievals  map[string]store.RetrievalGrant
	receipts    map[string]string // tenant -> latest receipt id
	audit       []Audit
}

// Audit is one recorded audit row.
type Audit struct {
	TenantID string
	store.AuditEntry
}

// New returns an empty fake.
func New() *Fake {
	return &Fake{state: state{
		tenants: map[string]store.Tenant{}, grants: map[string]store.Grant{}, submissions: map[string]Submission{},
		content: map[string]store.Content{}, search: map[string]store.SearchUnit{},
		retrievals: map[string]store.RetrievalGrant{}, receipts: map[string]string{},
	}}
}

func (s state) clone() state {
	return state{
		tenants: maps.Clone(s.tenants), grants: maps.Clone(s.grants), submissions: maps.Clone(s.submissions),
		content: maps.Clone(s.content), search: maps.Clone(s.search), retrievals: maps.Clone(s.retrievals),
		receipts: maps.Clone(s.receipts), audit: slices.Clone(s.audit),
	}
}

func key(parts ...string) string { return strings.Join(parts, "/") }

// PutTenant adds or replaces a tenant.
func (f *Fake) PutTenant(t store.Tenant) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state.tenants[t.TenantID] = t
}

// PutGrant adds or replaces an upload grant.
func (f *Fake) PutGrant(tenantID string, g store.Grant) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state.grants[key(tenantID, g.GrantID)] = g
}

// PutSubmission adds a submission.
func (f *Fake) PutSubmission(s Submission) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state.submissions[key(s.TenantID, s.SubmissionID)] = s
}

// Submission returns a submission as stored.
func (f *Fake) Submission(tenantID, submissionID string) Submission {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state.submissions[key(tenantID, submissionID)]
}

// PutReceipt records a tenant's latest erasure receipt.
func (f *Fake) PutReceipt(tenantID, receiptID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state.receipts[tenantID] = receiptID
}

// Grant returns an upload grant as stored.
func (f *Fake) Grant(tenantID, grantID string) store.Grant {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state.grants[key(tenantID, grantID)]
}

// Content returns every stored object of a tenant.
func (f *Fake) Content(tenantID string) []store.Content {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.Content
	for _, c := range f.state.content {
		if c.TenantID == tenantID {
			out = append(out, c)
		}
	}
	return out
}

// EditContent changes a stored object in place, to simulate expiry, deletion or tampering.
func (f *Fake) EditContent(tenantID, objectID string, edit func(*store.Content) (keep bool)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := key(tenantID, objectID)
	c := f.state.content[k]
	if edit(&c) {
		f.state.content[k] = c
	} else {
		delete(f.state.content, k)
	}
}

// SearchRows returns a tenant's index rows, ordered by submission, kind and index.
func (f *Fake) SearchRows(tenantID string) []store.SearchUnit {
	f.mu.Lock()
	defer f.mu.Unlock()
	var keys []string
	for k := range f.state.search {
		if strings.HasPrefix(k, tenantID+"/") {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	out := make([]store.SearchUnit, 0, len(keys))
	for _, k := range keys {
		out = append(out, f.state.search[k])
	}
	return out
}

// PutSearchRow adds an index row directly.
func (f *Fake) PutSearchRow(tenantID string, u store.SearchUnit) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state.search[searchKey(tenantID, u)] = u
}

// Audit returns the committed audit rows.
func (f *Fake) Audit() []Audit {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.state.audit)
}

// InTenant implements store.Store.
func (f *Fake) InTenant(ctx context.Context, tenantID string, fn func(store.Tx) error) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	tx := &tx{f: f, tenant: tenantID, s: f.state.clone()}
	if err := fn(tx); err != nil {
		return err
	}
	f.state = tx.s
	f.Commits++
	return nil
}

type tx struct {
	f      *Fake
	tenant string
	s      state
}

func (t *tx) Tenant(context.Context) (store.Tenant, error) {
	v, ok := t.s.tenants[t.tenant]
	if !ok {
		return store.Tenant{}, store.ErrNotFound
	}
	return v, nil
}

func (t *tx) ClaimGrant(_ context.Context, grantID, eventID string, now time.Time) (store.Grant, error) {
	k := key(t.tenant, grantID)
	g, ok := t.s.grants[k]
	if !ok || g.EventID != eventID || g.Decision != "granted" || !g.UsedAt.IsZero() || !now.Before(g.ExpiresAt) {
		return store.Grant{}, store.ErrNotFound
	}
	g.UsedAt = now
	t.s.grants[k] = g
	return g, nil
}

func (t *tx) Grant(_ context.Context, grantID string) (store.Grant, error) {
	g, ok := t.s.grants[key(t.tenant, grantID)]
	if !ok {
		return store.Grant{}, store.ErrNotFound
	}
	return g, nil
}

func (t *tx) UploadContext(_ context.Context, submissionID string) (store.UploadContext, error) {
	days := t.f.RetentionDays
	if days == 0 {
		days = 30
	}
	return store.UploadContext{PromptKind: t.s.submissions[key(t.tenant, submissionID)].PromptKind, RetentionDays: days}, nil
}

func (t *tx) InsertContent(_ context.Context, c store.Content) error {
	for _, existing := range t.s.content {
		if existing.TenantID == t.tenant && existing.EventID == c.EventID {
			return store.ErrContentExists
		}
	}
	c.TenantID = t.tenant
	t.s.content[key(t.tenant, c.ObjectID)] = c
	return nil
}

func (t *tx) MarkUploaded(_ context.Context, submissionID string) error {
	k := key(t.tenant, submissionID)
	s, ok := t.s.submissions[k]
	if ok && (s.ContentState == "" || s.ContentState == "not_captured" || s.ContentState == "local_only") {
		s.ContentState = "uploaded"
		t.s.submissions[k] = s
	}
	return nil
}

func searchKey(tenantID string, u store.SearchUnit) string {
	return key(tenantID, u.SubmissionID, u.UnitKind, strconv.Itoa(u.UnitIndex))
}

func (t *tx) PutSearchText(_ context.Context, u store.SearchUnit) error {
	if _, ok := t.s.submissions[key(t.tenant, u.SubmissionID)]; !ok {
		return errors.New("storetest: search text for a submission that does not exist")
	}
	t.s.search[searchKey(t.tenant, u)] = u
	return nil
}

func (t *tx) ContentForEvent(_ context.Context, eventID string) (store.Content, error) {
	for _, c := range t.s.content {
		if c.TenantID == t.tenant && c.EventID == eventID {
			c.Ciphertext = nil
			return c, nil
		}
	}
	return store.Content{}, store.ErrNotFound
}

func (t *tx) ContentForObject(_ context.Context, objectID string) (store.Content, error) {
	c, ok := t.s.content[key(t.tenant, objectID)]
	if !ok {
		return store.Content{}, store.ErrNotFound
	}
	return c, nil
}

func (t *tx) InsertRetrievalGrant(_ context.Context, g store.RetrievalGrant) error {
	if g.SecondApprover != "" && g.SecondApprover == g.Principal {
		return errors.New("storetest: retrieval_grant_second_approver_distinct")
	}
	t.s.retrievals[key(t.tenant, g.GrantID)] = g
	return nil
}

func (t *tx) RetrievalGrant(_ context.Context, grantID string) (store.RetrievalGrant, error) {
	g, ok := t.s.retrievals[key(t.tenant, grantID)]
	if !ok {
		return store.RetrievalGrant{}, store.ErrNotFound
	}
	return g, nil
}

func (t *tx) ClaimRetrievalGrant(_ context.Context, grantID, usedBy string, now time.Time) (store.RetrievalGrant, error) {
	k := key(t.tenant, grantID)
	g, ok := t.s.retrievals[k]
	if !ok || !g.UsedAt.IsZero() {
		return store.RetrievalGrant{}, store.ErrNotFound
	}
	g.UsedAt, g.UsedBy = now, usedBy
	t.s.retrievals[k] = g
	return g, nil
}

// Search matches case-insensitively by substring for every form, which is enough to exercise the
// vault's tier, filter, paging and audit rules; the SQL forms are covered by the live-database test.
func (t *tx) Search(_ context.Context, q store.SearchQuery) ([]store.SearchHit, error) {
	needle := strings.ToLower(strings.NewReplacer(" & ", " ", " <-> ", " ", `\%`, "%", `\_`, "_", `\\`, `\`).Replace(q.Text))
	var hits []store.SearchHit
	for k, u := range t.s.search {
		sub := t.s.submissions[key(t.tenant, u.SubmissionID)]
		switch {
		case !strings.HasPrefix(k, t.tenant+"/"),
			q.UnitKind != "" && u.UnitKind != q.UnitKind,
			!q.Now.Before(u.ExpiresAt),
			q.Filters.Subject != "" && sub.UserRef != q.Filters.Subject,
			q.Filters.Tool != "" && sub.Tool != q.Filters.Tool,
			q.Filters.Device != "" && sub.DeviceID != q.Filters.Device,
			q.Filters.Mode != "" && sub.Mode != q.Filters.Mode,
			!q.Filters.ReceivedFrom.IsZero() && sub.ReceivedAt.Before(q.Filters.ReceivedFrom),
			!q.Filters.ReceivedTo.IsZero() && !sub.ReceivedAt.Before(q.Filters.ReceivedTo):
			continue
		}
		matched := true
		for _, word := range strings.Fields(needle) {
			matched = matched && strings.Contains(strings.ToLower(u.Body), word)
		}
		if !matched {
			continue
		}
		hits = append(hits, store.SearchHit{SubmissionID: u.SubmissionID, UnitKind: u.UnitKind, UnitIndex: u.UnitIndex, Snippet: u.Body, Rank: 1, ReceivedAt: sub.ReceivedAt})
	}
	sort.Slice(hits, func(i, j int) bool { return after(hits[i], hits[j]) })
	if c := q.Cursor; c != nil {
		pos := store.SearchHit{ReceivedAt: c.ReceivedAt, SubmissionID: c.SubmissionID, UnitKind: c.UnitKind, UnitIndex: c.UnitIndex}
		hits = slices.DeleteFunc(hits, func(h store.SearchHit) bool { return !after(pos, h) })
	}
	if len(hits) > q.Limit {
		hits = hits[:q.Limit]
	}
	return hits, nil
}

// after reports whether a sorts before b in the newest-first order.
func after(a, b store.SearchHit) bool {
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
}

func (t *tx) LatestReceiptID(context.Context) (string, error) {
	return t.s.receipts[t.tenant], nil
}

func (t *tx) AppendAudit(_ context.Context, e store.AuditEntry) error {
	if t.f.FailAudit {
		return errors.New("storetest: audit unavailable")
	}
	t.s.audit = append(t.s.audit, Audit{TenantID: t.tenant, AuditEntry: e})
	return nil
}
