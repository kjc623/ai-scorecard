// Package store is the persistence seam of the content vault.
//
// It holds the two things §5.3 keeps in different systems: the wrapped per-object data key (a
// database row here) and the ciphertext (a blob this service never sees). The interface is shaped
// so that every operation the vault performs is **one statement**, which is what makes the
// integration with db/schema.sql reviewable: the statements live in sql.go, and
// sql_integration_test.go executes their exact text against a live PostgreSQL 17 server.
//
// Two implementations:
//
//   - Memory: the test double. It is deliberately *permissive* about the schema's invariants — it
//     will happily store a tenant whose custody and search tier contradict each other, exactly as
//     a database whose check constraint had been dropped would. That is what makes the service's
//     own refusal testable: if the double enforced the rule, the service's copy of it could be
//     deleted and every test would still pass.
//   - SQL: database/sql against the real schema, with the row-level-security session tenant set
//     inside every transaction. No PostgreSQL wire driver is fetchable offline (ADR 0016), so
//     NewSQL takes an already-opened *sql.DB and the binary that embeds this service registers the
//     driver.
package store

import (
	"context"
	"errors"
	"time"

	"github.com/shadow-ai-capture/device/protocol"
)

// Custody is ops.tenant.key_custody (§6.2).
type Custody string

const (
	CustodyVendor          Custody = "vendor"
	CustodyCustomerManaged Custody = "customer_managed"
	CustodyCustomerHeld    Custody = "customer_held"
)

// Valid reports membership of the closed set.
func (c Custody) Valid() bool {
	switch c {
	case CustodyVendor, CustodyCustomerManaged, CustodyCustomerHeld:
		return true
	default:
		return false
	}
}

// SearchTier is ops.tenant.content_search (§6.1, ADR 0014).
type SearchTier string

const (
	SearchDisabled        SearchTier = "disabled"
	SearchAttachmentNames SearchTier = "attachment_names"
	SearchFullText        SearchTier = "full_text"
)

// Valid reports membership of the closed set.
func (t SearchTier) Valid() bool {
	switch t {
	case SearchDisabled, SearchAttachmentNames, SearchFullText:
		return true
	default:
		return false
	}
}

// Rank orders the tiers so "the tenant tier is a ceiling" can be compared rather than string-matched.
func (t SearchTier) Rank() int {
	switch t {
	case SearchAttachmentNames:
		return 1
	case SearchFullText:
		return 2
	default:
		return 0
	}
}

// Tenant is ops.tenant as the vault needs it.
type Tenant struct {
	TenantID      string
	Name          string
	Status        string // active | suspended | offboarding | closed
	KeyCustody    Custody
	KEKID         string
	CeilingMode   protocol.CollectionMode
	ContentSearch SearchTier
	IngestEnabled bool
	ReadEnabled   bool
}

// ContentObject is ops.content_object: the wrapped DEK beside the blob reference, never the
// ciphertext itself (§5.3).
type ContentObject struct {
	TenantID           string
	ObjectID           string
	SubmissionID       string
	EventID            string
	BlobPath           string
	CiphertextSHA256   string
	PlaintextSizeBytes int64
	WrappedDEK         []byte
	KEKID              string
	KEKVersion         string
	RetentionClass     string
	State              string // uploaded | shredded
	ShreddedReason     string
	CreatedAt          time.Time
	ExpiresAt          time.Time
	ShreddedAt         time.Time
}

// Object states, from the schema's CHECK.
const (
	StateUploaded = "uploaded"
	StateShredded = "shredded"
)

// RetrievalGrant is a short-lived, single-use authorisation to read one object's plaintext, bound
// to the principal that asked and the event it belongs to (docs/02 §11 "Serving").
//
// The record is a table this service needs and db/schema.sql does not yet have; sql.go carries the
// DDL and the statements, and the README states plainly that the SQL path for retrieval grants is
// NOT VERIFIED because the table does not exist. The in-memory implementation is the one the tests
// exercise.
type RetrievalGrant struct {
	TenantID       string
	GrantID        string
	EventID        string
	ObjectID       string
	SubmissionID   string
	Principal      string
	CaseReference  string
	SecondApprover string
	IssuedAt       time.Time
	ExpiresAt      time.Time
	UsedAt         time.Time
	UsedBy         string
	RawDigest      string
}

// Used reports whether the grant has been consumed. The window between UsedAt and a claimed write
// is closed by ClaimRetrievalGrant, which is a single conditional UPDATE in SQL.
func (g RetrievalGrant) Used() bool { return !g.UsedAt.IsZero() }

// SearchUnit is one row of ingest.search_text.
type SearchUnit struct {
	TenantID     string
	SubmissionID string
	UnitKind     string // prompt_body | attachment_name
	UnitIndex    int
	Body         string
	ExpiresAt    time.Time
}

// Unit kinds, from the schema's CHECK.
const (
	UnitPromptBody     = "prompt_body"
	UnitAttachmentName = "attachment_name"
)

// ValidUnitKind reports membership of the schema's closed set.
func ValidUnitKind(k string) bool { return k == UnitPromptBody || k == UnitAttachmentName }

// SearchForm is the closed set of match expressions an analyst may type (docs/04 §15.3).
type SearchForm string

const (
	FormTerms     SearchForm = "terms"     // tsv @@ to_tsquery, served by search_text_tsv_gin
	FormSubstring SearchForm = "substring" // body ILIKE, served by search_text_name_trgm
	FormFuzzy     SearchForm = "fuzzy"     // similarity(), served by search_text_name_trgm
)

// Valid reports membership of the closed set.
func (f SearchForm) Valid() bool {
	switch f {
	case FormTerms, FormSubstring, FormFuzzy:
		return true
	default:
		return false
	}
}

// SearchQuery is one read of the index. Every field is bound, never interpolated: the query text
// reaches PostgreSQL as a parameter and the unit kinds as a slice.
type SearchQuery struct {
	TenantID   string
	Form       SearchForm
	Text       string
	UnitKind   string // "" means both kinds the caller is permitted to read
	Limit      int
	MinSimilar float64 // fuzzy only
}

// SearchHit is one bounded result: a submission, the unit it matched, and a highlighted snippet.
// The full body is never a field, so no caller can receive one by accident.
type SearchHit struct {
	SubmissionID string
	UnitKind     string
	UnitIndex    int
	Snippet      string
	Rank         float64
}

// AuditEntry is one ops.audit row. prev_hash and row_hash are filled by ops.audit_chain() in the
// database, so a caller cannot forge a link by supplying one.
type AuditEntry struct {
	TenantID      string
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

// ErasureReceipt is ops.erasure_receipt: the record that makes "we deleted it" checkable (C34).
type ErasureReceipt struct {
	TenantID        string
	ReceiptID       string
	ScopeKind       string // subject | tenant | retention | hold_release
	SubjectRef      string
	RequestedBy     string
	RequestedAt     time.Time
	CompletedAt     time.Time
	Mechanisms      []string
	RemovedCounts   map[string]int
	RemainingCounts map[string]int
}

// Errors the vault branches on. Anything else is infrastructure and retryable.
var (
	ErrUnknownTenant      = errors.New("store: tenant unknown")
	ErrObjectNotFound     = errors.New("store: content object not found")
	ErrGrantNotFound      = errors.New("store: retrieval grant not found")
	ErrGrantAlreadyUsed   = errors.New("store: retrieval grant already used")
	ErrTenantNotPermitted = errors.New("store: tenant is not permitted to store content")
)

// Store is the persistence seam. Every method is one statement or one transaction of statements
// that the vault issues in a fixed order.
type Store interface {
	// Tenant reads ops.tenant (RLS-scoped by the session tenant in SQL).
	Tenant(ctx context.Context, tenantID string) (Tenant, error)

	// PutContentObject inserts or replaces the row that holds a wrapped DEK.
	PutContentObject(ctx context.Context, obj ContentObject) error

	// ContentObject reads one object by id.
	ContentObject(ctx context.Context, tenantID, objectID string) (ContentObject, error)

	// ObjectForEvent reads the object an event's grant produced, if any.
	ObjectForEvent(ctx context.Context, tenantID, eventID string) (ContentObject, error)

	// ObjectsForTenant lists the objects a rotation must re-wrap.
	ObjectsForTenant(ctx context.Context, tenantID string) ([]ContentObject, error)

	// RewrapObject records a DEK sealed under a new KEK version. The single-use guard is
	// kek_version = expectVersion: a concurrent rotation cannot re-wrap the same row twice with
	// two different new versions and have the second silently win.
	RewrapObject(ctx context.Context, tenantID, objectID string, wrapped []byte, kekVersion, expectVersion string, at time.Time) error

	// ShredObject marks an object destroyed **and destroys its wrapped key in the same
	// statement**, so "shredded" cannot be recorded while the key survives.
	ShredObject(ctx context.Context, tenantID, objectID, reason string, at time.Time) error

	// PutSearchUnit writes one ingest.search_text row.
	PutSearchUnit(ctx context.Context, unit SearchUnit) error

	// DeleteSearchText removes index rows for one submission, or for a whole tenant when
	// submissionID is empty. §6.4: erasure must reach the index by row deletion, because key
	// destruction does not touch it.
	DeleteSearchText(ctx context.Context, tenantID, submissionID string) (int64, error)

	// SearchText runs one bounded search query.
	SearchText(ctx context.Context, q SearchQuery) ([]SearchHit, error)

	// AppendAudit writes one audit row. In SQL this is the same transaction that serves the read,
	// which is what "audit before serve, failing closed" means mechanically.
	AppendAudit(ctx context.Context, e AuditEntry) error

	// PutRetrievalGrant records a new single-use grant.
	PutRetrievalGrant(ctx context.Context, g RetrievalGrant) error

	// RetrievalGrant reads one grant.
	RetrievalGrant(ctx context.Context, tenantID, grantID string) (RetrievalGrant, error)

	// ClaimRetrievalGrant consumes a grant atomically. It returns ErrGrantAlreadyUsed when the
	// grant was already consumed, which is the only single-use mechanism that survives two
	// concurrent requests.
	ClaimRetrievalGrant(ctx context.Context, tenantID, grantID, principal string, now time.Time) (RetrievalGrant, error)

	// PutErasureReceipt records what was removed and what deliberately survived.
	PutErasureReceipt(ctx context.Context, r ErasureReceipt) error

	// Close releases resources.
	Close() error
}
