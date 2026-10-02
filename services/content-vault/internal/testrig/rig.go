// Package testrig builds vault fixtures for the tests in this module: a tenant with a given
// custody mode and search tier, a service wired to an in-memory store and the local key wrapper,
// and a helper that walks the whole content path (prepare → finalise → retrieve → redeem).
//
// It is test-only. Nothing in the product imports it, and it never touches a database or a cloud
// service: the SQL path is exercised by store/sql_integration_test.go against the live container,
// and everything else is exercised here.
package testrig

import (
	"context"
	"crypto/rand"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/protocol"

	"github.com/shadow-ai-capture/content-vault/internal/keys"
	"github.com/shadow-ai-capture/content-vault/internal/store"
	"github.com/shadow-ai-capture/content-vault/internal/vault"
)

// Clock is a controllable clock, so grant expiry and retention expiry are tested by moving time
// rather than by sleeping.
type Clock struct{ T time.Time }

// Now returns the clock's current instant.
func (c *Clock) Now() time.Time { return c.T }

// Advance moves the clock forward.
func (c *Clock) Advance(d time.Duration) { c.T = c.T.Add(d) }

// TenantID is a fixed uuid used by every fixture, so assertions can name it.
const TenantID = "11111111-1111-4111-8111-111111111111"

// OtherTenantID is a second tenant, for cross-tenant assertions.
const OtherTenantID = "22222222-2222-4222-8222-222222222222"

// Tenant builds a tenant with sensible defaults.
func Tenant(opts ...func(*store.Tenant)) store.Tenant {
	t := store.Tenant{
		TenantID:      TenantID,
		Name:          "Test Tenant",
		Status:        "active",
		KeyCustody:    store.CustodyVendor,
		KEKID:         "kek-" + TenantID,
		CeilingMode:   protocol.ModeM3,
		ContentSearch: store.SearchDisabled,
		IngestEnabled: true,
		ReadEnabled:   true,
	}
	for _, f := range opts {
		f(&t)
	}
	return t
}

// Rig is a service plus the pieces a test needs to inspect.
type Rig struct {
	Service *vault.Service
	// Memory is the in-memory store behind the service, exposed so a test can inspect rows, the
	// audit trail and search units directly. (The field is not called Store: Rig.Store is the
	// helper that walks the whole store path.)
	Memory  *store.Memory
	Keys    *keys.LocalKeyWrapper
	Clock   *Clock
	Tenants []store.Tenant
}

// Options adjusts the rig.
type Options struct {
	Tenants     []store.Tenant
	ScopeTiers  map[string]store.SearchTier
	GrantTTL    time.Duration
	FetchBlob   func(string) ([]byte, error)
	AdjustVault func(*vault.Options)
}

// New builds a rig. If no tenants are given, one vendor/m3/disabled tenant is created.
func New(t *testing.T, o Options) *Rig {
	t.Helper()
	mem := store.NewMemory()
	tenants := o.Tenants
	if len(tenants) == 0 {
		tenants = []store.Tenant{Tenant()}
	}
	for _, tn := range tenants {
		mem.PutTenant(tn)
	}
	clock := &Clock{T: time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)}
	kw := keys.NewLocalWithReader(rand.Reader)
	opts := vault.Options{
		Store:      mem,
		Keys:       kw,
		Now:        clock.Now,
		Rnd:        rand.Reader,
		ScopeTiers: o.ScopeTiers,
		FetchBlob:  o.FetchBlob,
	}
	if o.GrantTTL > 0 {
		opts.RetrievalGrantTTL = o.GrantTTL
	}
	if o.AdjustVault != nil {
		o.AdjustVault(&opts)
	}
	svc, err := vault.New(opts)
	if err != nil {
		t.Fatalf("testrig: building the service: %v", err)
	}
	return &Rig{Service: svc, Memory: mem, Keys: kw, Clock: clock, Tenants: tenants}
}

// StoredObject is a fully stored object: the DEK, the wrapped form, and the row.
type StoredObject struct {
	ObjectID     string
	SubmissionID string
	EventID      string
	DEK          []byte
	Wrapped      []byte
	KEKVersion   string
	BlobPath     string
	Digest       string
}

// Store walks the two-phase store path and returns what a caller needs to check it.
func (r *Rig) Store(t *testing.T, tenantID, objectID, submissionID, eventID string, units ...vault.IndexUnit) StoredObject {
	t.Helper()
	ctx := context.Background()
	prep, err := r.Service.PrepareObject(ctx, vault.PrepareRequest{
		TenantID: tenantID, ObjectID: objectID, SubmissionID: submissionID, EventID: eventID,
		RetentionClass: "standard", ExpiresAt: r.Clock.T.Add(90 * 24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("testrig: prepare: %v", err)
	}
	blobPath := "tenants/" + tenantID + "/objects/" + objectID
	digest := "sha256:" + objectID // a stable, well-formed digest for the fixture
	_, err = r.Service.FinaliseObject(ctx, vault.FinaliseRequest{
		TenantID: tenantID, ObjectID: objectID, SubmissionID: submissionID, EventID: eventID,
		BlobPath: blobPath, CiphertextSHA256: digest, PlaintextSizeBytes: 4096,
		WrappedDEK: prep.WrappedDEK, KEKID: prep.KEKID, KEKVersion: prep.KEKVersion,
		RetentionClass: "standard", ExpiresAt: r.Clock.T.Add(90 * 24 * time.Hour),
		IndexUnits: units,
	})
	if err != nil {
		t.Fatalf("testrig: finalise: %v", err)
	}
	return StoredObject{
		ObjectID: objectID, SubmissionID: submissionID, EventID: eventID, DEK: prep.DEK,
		Wrapped: prep.WrappedDEK, KEKVersion: prep.KEKVersion, BlobPath: blobPath, Digest: digest,
	}
}

// Retrieve runs the approved retrieval path and returns the grant id.
func (r *Rig) Retrieve(t *testing.T, principal, eventID string) string {
	t.Helper()
	res, err := r.Service.Retrieve(context.Background(), vault.RetrieveRequest{
		TenantID: TenantID, EventID: eventID, Principal: principal,
		CaseReference: "CASE-42", SecondApprover: "approver@example.com",
	})
	if err != nil {
		t.Fatalf("testrig: retrieve: %v", err)
	}
	return res.GrantID
}

// UUIDs for fixtures. They are literals rather than generated so a failure message names a stable
// value.
const (
	ObjectA     = "aaaaaaaa-0000-4000-8000-000000000001"
	ObjectB     = "bbbbbbbb-0000-4000-8000-000000000002"
	SubmissionA = "aaaaaaaa-1111-4111-8111-00000000000a"
	SubmissionB = "bbbbbbbb-1111-4111-8111-00000000000b"
	EventA      = "aaaaaaaa-2222-4222-8222-0000000000ea"
	EventB      = "bbbbbbbb-2222-4222-8222-0000000000eb"
)
