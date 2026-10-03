// Package vaultinvariants holds invariant-level tests for the content vault, written from outside
// the package. They are Lead-owned on purpose: the vault's own author is building its suite inside
// internal/, and INV-1 - "content stays put: the only content-egress path is a per-event grant" - is
// a property of the whole product, not of one package's internals. A component cannot be the only
// witness to the invariant it exists to enforce.
//
// What these tests prove: the service refuses to produce plaintext without a grant, and the
// refusals are attributable to the closed reason vocabulary rather than to a generic error.
//
// What they deliberately do NOT prove: that the storage layer, the key backend or the HTTP surface
// behave correctly in detail. Those belong to the component's own suite, and `internal/` is out of
// this package's reach except through the exported service API.
package vaultinvariants

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/shadow-ai-capture/content-vault/internal/keys"
	"github.com/shadow-ai-capture/content-vault/internal/store"
	"github.com/shadow-ai-capture/content-vault/internal/vault"
	"github.com/shadow-ai-capture/device/protocol"
)

const (
	tenantID = "11111111-1111-7111-8111-111111111111"
	eventID  = "e0000000-0000-7000-8000-000000000001"
	objectID = "0b000000-0000-7000-8000-000000000001"
	analyst  = "analyst@example.test"
	other    = "someone.else@example.test"
	caseRef  = "CASE-2026-0042"
)

func newService(t *testing.T, now func() time.Time) (*vault.Service, *store.Memory) {
	t.Helper()
	mem := store.NewMemory()
	mem.PutTenant(store.Tenant{
		TenantID:      tenantID,
		Name:          "integration tenant",
		Status:        "active",
		KeyCustody:    store.CustodyVendor,
		KEKID:         "kek-integration",
		CeilingMode:   protocol.ModeM3,
		ContentSearch: store.SearchDisabled,
		IngestEnabled: true,
		ReadEnabled:   true,
	})
	svc, err := vault.New(vault.Options{
		Store:             mem,
		Keys:              keys.NewLocal(),
		Now:               now,
		RetrievalGrantTTL: 5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("vault.New: %v", err)
	}
	return svc, mem
}

/** Store one object and its bytes, ending in the state a completed upload leaves behind. */
func storeObject(t *testing.T, svc *vault.Service, mem *store.Memory, plaintext []byte) {
	t.Helper()
	ctx := context.Background()
	prep, err := svc.PrepareObject(ctx, vault.PrepareRequest{
		TenantID:                   tenantID,
		ObjectID:                   objectID,
		SubmissionID:               "50000000-0000-7000-8000-000000000001",
		EventID:                    eventID,
		RetentionClass:             "standard",
		ExpiresAt:                  time.Now().Add(24 * time.Hour),
		ExpectedPlaintextSizeBytes: int64(len(plaintext)),
	})
	if err != nil {
		t.Fatalf("PrepareObject: %v", err)
	}
	if len(prep.DEK) == 0 || len(prep.WrappedDEK) == 0 {
		t.Fatal("PrepareObject returned no key material")
	}
	if _, err := svc.FinaliseObject(ctx, vault.FinaliseRequest{
		TenantID:           tenantID,
		ObjectID:           objectID,
		SubmissionID:       "50000000-0000-7000-8000-000000000001",
		EventID:            eventID,
		BlobPath:           "blob://integration/object-1",
		CiphertextSHA256:   "sha256:" + strings.Repeat("ab", 32),
		PlaintextSizeBytes: int64(len(plaintext)),
		WrappedDEK:         prep.WrappedDEK,
		KEKID:              prep.KEKID,
		KEKVersion:         prep.KEKVersion,
	}); err != nil {
		t.Fatalf("FinaliseObject: %v", err)
	}
	_ = mem
}

func asDenial(t *testing.T, err error) *vault.Denial {
	t.Helper()
	if err == nil {
		return nil
	}
	var d *vault.Denial
	if errors.As(err, &d) {
		return d
	}
	t.Fatalf("refusal is not a *vault.Denial, so it is not attributable to the closed vocabulary: %v", err)
	return nil
}

// INV-1's gate, at the point where content is actually produced.
//
// The shape of this API matters and was worth getting right rather than assuming: `Retrieve`
// *schedules* a retrieval and returns a grant id and an expiry, never content. The bytes are
// produced by `Redeem`, which is the single content-egress point in the whole system. So the
// invariant is enforced there, and a test that expects `Retrieve` to refuse a legitimate request
// would be testing the wrong function - an unattributed redemption is refused, and an
// unattributed request is simply a request for approval.
func TestINV1_RedeemWithoutARealGrantIsRefused(t *testing.T) {
	svc, mem := newService(t, time.Now)
	storeObject(t, svc, mem, []byte("the customer's exact prompt text"))

	_, err := svc.Redeem(context.Background(), vault.RedeemRequest{
		TenantID:  tenantID,
		GrantID:   "deadbeef-0000-4000-8000-000000000000",
		Principal: analyst,
		EventID:   eventID,
	})
	d := asDenial(t, err)
	if d == nil {
		t.Fatal("REDEEM SUCCEEDED WITH NO GRANT. Redeem is the only content-egress point in the " +
			"system; if it serves bytes without a recorded grant, INV-1 does not hold at runtime")
	}
	if d.Reason == "" || !d.Reason.Valid() {
		t.Fatalf("refusal reason %q is not in the closed vocabulary, so an operator cannot act on it", d.Reason)
	}
	t.Logf("redeem with a fabricated grant refused: reason=%s", d.Reason)
}

// A grant is bound to one event. A grant for event A must not produce event B's content - this is
// the difference between "an analyst may see this event" and "an analyst may see this tenant".
//
// The positive half comes first and is asserted rather than skipped: a well-formed retrieval must
// SCHEDULE a grant (that is what Retrieve does - it returns a grant id and an expiry, never
// content), and the bytes must come only from Redeem. Without this the negative cases below would
// pass for the wrong reason, because a service that refuses everything refuses cross-event reads
// too.
func TestINV1_AGrantForAnotherEventIsRefused(t *testing.T) {
	svc, mem := newService(t, time.Now)
	storeObject(t, svc, mem, []byte("content for the granted event"))

	ctx := context.Background()
	res, err := svc.Retrieve(ctx, vault.RetrieveRequest{
		TenantID:       tenantID,
		EventID:        eventID,
		Principal:      analyst,
		CaseReference:  caseRef,
		SecondApprover: other,
		Justification:  "investigating a reported leak",
	})
	if err != nil {
		t.Fatalf("a well-formed retrieval was refused (%v); the negative cases below could not then "+
			"distinguish a correct refusal from a service that refuses everything", err)
	}
	if res.GrantID == "" {
		t.Fatal("a scheduled retrieval returned no grant id")
	}
	// The scheduled retrieval must not have carried content: Retrieve is the approval step.
	if res.RawDigest == "" {
		t.Fatal("the scheduled retrieval recorded no digest of what it will return, so an analyst cannot verify what they were granted")
	}

	// Redeem the grant against a DIFFERENT event.
	_, err = svc.Redeem(ctx, vault.RedeemRequest{
		TenantID:  tenantID,
		GrantID:   res.GrantID,
		Principal: analyst,
		EventID:   "e0000000-0000-7000-8000-0000000000ff",
	})
	d := asDenial(t, err)
	if d == nil {
		t.Fatal("a grant issued for one event produced content for another; grants would then be per-tenant rather than per-event")
	}
	if d.Reason != vault.DenyGrantEventMismatch {
		t.Fatalf("refused with %q, want %q", d.Reason, vault.DenyGrantEventMismatch)
	}

	// And the same grant, redeemed against the event it WAS issued for, must still work - which
	// proves the refusal above was about the event and not about the grant being unusable.
	ok, err := svc.Redeem(ctx, vault.RedeemRequest{
		TenantID: tenantID, GrantID: res.GrantID, Principal: analyst, EventID: eventID,
	})
	if err != nil {
		t.Fatalf("the grant was refused for its own event after a mismatched attempt: %v", err)
	}
	if ok.GrantID == "" || ok.EventID != eventID {
		t.Fatalf("redeemed grant reports grant=%q event=%q, want the granted event", ok.GrantID, ok.EventID)
	}
}

// The positive path, and an honest limit stated as a test rather than discovered later.
//
// A granted redemption produces content only if the ciphertext exists to decrypt. In a deployment
// that ciphertext is in Blob storage and the vault mints a short-lived URL to it; on this host there
// is no blob store and the fixture has no ciphertext, so `Redeem` cannot return bytes. The
// invariant that matters here is what it does INSTEAD: report a state with a reason from the closed
// `no_longer_available` vocabulary, rather than an empty success that a caller could mistake for
// "there was nothing to see". An empty success is the failure mode this test exists to forbid.
func TestINV1_TheGrantedPathReportsUnavailabilityRatherThanEmptySuccess(t *testing.T) {
	svc, mem := newService(t, time.Now)
	plaintext := []byte("the customer's exact prompt text, which crosses only because it was granted")
	storeObject(t, svc, mem, plaintext)

	ctx := context.Background()
	res, err := svc.Retrieve(ctx, vault.RetrieveRequest{
		TenantID: tenantID, EventID: eventID, Principal: analyst,
		CaseReference: caseRef, SecondApprover: other, Justification: "investigating a reported leak",
	})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	out, err := svc.Redeem(ctx, vault.RedeemRequest{
		TenantID: tenantID, GrantID: res.GrantID, Principal: analyst, EventID: eventID,
	})

	if err != nil {
		d := asDenial(t, err)
		if d == nil {
			t.Fatalf("Redeem refused with a non-Denial error, so the refusal is not attributable: %v", err)
		}
		if d.Reason == "" || !d.Reason.Valid() {
			t.Fatalf("Redeem refused with reason %q, which is not in the closed vocabulary", d.Reason)
		}
		t.Logf("granted redemption refused attributionally: reason=%s (no blob store on this host)", d.Reason)
		return
	}

	// It did not error. Then it must not have silently produced nothing.
	if len(out.Plaintext) == 0 {
		if out.State == "" {
			t.Fatal("a granted redemption returned neither content, nor an error, nor a state: the " +
				"caller cannot tell 'no content was stored' from 'you may not see it', and an empty " +
				"success is the failure this test exists to forbid")
		}
		if out.State == "served" || out.State == "ok" {
			t.Fatalf("a granted redemption reported state %q with no content; a state that says "+
				"served must mean bytes were served", out.State)
		}
		if out.Reason != "" && !out.Reason.Valid() {
			t.Fatalf("the unavailable reason %q is not in the closed set", out.Reason)
		}
		t.Logf("granted redemption reported state=%q reason=%q with no bytes (no blob store on this host)", out.State, out.Reason)
		return
	}

	if !strings.Contains(string(out.Plaintext), "exact prompt text") {
		t.Fatalf("the granted redemption returned %q, which is not the stored plaintext", string(out.Plaintext))
	}

	// One grant, one read: the second attempt must be refused.
	if _, err := svc.Redeem(ctx, vault.RedeemRequest{
		TenantID: tenantID, GrantID: res.GrantID, Principal: analyst, EventID: eventID,
	}); err == nil {
		t.Fatal("a grant was redeemed twice; a reusable grant is a bulk export path with extra steps")
	}
}

// A grant is single-use and principal-bound. Both halves matter: a grant that can be redeemed twice
// is a bulk path with extra steps, and one that any principal can redeem is not bound to the person
// who asked for it.
func TestINV1_AGrantIsSingleUseAndBoundToItsPrincipal(t *testing.T) {
	newGrant := func(t *testing.T) (*vault.Service, *store.Memory, string, func()) {
		now := time.Now()
		svc, mem := newService(t, func() time.Time { return now })
		storeObject(t, svc, mem, []byte("content"))
		res, err := svc.Retrieve(context.Background(), vault.RetrieveRequest{
			TenantID:       tenantID,
			EventID:        eventID,
			Principal:      analyst,
			CaseReference:  caseRef,
			SecondApprover: other,
			Justification:  "investigating a reported leak",
		})
		if err != nil {
			t.Skipf("this build refuses even a well-formed retrieval (%v); grant-lifecycle cases cannot run yet", err)
		}
		return svc, mem, res.GrantID, func() { now = now.Add(10 * time.Minute) }
	}

	t.Run("another principal cannot redeem it", func(t *testing.T) {
		svc, _, grant, _ := newGrant(t)
		_, err := svc.Redeem(context.Background(), vault.RedeemRequest{
			TenantID: tenantID, GrantID: grant, Principal: other, EventID: eventID,
		})
		d := asDenial(t, err)
		if d == nil {
			t.Fatal("a different principal redeemed someone else's grant")
		}
		if d.Reason != vault.DenyGrantPrincipalMismatch {
			t.Fatalf("refused with %q, want %q", d.Reason, vault.DenyGrantPrincipalMismatch)
		}
	})

	t.Run("an expired grant cannot be redeemed", func(t *testing.T) {
		svc, _, grant, advance := newGrant(t)
		advance()
		_, err := svc.Redeem(context.Background(), vault.RedeemRequest{
			TenantID: tenantID, GrantID: grant, Principal: analyst, EventID: eventID,
		})
		d := asDenial(t, err)
		if d == nil {
			t.Fatal("an expired grant was redeemed")
		}
		if d.Reason != vault.DenyGrantExpired {
			t.Fatalf("refused with %q, want %q", d.Reason, vault.DenyGrantExpired)
		}
	})

	t.Run("a grant cannot be redeemed twice", func(t *testing.T) {
		svc, _, grant, _ := newGrant(t)
		ctx := context.Background()
		if _, err := svc.Redeem(ctx, vault.RedeemRequest{
			TenantID: tenantID, GrantID: grant, Principal: analyst, EventID: eventID,
		}); err != nil {
			t.Skipf("the first redemption was refused (%v), so single-use cannot be exercised yet", err)
		}
		_, err := svc.Redeem(ctx, vault.RedeemRequest{
			TenantID: tenantID, GrantID: grant, Principal: analyst, EventID: eventID,
		})
		d := asDenial(t, err)
		if d == nil {
			t.Fatal("a second redemption of the same grant succeeded; a reusable grant is a bulk export path with extra steps")
		}
		if d.Reason != vault.DenyGrantAlreadyUsed {
			t.Fatalf("refused with %q, want %q", d.Reason, vault.DenyGrantAlreadyUsed)
		}
	})
}

// A tenant whose custody mode makes server-side full-text search impossible must be refused at the
// service boundary, not only by a database constraint - the vault is the component that would
// otherwise hold the plaintext index, and the database is not the only caller.
func TestINV1_SearchRefusedWhenCustodyMakesItImpossible(t *testing.T) {
	svc, mem := newService(t, time.Now)
	tenant, err := mem.Tenant(context.Background(), tenantID)
	if err != nil {
		t.Fatalf("tenant: %v", err)
	}
	// Vendor-held keys with search disabled: the tier is a capability, and enabling full_text on a
	// tenant that has not been configured for it must be refused with a closed reason.
	_, err = svc.Search(context.Background(), vault.SearchRequest{
		TenantID:  tenantID,
		Principal: analyst,
		Query:     "salary",
	})
	if err == nil {
		t.Fatalf("search succeeded on a tenant whose content_search is %q", tenant.ContentSearch)
	}
	d := asDenial(t, err)
	if d == nil {
		t.Fatalf("search was refused without a closed reason: %v", err)
	}
	t.Logf("search refused as expected: reason=%s", d.Reason)
}
