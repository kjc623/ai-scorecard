// Package store is content-vault's PostgreSQL access.
//
// Every operation runs inside one transaction scoped to one tenant: the transaction first sets
// app.tenant_id, which the schema's row-level security policies read, so a statement can only see
// and write that tenant's rows. The vault's rules live in package vault; this package is the SQL.
package store

import (
	"context"
	"errors"
	"time"
)

// Errors the vault branches on. Anything else is an infrastructure failure.
var (
	// ErrNotFound means the row asked for does not exist for this tenant, or a conditional claim
	// matched no row.
	ErrNotFound = errors.New("store: not found")
	// ErrContentExists means the event already has stored content.
	ErrContentExists = errors.New("store: content already stored for this event")
)

// Store opens tenant-scoped transactions.
type Store interface {
	// InTenant runs fn in one transaction whose row-level-security tenant is tenantID. The
	// transaction commits when fn returns nil and rolls back otherwise.
	InTenant(ctx context.Context, tenantID string, fn func(Tx) error) error
}

// Tx is one tenant-scoped transaction.
type Tx interface {
	// Tenant reads the tenant row.
	Tenant(ctx context.Context) (Tenant, error)

	// ClaimGrant marks an upload grant used, provided it is for eventID, granted, unused and
	// unexpired at now. It returns ErrNotFound when no grant matched all of that.
	ClaimGrant(ctx context.Context, grantID, eventID string, now time.Time) (Grant, error)
	// Grant reads an upload grant, to explain a claim that matched nothing.
	Grant(ctx context.Context, grantID string) (Grant, error)
	// UploadContext reads what storing content for submissionID needs: the submission's prompt
	// kind and the tenant's content retention. submissionID may be empty.
	UploadContext(ctx context.Context, submissionID string) (UploadContext, error)
	// InsertContent stores one encrypted object. It returns ErrContentExists when the event
	// already has one.
	InsertContent(ctx context.Context, c Content) error
	// MarkUploaded records on the submission that its content is stored, unless it already says so
	// or says the content was shredded.
	MarkUploaded(ctx context.Context, submissionID string) error
	// PutSearchText writes or replaces one search index row.
	PutSearchText(ctx context.Context, u SearchUnit) error

	// ContentForEvent reads an event's stored object without its ciphertext.
	ContentForEvent(ctx context.Context, eventID string) (Content, error)
	// ContentForObject reads one stored object with its ciphertext.
	ContentForObject(ctx context.Context, objectID string) (Content, error)
	// ContentForSubject reads every stored object (with ciphertext) belonging to one subject, by
	// the subject's submissions or events.
	ContentForSubject(ctx context.Context, subjectRef string) ([]Content, error)

	// InsertRetrievalGrant records a single-use retrieval grant.
	InsertRetrievalGrant(ctx context.Context, g RetrievalGrant) error
	// RetrievalGrant reads one retrieval grant.
	RetrievalGrant(ctx context.Context, grantID string) (RetrievalGrant, error)
	// ClaimRetrievalGrant redeems a grant that is unused. It returns ErrNotFound when the grant
	// was already redeemed, so two concurrent redemptions cannot both succeed.
	ClaimRetrievalGrant(ctx context.Context, grantID, usedBy string, now time.Time) (RetrievalGrant, error)

	// Search reads the content search index.
	Search(ctx context.Context, q SearchQuery) ([]SearchHit, error)
	// LatestReceiptID returns the tenant's most recent erasure receipt id, or "" when there is
	// none.
	LatestReceiptID(ctx context.Context) (string, error)
	// AppendAudit writes one audit row.
	AppendAudit(ctx context.Context, e AuditEntry) error
}

// Tenant is the part of ops.tenant the vault reads.
type Tenant struct {
	TenantID      string
	Status        string // active | suspended | offboarding | closed
	ContentSearch SearchTier
	IngestEnabled bool
	ReadEnabled   bool
}

// Grant is a per-event content upload grant (ops.grant).
type Grant struct {
	GrantID      string
	EventID      string
	DeviceID     string
	SubmissionID string // "" when the event's submission was not known at grant time
	Decision     string // pending | granted | denied | expired | voided
	ExpiresAt    time.Time
	UsedAt       time.Time // zero while unused
}

// UploadContext is what the vault reads before storing content.
type UploadContext struct {
	// PromptKind is the submission's request kind: user | client_generated | unknown, or "".
	PromptKind string
	// RetentionDays is the tenant's content retention for this submission.
	RetentionDays int
}

// Content is one stored object (ops.content).
type Content struct {
	TenantID           string
	ObjectID           string
	EventID            string
	SubmissionID       string // "" when unknown
	GrantID            string
	KeyVersion         string
	Ciphertext         []byte // nil when read without it
	PlaintextSizeBytes int
	RawDigest          string
	RetentionClass     string
	PromptKind         string // "" when unknown
	CreatedAt          time.Time
	ExpiresAt          time.Time
}

// RetrievalGrant is a single-use authorisation to read one object (ops.retrieval_grant).
type RetrievalGrant struct {
	GrantID        string
	EventID        string
	ObjectID       string
	SubmissionID   string
	Principal      string
	CaseReference  string
	SecondApprover string
	IssuedAt       time.Time
	ExpiresAt      time.Time
	UsedAt         time.Time // zero while unused
	UsedBy         string
	RawDigest      string
}

// SearchTier is ops.tenant.content_search.
type SearchTier string

// The search tiers, lowest first.
const (
	SearchDisabled        SearchTier = "disabled"
	SearchAttachmentNames SearchTier = "attachment_names"
	SearchFullText        SearchTier = "full_text"
)

// Allows reports whether the tier permits indexing and searching units of kind.
func (t SearchTier) Allows(kind string) bool {
	switch kind {
	case UnitPromptBody:
		return t == SearchFullText
	case UnitAttachmentName:
		return t == SearchFullText || t == SearchAttachmentNames
	}
	return false
}

// Search index unit kinds (ingest.search_text.unit_kind).
const (
	UnitPromptBody     = "prompt_body"
	UnitAttachmentName = "attachment_name"
)

// SearchUnit is one ingest.search_text row.
type SearchUnit struct {
	SubmissionID string
	UnitKind     string
	UnitIndex    int
	Body         string
	ExpiresAt    time.Time
}

// SearchForm is how a search matches.
type SearchForm string

// The search forms.
const (
	FormTerms     SearchForm = "terms"     // full-text terms and phrases over the tsvector
	FormSubstring SearchForm = "substring" // case-insensitive substring of an attachment name
	FormFuzzy     SearchForm = "fuzzy"     // trigram similarity to an attachment name
)

// SearchFilters narrow a search by submission metadata. Zero values add no predicate.
type SearchFilters struct {
	Subject      string // ingest.submission.user_ref
	Tool         string // ingest.submission.tool_fingerprint
	Device       string // ingest.submission.device_id
	Mode         string // ingest.submission.collection_mode
	ReceivedFrom time.Time
	ReceivedTo   time.Time // exclusive
}

// SearchCursor is the keyset position after the last hit of a page.
type SearchCursor struct {
	ReceivedAt   time.Time
	SubmissionID string
	UnitKind     string
	UnitIndex    int
}

// SearchQuery is one read of the index. Every value is a bound parameter.
type SearchQuery struct {
	Form       SearchForm
	Text       string // a tsquery built by the vault for FormTerms; lower-cased text otherwise
	UnitKind   string // "" searches both kinds
	Limit      int
	MinSimilar float64 // FormFuzzy only
	Filters    SearchFilters
	Cursor     *SearchCursor
	Now        time.Time // index rows that expired before Now are not returned
}

// SearchHit is one match: a submission, the unit that matched, and a snippet.
type SearchHit struct {
	SubmissionID string
	UnitKind     string
	UnitIndex    int
	Snippet      string
	Rank         float64
	ReceivedAt   time.Time
}

// AuditEntry is one ops.audit row. The chain hashes are computed by the database.
type AuditEntry struct {
	ActorType     string // user | device | service | system
	ActorID       string
	Action        string
	ObjectType    string
	ObjectID      string
	SubjectRef    string
	CaseReference string
	Detail        map[string]any
	OccurredAt    time.Time
}
