package vault_test

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/shadow-ai-capture/content-vault/internal/keys"
	"github.com/shadow-ai-capture/content-vault/internal/store"
	"github.com/shadow-ai-capture/content-vault/internal/testrig"
	"github.com/shadow-ai-capture/content-vault/internal/vault"
)

// encryptWithDEK is the device's half of the hierarchy: AES-256-GCM under the per-object data key.
// The rotation test uses it to prove that a re-wrap does not make an existing ciphertext
// unreadable — the property "re-wrap, never re-encrypt" exists to protect.
func encryptWithDEK(t *testing.T, dek, plaintext []byte) []byte {
	t.Helper()
	block, err := aes.NewCipher(dek)
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	nonce := bytes.Repeat([]byte{7}, gcm.NonceSize())
	return gcm.Seal(nil, nonce, plaintext, nil)
}

func decryptWithDEK(t *testing.T, dek, ciphertext []byte) []byte {
	t.Helper()
	block, err := aes.NewCipher(dek)
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	nonce := bytes.Repeat([]byte{7}, gcm.NonceSize())
	out, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		t.Fatalf("the content no longer decrypts under its data key: %v", err)
	}
	return out
}

// ---------------------------------------------------------------------------------------
// The grant matrix
// ---------------------------------------------------------------------------------------

// TestGrantMatrix is the deliverable's core: every row is a way a retrieval can be attempted, and
// every refusal carries a reason from the closed set.
func TestGrantMatrix(t *testing.T) {
	const principal = "analyst@example.com"

	t.Run("a valid grant redeems once and returns the content path", func(t *testing.T) {
		rig := testrig.New(t, testrig.Options{})
		obj := rig.Store(t, testrig.TenantID, testrig.ObjectA, testrig.SubmissionA, testrig.EventA)
		grantID := rig.Retrieve(t, principal, testrig.EventA)

		res, err := rig.Service.Redeem(context.Background(), vault.RedeemRequest{
			TenantID: testrig.TenantID, GrantID: grantID, Principal: principal, EventID: testrig.EventA,
		})
		if err != nil {
			t.Fatalf("redeem: %v", err)
		}
		if res.State != "available" {
			t.Fatalf("state is %q, want available", res.State)
		}
		if res.RawDigest != obj.Digest {
			t.Errorf("raw digest is %q, want %q", res.RawDigest, obj.Digest)
		}
		if res.BlobPath != obj.BlobPath {
			t.Errorf("blob path is %q, want %q", res.BlobPath, obj.BlobPath)
		}
		if !res.GrantExpiresAt.After(rig.Clock.T) {
			t.Errorf("the grant expires at %s, which is not after now (%s)", res.GrantExpiresAt, rig.Clock.T)
		}
	})

	t.Run("no grant is denied with grant_required", func(t *testing.T) {
		rig := testrig.New(t, testrig.Options{})
		rig.Store(t, testrig.TenantID, testrig.ObjectA, testrig.SubmissionA, testrig.EventA)
		_, err := rig.Service.Redeem(context.Background(), vault.RedeemRequest{
			TenantID: testrig.TenantID, GrantID: "99999999-9999-4999-8999-999999999999",
			Principal: principal, EventID: testrig.EventA,
		})
		assertDenial(t, err, vault.DenyGrantRequired)
	})

	t.Run("an expired grant is denied with grant_expired", func(t *testing.T) {
		rig := testrig.New(t, testrig.Options{GrantTTL: time.Minute})
		rig.Store(t, testrig.TenantID, testrig.ObjectA, testrig.SubmissionA, testrig.EventA)
		grantID := rig.Retrieve(t, principal, testrig.EventA)

		// One second past the window. Time is moved on the injected clock, so the test is not
		// waiting for anything and cannot flake on a slow machine.
		rig.Clock.Advance(time.Minute + time.Second)
		_, err := rig.Service.Redeem(context.Background(), vault.RedeemRequest{
			TenantID: testrig.TenantID, GrantID: grantID, Principal: principal, EventID: testrig.EventA,
		})
		assertDenial(t, err, vault.DenyGrantExpired)
	})

	t.Run("an already-used grant is denied with grant_already_used", func(t *testing.T) {
		rig := testrig.New(t, testrig.Options{})
		rig.Store(t, testrig.TenantID, testrig.ObjectA, testrig.SubmissionA, testrig.EventA)
		grantID := rig.Retrieve(t, principal, testrig.EventA)

		if _, err := rig.Service.Redeem(context.Background(), vault.RedeemRequest{
			TenantID: testrig.TenantID, GrantID: grantID, Principal: principal, EventID: testrig.EventA,
		}); err != nil {
			t.Fatalf("first redemption: %v", err)
		}
		_, err := rig.Service.Redeem(context.Background(), vault.RedeemRequest{
			TenantID: testrig.TenantID, GrantID: grantID, Principal: principal, EventID: testrig.EventA,
		})
		assertDenial(t, err, vault.DenyGrantAlreadyUsed)
	})

	t.Run("a grant for event A used against event B is denied", func(t *testing.T) {
		rig := testrig.New(t, testrig.Options{})
		rig.Store(t, testrig.TenantID, testrig.ObjectA, testrig.SubmissionA, testrig.EventA)
		rig.Store(t, testrig.TenantID, testrig.ObjectB, testrig.SubmissionB, testrig.EventB)
		grantID := rig.Retrieve(t, principal, testrig.EventA)

		_, err := rig.Service.Redeem(context.Background(), vault.RedeemRequest{
			TenantID: testrig.TenantID, GrantID: grantID, Principal: principal, EventID: testrig.EventB,
		})
		assertDenial(t, err, vault.DenyGrantEventMismatch)
	})

	t.Run("a grant issued to one principal is denied to another", func(t *testing.T) {
		rig := testrig.New(t, testrig.Options{})
		rig.Store(t, testrig.TenantID, testrig.ObjectA, testrig.SubmissionA, testrig.EventA)
		grantID := rig.Retrieve(t, principal, testrig.EventA)

		_, err := rig.Service.Redeem(context.Background(), vault.RedeemRequest{
			TenantID: testrig.TenantID, GrantID: grantID, Principal: "someone.else@example.com", EventID: testrig.EventA,
		})
		assertDenial(t, err, vault.DenyGrantPrincipalMismatch)
	})

	t.Run("concurrent redemptions of one grant yield exactly one success", func(t *testing.T) {
		rig := testrig.New(t, testrig.Options{})
		rig.Store(t, testrig.TenantID, testrig.ObjectA, testrig.SubmissionA, testrig.EventA)
		grantID := rig.Retrieve(t, principal, testrig.EventA)

		const attempts = 8
		var wg sync.WaitGroup
		results := make(chan error, attempts)
		for i := 0; i < attempts; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := rig.Service.Redeem(context.Background(), vault.RedeemRequest{
					TenantID: testrig.TenantID, GrantID: grantID, Principal: principal, EventID: testrig.EventA,
				})
				results <- err
			}()
		}
		wg.Wait()
		close(results)
		ok, used := 0, 0
		for err := range results {
			switch {
			case err == nil:
				ok++

			default:
				if d, isDenial := vault.IsDenial(err); isDenial && d.Reason == vault.DenyGrantAlreadyUsed {
					used++
					continue
				}
				t.Errorf("unexpected concurrent outcome: %v", err)
			}
		}
		if ok != 1 || used != attempts-1 {
			t.Fatalf("concurrent redemption: %d succeeded, %d refused as used; want 1 and %d", ok, used, attempts-1)
		}
	})

	t.Run("C16's approval fields are required and distinct", func(t *testing.T) {
		rig := testrig.New(t, testrig.Options{})
		rig.Store(t, testrig.TenantID, testrig.ObjectA, testrig.SubmissionA, testrig.EventA)
		base := vault.RetrieveRequest{
			TenantID: testrig.TenantID, EventID: testrig.EventA, Principal: principal,
			CaseReference: "CASE-42", SecondApprover: "approver@example.com",
		}

		noCase := base
		noCase.CaseReference = ""
		_, err := rig.Service.Retrieve(context.Background(), noCase)
		assertDenial(t, err, vault.DenyCaseReferenceRequired)

		noApprover := base
		noApprover.SecondApprover = ""
		_, err = rig.Service.Retrieve(context.Background(), noApprover)
		assertDenial(t, err, vault.DenySecondApproverRequired)

		selfApproved := base
		selfApproved.SecondApprover = principal
		_, err = rig.Service.Retrieve(context.Background(), selfApproved)
		assertDenial(t, err, vault.DenySecondApproverNotDistinct)
	})

	t.Run("an event with no stored content is refused, not served empty", func(t *testing.T) {
		rig := testrig.New(t, testrig.Options{})
		_, err := rig.Service.Retrieve(context.Background(), vault.RetrieveRequest{
			TenantID: testrig.TenantID, EventID: testrig.EventA, Principal: principal,
			CaseReference: "CASE-42", SecondApprover: "approver@example.com",
		})
		assertDenial(t, err, vault.DenyNoContentObject)
	})

	t.Run("a tenant whose reads are disabled is refused", func(t *testing.T) {
		rig := testrig.New(t, testrig.Options{Tenants: []store.Tenant{
			testrig.Tenant(func(tn *store.Tenant) { tn.ReadEnabled = false }),
		}})
		rig.Store(t, testrig.TenantID, testrig.ObjectA, testrig.SubmissionA, testrig.EventA)
		_, err := rig.Service.Retrieve(context.Background(), vault.RetrieveRequest{
			TenantID: testrig.TenantID, EventID: testrig.EventA, Principal: principal,
			CaseReference: "CASE-42", SecondApprover: "approver@example.com",
		})
		assertDenial(t, err, vault.DenyRetrievalDisabled)
	})
}

// TestEveryRefusalReasonIsInTheClosedSet guards the closed set itself: a refusal that is not in the
// set cannot be counted, and §10.2's four must be members.
func TestEveryRefusalReasonIsInTheClosedSet(t *testing.T) {
	for _, r := range vault.AllDenialReasons {
		if !r.Valid() {
			t.Errorf("reason %q reports itself as outside the closed set", r)
		}
	}
	for _, r := range vault.GrantDecisionReasons {
		if !r.Valid() {
			t.Errorf("§10.2 reason %q is missing from the closed set", r)
		}
	}
	if len(vault.GrantDecisionReasons) != 4 {
		t.Errorf("§10.2 enumerates four grant-decision reasons, the package carries %d", len(vault.GrantDecisionReasons))
	}
	if vault.DenialReason("not_a_reason").Valid() {
		t.Error("the closed set accepted an invented reason")
	}
	for _, r := range vault.AllUnavailableReasons {
		if !r.Valid() {
			t.Errorf("unavailability %q is outside the closed set", r)
		}
	}
	if len(vault.AllUnavailableReasons) != 5 {
		t.Errorf("docs/02 §11 enumerates five no_longer_available reasons, the package carries %d", len(vault.AllUnavailableReasons))
	}
}

// ---------------------------------------------------------------------------------------
// Rotation
// ---------------------------------------------------------------------------------------

// TestRotationRewrapsWithoutReencrypting is the rotation requirement: an object encrypted under
// tenant key version N stays readable after rotation to N+1, nothing re-encrypts, and the old
// version is not used for new writes.
func TestRotationRewrapsWithoutReencrypting(t *testing.T) {
	ctx := context.Background()
	rig := testrig.New(t, testrig.Options{})
	plaintext := []byte("the prompt that must survive a rotation")

	first := rig.Store(t, testrig.TenantID, testrig.ObjectA, testrig.SubmissionA, testrig.EventA)
	ciphertext := encryptWithDEK(t, first.DEK, plaintext)
	if first.KEKVersion != "1" {
		t.Fatalf("the first object was sealed under version %q, want 1", first.KEKVersion)
	}

	// A second object, so the rotation has more than one row to re-wrap.
	second := rig.Store(t, testrig.TenantID, testrig.ObjectB, testrig.SubmissionB, testrig.EventB)

	before, err := rig.Memory.ContentObject(ctx, testrig.TenantID, testrig.ObjectA)
	if err != nil {
		t.Fatal(err)
	}

	rot, err := rig.Service.RotateTenant(ctx, testrig.TenantID)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if rot.OldVersion != "1" || rot.NewVersion != "2" {
		t.Fatalf("rotation went %s -> %s, want 1 -> 2", rot.OldVersion, rot.NewVersion)
	}
	if rot.Rewrapped != 2 || len(rot.Failed) != 0 {
		t.Fatalf("rotation re-wrapped %d objects with %d failures; want 2 and 0", rot.Rewrapped, len(rot.Failed))
	}

	after, err := rig.Memory.ContentObject(ctx, testrig.TenantID, testrig.ObjectA)
	if err != nil {
		t.Fatal(err)
	}
	if after.KEKVersion != "2" {
		t.Errorf("object A is on key version %q, want 2", after.KEKVersion)
	}
	// Nothing else about the object changed: not the blob, not its digest, not its size. If a
	// byte of those moved, the rotation re-encrypted, which is the thing it must not do.
	if after.CiphertextSHA256 != before.CiphertextSHA256 || after.BlobPath != before.BlobPath ||
		after.PlaintextSizeBytes != before.PlaintextSizeBytes {
		t.Errorf("rotation changed the stored object: before %+v after %+v", before, after)
	}
	if bytes.Equal(after.WrappedDEK, before.WrappedDEK) {
		t.Error("the wrapped key did not change, so it was not re-wrapped")
	}

	// The real proof: unwrap under the new version and decrypt the ciphertext that was produced
	// before the rotation.
	aad := keys.AAD{TenantID: testrig.TenantID, ObjectID: testrig.ObjectA, KEKID: after.KEKID, KEKVersion: after.KEKVersion}
	dek, err := rig.Keys.Unwrap(ctx, aad, after.WrappedDEK)
	if err != nil {
		t.Fatalf("unwrapping after rotation: %v", err)
	}
	if !bytes.Equal(dek, first.DEK) {
		t.Error("rotation produced a different data key, so the content was re-encrypted")
	}
	if got := decryptWithDEK(t, dek, ciphertext); !bytes.Equal(got, plaintext) {
		t.Fatalf("content encrypted before the rotation no longer decrypts: %q", got)
	}

	// The old version stays in the key store: a row that failed to re-wrap must remain readable,
	// and deleting it would turn a partial rotation into data loss.
	versions := rig.Keys.Versions("kek-" + testrig.TenantID)
	if len(versions) < 2 || versions[0] != "1" || versions[1] != "2" {
		t.Errorf("key versions after rotation: %v; both 1 and 2 must remain", versions)
	}

	// A new write is sealed under the new version: the old one is not silently used again.
	third := rig.Store(t, testrig.TenantID, "cccccccc-0000-4000-8000-000000000003", testrig.SubmissionA, "cccccccc-2222-4222-8222-0000000000ec")
	if third.KEKVersion != "2" {
		t.Fatalf("a new object after rotation was sealed under version %q, want 2", third.KEKVersion)
	}
	_ = second
}

// TestRotationAuditsAndReportsFailures covers the two things a rotation must not do silently.
func TestRotationAuditsAndReportsFailures(t *testing.T) {
	ctx := context.Background()
	rig := testrig.New(t, testrig.Options{})
	rig.Store(t, testrig.TenantID, testrig.ObjectA, testrig.SubmissionA, testrig.EventA)

	rot, err := rig.Service.RotateTenant(ctx, testrig.TenantID)
	if err != nil {
		t.Fatal(err)
	}
	if rot.NewVersion != "2" {
		t.Fatalf("new version %q", rot.NewVersion)
	}
	found := false
	for _, row := range rig.Memory.Audit() {
		if row.Action == vault.ActionKeyRotated {
			found = true
			if row.Detail["new_version"] != "2" {
				t.Errorf("the rotation audit row does not name the new version: %+v", row.Detail)
			}
		}
	}
	if !found {
		t.Error("the rotation wrote no audit row")
	}

	// Rotating a tenant with no KEK is refused rather than treated as a no-op.
	noKey := testrig.New(t, testrig.Options{Tenants: []store.Tenant{
		testrig.Tenant(func(tn *store.Tenant) { tn.KEKID = "" }),
	}})
	if _, err := noKey.Service.RotateTenant(ctx, testrig.TenantID); err == nil {
		t.Error("a tenant with no KEK rotated successfully")
	}
}

// ---------------------------------------------------------------------------------------
// Erasure
// ---------------------------------------------------------------------------------------

// TestErasureDestroysContentAndReportsItDestroyed is §6.4 plus the receipt requirement: after
// erasure the ciphertext is unreadable and the read path says so as a *result*, not an error.
func TestErasureDestroysContentAndReportsItDestroyed(t *testing.T) {
	ctx := context.Background()
	const principal = "analyst@example.com"
	rig := testrig.New(t, testrig.Options{Tenants: []store.Tenant{
		testrig.Tenant(func(tn *store.Tenant) { tn.ContentSearch = store.SearchAttachmentNames }),
	}})
	obj := rig.Store(t, testrig.TenantID, testrig.ObjectA, testrig.SubmissionA, testrig.EventA,
		vault.IndexUnit{UnitKind: store.UnitAttachmentName, UnitIndex: 0, Body: "Q3-contract.pdf"})
	grantID := rig.Retrieve(t, principal, testrig.EventA)

	res, err := rig.Service.ShredObject(ctx, vault.ShredRequest{
		TenantID: testrig.TenantID, ObjectID: obj.ObjectID, Reason: "erasure", RequestedBy: "dpo@example.com",
	})
	if err != nil {
		t.Fatalf("shred: %v", err)
	}
	if res.AlreadyShredded {
		t.Fatal("the object reported itself already shredded")
	}
	if res.Receipt.ReceiptID == "" || len(res.Receipt.Mechanisms) == 0 {
		t.Fatalf("the erasure produced no receipt: %+v", res.Receipt)
	}

	// 1. The wrapped key is gone: what is stored cannot be unwrapped by anyone, including a reader
	//    who reaches the row directly.
	row, err := rig.Memory.ContentObject(ctx, testrig.TenantID, obj.ObjectID)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != store.StateShredded || row.ShreddedReason != "erasure" || row.ShreddedAt.IsZero() {
		t.Errorf("the row does not record the shred: %+v", row)
	}
	aad := keys.AAD{TenantID: testrig.TenantID, ObjectID: obj.ObjectID, KEKID: row.KEKID, KEKVersion: row.KEKVersion}
	if _, err := rig.Keys.Unwrap(ctx, aad, row.WrappedDEK); err == nil {
		t.Error("the wrapped key still unwraps after erasure")
	}

	// 2. The read path reports it as destroyed, not as an error, and links the receipt.
	_, err = rig.Service.Retrieve(ctx, vault.RetrieveRequest{
		TenantID: testrig.TenantID, EventID: testrig.EventA, Principal: principal,
		CaseReference: "CASE-42", SecondApprover: "approver@example.com",
	})
	unavail, ok := vault.IsUnavailable(err)
	if !ok {
		t.Fatalf("retrieving erased content returned %v, want a no_longer_available result", err)
	}
	if unavail.Reason != vault.UnavailableErasure {
		t.Errorf("unavailability reason is %q, want erasure", unavail.Reason)
	}
	if unavail.ReceiptRef != res.Receipt.ReceiptID {
		t.Errorf("the unavailability does not link the receipt: %q vs %q", unavail.ReceiptRef, res.Receipt.ReceiptID)
	}

	// 3. A grant issued before the erasure also reports the content as destroyed.
	redeem, err := rig.Service.Redeem(ctx, vault.RedeemRequest{
		TenantID: testrig.TenantID, GrantID: grantID, Principal: principal, EventID: testrig.EventA,
	})
	if err != nil {
		t.Fatalf("redeeming a grant whose content was erased returned an error: %v", err)
	}
	if redeem.State != "no_longer_available" || redeem.Reason != vault.UnavailableErasure {
		t.Errorf("redeem state %q reason %q, want no_longer_available/erasure", redeem.State, redeem.Reason)
	}

	// 4. Erasure reaches the index too: key destruction cannot touch a table that is not under a
	//    key, which is §6.4's footnote.
	left, err := rig.Memory.DeleteSearchText(ctx, testrig.TenantID, obj.SubmissionID)
	if err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Errorf("the erasure left %d index rows behind", left)
	}
	if res.SearchRowsRemoved != 1 {
		t.Errorf("the receipt says %d index rows were removed, want 1", res.SearchRowsRemoved)
	}
	if res.Receipt.RemovedCounts["search_text"] != 1 {
		t.Errorf("the receipt's counts do not include the index: %+v", res.Receipt.RemovedCounts)
	}

	// 5. Erasure is idempotent and does not claim a second deletion.
	again, err := rig.Service.ShredObject(ctx, vault.ShredRequest{
		TenantID: testrig.TenantID, ObjectID: obj.ObjectID, Reason: "erasure", RequestedBy: "dpo@example.com",
	})
	if err != nil {
		t.Fatalf("a second erasure returned an error: %v", err)
	}
	if !again.AlreadyShredded {
		t.Error("a second erasure did not report the object as already shredded")
	}
	if again.Receipt.ReceiptID != "" {
		t.Error("a second erasure wrote a second receipt")
	}
}

// TestTenantKeyDestructionMakesContentUnavailable is the offboarding case: destroying the KEK
// destroys every object of the tenant at once, and the read path reports `key_unavailable`.
func TestTenantKeyDestructionMakesContentUnavailable(t *testing.T) {
	ctx := context.Background()
	const principal = "analyst@example.com"
	rig := testrig.New(t, testrig.Options{})
	rig.Store(t, testrig.TenantID, testrig.ObjectA, testrig.SubmissionA, testrig.EventA)

	res, err := rig.Service.ShredObject(ctx, vault.ShredRequest{
		TenantID: testrig.TenantID, ObjectID: testrig.ObjectA, Reason: "tenant_offboarded",
		RequestedBy: "ops@example.com", DestroyTenantKey: true,
	})
	if err != nil {
		t.Fatalf("offboarding shred: %v", err)
	}
	if !res.KeyDestroyed {
		t.Fatal("the tenant key was not destroyed")
	}
	if _, gone := rig.Keys.DestroyedReason(testrig.Tenant().KEKID); !gone {
		t.Error("the key store does not report the KEK as destroyed")
	}

	_, err = rig.Service.Retrieve(ctx, vault.RetrieveRequest{
		TenantID: testrig.TenantID, EventID: testrig.EventA, Principal: principal,
		CaseReference: "CASE-42", SecondApprover: "approver@example.com",
	})
	unavail, ok := vault.IsUnavailable(err)
	if !ok {
		t.Fatalf("retrieving offboarded content returned %v, want a no_longer_available result", err)
	}
	if unavail.Reason != vault.UnavailableTenantOffboarded {
		t.Errorf("unavailability reason is %q, want tenant_offboarded", unavail.Reason)
	}

	// A pre-existing wrapped key cannot be opened at all: this is the "destroying the key destroys
	// the content" property, exercised on bytes that are still in the row.
	obj, err := rig.Memory.ContentObject(ctx, testrig.TenantID, testrig.ObjectA)
	if err != nil {
		t.Fatal(err)
	}
	aad := keys.AAD{TenantID: testrig.TenantID, ObjectID: testrig.ObjectA, KEKID: obj.KEKID, KEKVersion: obj.KEKVersion}
	if _, err := rig.Keys.Unwrap(ctx, aad, obj.WrappedDEK); !errors.Is(err, keys.ErrKeyDestroyed) {
		t.Errorf("unwrapping after key destruction returned %v, want ErrKeyDestroyed", err)
	}
}

// TestRetentionExpiryIsReportedAsUnavailable covers the other §11 reason: an object past its
// retention is unavailable whether or not the sweep has run.
func TestRetentionExpiryIsReportedAsUnavailable(t *testing.T) {
	ctx := context.Background()
	rig := testrig.New(t, testrig.Options{})
	rig.Store(t, testrig.TenantID, testrig.ObjectA, testrig.SubmissionA, testrig.EventA)
	rig.Clock.Advance(200 * 24 * time.Hour) // past the fixture's 90-day retention

	_, err := rig.Service.Retrieve(ctx, vault.RetrieveRequest{
		TenantID: testrig.TenantID, EventID: testrig.EventA, Principal: "analyst@example.com",
		CaseReference: "CASE-42", SecondApprover: "approver@example.com",
	})
	unavail, ok := vault.IsUnavailable(err)
	if !ok {
		t.Fatalf("retrieving expired content returned %v, want a no_longer_available result", err)
	}
	if unavail.Reason != vault.UnavailableRetentionExpired {
		t.Errorf("reason %q, want retention_expired", unavail.Reason)
	}
}

// ---------------------------------------------------------------------------------------
// Search tiers and the ADR 0014 invariant at the service boundary
// ---------------------------------------------------------------------------------------

func TestSearchTierRefusals(t *testing.T) {
	ctx := context.Background()

	t.Run("a disabled tenant may not search", func(t *testing.T) {
		rig := testrig.New(t, testrig.Options{ScopeTiers: map[string]store.SearchTier{"tool:chatgpt": store.SearchFullText}})
		_, err := rig.Service.Search(ctx, vault.SearchRequest{
			TenantID: testrig.TenantID, Principal: "a@example.com", Scope: "tool:chatgpt", Form: store.FormTerms, Query: "secret",
		})
		assertDenial(t, err, vault.DenySearchDisabled)
	})

	t.Run("a scope the bundle does not name is refused", func(t *testing.T) {
		rig := testrig.New(t, testrig.Options{
			Tenants:    []store.Tenant{testrig.Tenant(FullText())},
			ScopeTiers: map[string]store.SearchTier{"tool:chatgpt": store.SearchFullText},
		})
		_, err := rig.Service.Search(ctx, vault.SearchRequest{
			TenantID: testrig.TenantID, Principal: "a@example.com", Scope: "tool:other", Form: store.FormTerms, Query: "secret",
		})
		assertDenial(t, err, vault.DenySearchTierNotInScope)
	})

	t.Run("the bundle narrows the tenant ceiling", func(t *testing.T) {
		rig := testrig.New(t, testrig.Options{
			Tenants:    []store.Tenant{testrig.Tenant(FullText())},
			ScopeTiers: map[string]store.SearchTier{"tool:chatgpt": store.SearchFullText, "tool:other": store.SearchAttachmentNames},
		})
		res, err := rig.Service.Search(ctx, vault.SearchRequest{
			TenantID: testrig.TenantID, Principal: "a@example.com", Scope: "tool:other", Form: store.FormTerms, Query: "contract",
		})
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		if res.Effective != store.SearchAttachmentNames {
			t.Errorf("effective tier is %q, want attachment_names", res.Effective)
		}
		for _, k := range res.UnitKinds {
			if k != store.UnitAttachmentName {
				t.Errorf("a narrowed scope searched %q", k)
			}
		}
	})

	t.Run("prompt bodies are indexed only at full_text", func(t *testing.T) {
		rig := testrig.New(t, testrig.Options{
			Tenants: []store.Tenant{testrig.Tenant(func(tn *store.Tenant) { tn.ContentSearch = store.SearchAttachmentNames })},
		})
		err := rig.Service.IndexUnit(ctx, testrig.TenantID, vault.IndexUnit{
			UnitKind: store.UnitPromptBody, Body: "a prompt that must not be indexed",
		}, testrig.SubmissionA)
		assertDenial(t, err, vault.DenyIndexUnitNotPermitted)
		if err := rig.Service.IndexUnit(ctx, testrig.TenantID, vault.IndexUnit{
			UnitKind: store.UnitAttachmentName, Body: "Q3-contract.pdf",
		}, testrig.SubmissionA); err != nil {
			t.Errorf("a filename must be indexable at attachment_names: %v", err)
		}
	})

	t.Run("full_text with customer_held is refused at the service boundary", func(t *testing.T) {
		// The database makes this pair unrepresentable. The memory store does not, precisely so
		// this test can prove the service refuses it on its own: a dropped constraint, a bypassed
		// migration or a stale replica must not turn the vault into an indexer.
		impossible := testrig.Tenant(func(tn *store.Tenant) {
			tn.ContentSearch = store.SearchFullText
			tn.KeyCustody = store.CustodyCustomerHeld
		})
		rig := testrig.New(t, testrig.Options{
			Tenants:    []store.Tenant{impossible},
			ScopeTiers: map[string]store.SearchTier{"tool:chatgpt": store.SearchFullText},
		})

		err := rig.Service.IndexUnit(ctx, testrig.TenantID, vault.IndexUnit{
			UnitKind: store.UnitPromptBody, Body: "must never be indexed",
		}, testrig.SubmissionA)
		assertDenial(t, err, vault.DenyKeyCustodySearchConflict)

		_, err = rig.Service.Search(ctx, vault.SearchRequest{
			TenantID: testrig.TenantID, Principal: "a@example.com", Scope: "tool:chatgpt", Form: store.FormTerms, Query: "must",
		})
		assertDenial(t, err, vault.DenyKeyCustodySearchConflict)

		res, err := rig.Service.FinaliseObject(ctx, vault.FinaliseRequest{
			TenantID: testrig.TenantID, ObjectID: testrig.ObjectA, SubmissionID: testrig.SubmissionA,
			EventID: testrig.EventA, BlobPath: "b", CiphertextSHA256: "sha256:" + testrig.ObjectA,
			WrappedDEK: []byte{1, 2, 3}, KEKID: "k", KEKVersion: "1",
			IndexUnits: []vault.IndexUnit{{UnitKind: store.UnitPromptBody, Body: "must never be indexed"}},
		})
		if err != nil {
			t.Fatalf("finalise: %v", err)
		}
		if len(res.Refused) != 1 || res.Refused[0].Reason != vault.DenyKeyCustodySearchConflict {
			t.Fatalf("the refused unit was not reported: %+v", res)
		}
		if res.Indexed != 0 {
			t.Errorf("the service indexed %d units it must not have", res.Indexed)
		}
	})

	t.Run("full_text below an M3 ceiling is refused", func(t *testing.T) {
		rig := testrig.New(t, testrig.Options{
			Tenants: []store.Tenant{testrig.Tenant(FullText(), func(tn *store.Tenant) {
				tn.CeilingMode = "m1"
			})},
			ScopeTiers: map[string]store.SearchTier{"tool:chatgpt": store.SearchFullText},
		})
		_, err := rig.Service.Search(ctx, vault.SearchRequest{
			TenantID: testrig.TenantID, Principal: "a@example.com", Scope: "tool:chatgpt", Form: store.FormTerms, Query: "x",
		})
		assertDenial(t, err, vault.DenySearchTierRequiresM3)
	})

	t.Run("the substring and fuzzy forms are filenames only", func(t *testing.T) {
		rig := testrig.New(t, testrig.Options{
			Tenants:    []store.Tenant{testrig.Tenant(FullText())},
			ScopeTiers: map[string]store.SearchTier{"tool:chatgpt": store.SearchFullText},
		})
		rig.Store(t, testrig.TenantID, testrig.ObjectA, testrig.SubmissionA, testrig.EventA,
			vault.IndexUnit{UnitKind: store.UnitAttachmentName, UnitIndex: 0, Body: "Q3-contract.pdf"},
			vault.IndexUnit{UnitKind: store.UnitPromptBody, UnitIndex: 0, Body: "the Q3-contract is attached"})

		sub, err := rig.Service.Search(ctx, vault.SearchRequest{
			TenantID: testrig.TenantID, Principal: "a@example.com", Scope: "tool:chatgpt",
			Form: store.FormSubstring, Query: "Q3-contract",
		})
		if err != nil {
			t.Fatalf("substring search: %v", err)
		}
		if len(sub.Hits) != 1 || sub.Hits[0].UnitKind != store.UnitAttachmentName {
			t.Fatalf("a substring search returned %+v; it must return filenames only", sub.Hits)
		}

		// The terms form is whole-word, so a hyphenated filename is the substring form's job
		// (docs/04 §15.2: full-text search is the right tool for prose, and trigrams are the right
		// tool for "find me the file called something like Q3-contract"). A word that appears in the
		// prompt body is found by the terms form, and only in the prompt body.
		terms, err := rig.Service.Search(ctx, vault.SearchRequest{
			TenantID: testrig.TenantID, Principal: "a@example.com", Scope: "tool:chatgpt",
			Form: store.FormTerms, Query: "attached",
		})
		if err != nil {
			t.Fatalf("terms search: %v", err)
		}
		if len(terms.Hits) != 1 || terms.Hits[0].UnitKind != store.UnitPromptBody {
			t.Errorf("a terms search for a prompt word returned %+v; want the prompt body only", terms.Hits)
		}
	})

	t.Run("a search with no hits is still audited", func(t *testing.T) {
		rig := testrig.New(t, testrig.Options{
			Tenants:    []store.Tenant{testrig.Tenant(FullText())},
			ScopeTiers: map[string]store.SearchTier{"tool:chatgpt": store.SearchFullText},
		})
		res, err := rig.Service.Search(ctx, vault.SearchRequest{
			TenantID: testrig.TenantID, Principal: "a@example.com", Scope: "tool:chatgpt",
			Form: store.FormTerms, Query: "nothing-matches-this",
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Hits) != 0 {
			t.Fatalf("expected no hits, got %+v", res.Hits)
		}
		found := false
		for _, row := range rig.Memory.Audit() {
			if row.Action == vault.ActionSearch {
				found = true
				if row.Detail["terms"] == "" {
					t.Error("the audit row does not record the query terms")
				}
			}
		}
		if !found {
			t.Error("a search that returned nothing wrote no audit row; §6.3 requires one")
		}
	})

	t.Run("search terms are sanitised into a closed tsquery", func(t *testing.T) {
		rig := testrig.New(t, testrig.Options{
			Tenants:    []store.Tenant{testrig.Tenant(FullText())},
			ScopeTiers: map[string]store.SearchTier{"tool:chatgpt": store.SearchFullText},
		})
		rig.Store(t, testrig.TenantID, testrig.ObjectA, testrig.SubmissionA, testrig.EventA,
			vault.IndexUnit{UnitKind: store.UnitPromptBody, UnitIndex: 0, Body: "wire transfer to iban"})

		res, err := rig.Service.Search(ctx, vault.SearchRequest{
			TenantID: testrig.TenantID, Principal: "a@example.com", Scope: "tool:chatgpt",
			Form: store.FormTerms, Query: `"wire transfer" AND iban & !;`,
		})
		if err != nil {
			t.Fatalf("search with operator-shaped input: %v", err)
		}
		if len(res.Hits) != 1 {
			t.Fatalf("the phrase search returned %d hits, want 1", len(res.Hits))
		}
		var terms string
		for _, row := range rig.Memory.Audit() {
			if row.Action == vault.ActionSearch {
				terms, _ = row.Detail["terms"].(string)
			}
		}
		if terms == "" {
			t.Fatal("no terms were recorded")
		}
		// The closed transformation: the phrase becomes an adjacency pair, the operator words are
		// dropped, and every tsquery operator the analyst typed is gone.
		if terms != "wire <-> transfer & iban" {
			t.Errorf("sanitised query is %q, want %q", terms, "wire <-> transfer & iban")
		}
		for _, bad := range []string{"!", ";", "drop", "table"} {
			if contains(terms, bad) {
				t.Errorf("the query text %q still contains %q after sanitisation", terms, bad)
			}
		}
	})
}

// ---------------------------------------------------------------------------------------
// Audit-before-serve, and failing closed
// ---------------------------------------------------------------------------------------

// TestAuditIsWrittenBeforeTheObjectIsRead asserts the ordering §11 requires, by recording the call
// sequence rather than by trusting the code's shape.
func TestAuditIsWrittenBeforeTheObjectIsRead(t *testing.T) {
	ctx := context.Background()
	rig := testrig.New(t, testrig.Options{})
	rig.Store(t, testrig.TenantID, testrig.ObjectA, testrig.SubmissionA, testrig.EventA)

	rec := &recordingStore{Memory: rig.Memory}
	svc, err := vault.New(vault.Options{Store: rec, Keys: rig.Keys, Now: rig.Clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Retrieve(ctx, vault.RetrieveRequest{
		TenantID: testrig.TenantID, EventID: testrig.EventA, Principal: "analyst@example.com",
		CaseReference: "CASE-42", SecondApprover: "approver@example.com",
	}); err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	var auditAt, readAt = -1, -1
	for i, call := range rec.calls {
		if call == "AppendAudit" && auditAt < 0 {
			auditAt = i
		}
		if call == "ObjectForEvent" && readAt < 0 {
			readAt = i
		}
	}
	if auditAt < 0 || readAt < 0 {
		t.Fatalf("call sequence did not include both calls: %v", rec.calls)
	}
	if auditAt > readAt {
		t.Fatalf("the object was read before the audit row was written: %v", rec.calls)
	}
}

// TestReadsFailClosedWhenTheAuditCannotBeWritten is §11's "no audit row, no content".
func TestReadsFailClosedWhenTheAuditCannotBeWritten(t *testing.T) {
	ctx := context.Background()
	rig := testrig.New(t, testrig.Options{
		Tenants:    []store.Tenant{testrig.Tenant(FullText())},
		ScopeTiers: map[string]store.SearchTier{"tool:chatgpt": store.SearchFullText},
	})
	rig.Store(t, testrig.TenantID, testrig.ObjectA, testrig.SubmissionA, testrig.EventA)

	broken := &failingAuditStore{Memory: rig.Memory}
	svc, err := vault.New(vault.Options{
		Store: broken, Keys: rig.Keys, Now: rig.Clock.Now,
		ScopeTiers: map[string]store.SearchTier{"tool:chatgpt": store.SearchFullText},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.Retrieve(ctx, vault.RetrieveRequest{
		TenantID: testrig.TenantID, EventID: testrig.EventA, Principal: "analyst@example.com",
		CaseReference: "CASE-42", SecondApprover: "approver@example.com",
	})
	assertDenial(t, err, vault.DenyAuditUnavailable)

	_, err = svc.Search(ctx, vault.SearchRequest{
		TenantID: testrig.TenantID, Principal: "analyst@example.com", Scope: "tool:chatgpt",
		Form: store.FormTerms, Query: "anything",
	})
	if err == nil {
		t.Fatal("a search succeeded while the audit path was failing")
	}
	if d, ok := vault.IsDenial(err); !ok || d.Reason != vault.DenyAuditUnavailable {
		t.Fatalf("a search with a broken audit path returned %v, want audit_unavailable", err)
	}
}

// TestPrepareStoresNothingUntilFinalise is the two-phase shape: a minted key with no stored object.
func TestPrepareStoresNothingUntilFinalise(t *testing.T) {
	ctx := context.Background()
	rig := testrig.New(t, testrig.Options{})
	prep, err := rig.Service.PrepareObject(ctx, vault.PrepareRequest{
		TenantID: testrig.TenantID, ObjectID: testrig.ObjectA, SubmissionID: testrig.SubmissionA,
		EventID: testrig.EventA, RetentionClass: "standard",
	})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if len(prep.DEK) != 32 || len(prep.WrappedDEK) == 0 {
		t.Fatalf("prepare returned a %d-byte key and a %d-byte wrapped form", len(prep.DEK), len(prep.WrappedDEK))
	}
	if _, err := rig.Memory.ContentObject(ctx, testrig.TenantID, testrig.ObjectA); !errors.Is(err, store.ErrObjectNotFound) {
		t.Errorf("prepare wrote an object row: %v", err)
	}
	// The wrapped form cannot be opened as a different object's key: the AAD binds it to its row.
	other := keys.AAD{TenantID: testrig.TenantID, ObjectID: testrig.ObjectB, KEKID: prep.KEKID, KEKVersion: prep.KEKVersion}
	if _, err := rig.Keys.Unwrap(ctx, other, prep.WrappedDEK); !errors.Is(err, keys.ErrAuthentication) {
		t.Errorf("a wrapped key opened under another object's AAD: %v", err)
	}
}

// FullText is a tenant option for a vendor-managed M3 tenant with full-text search.
func FullText() func(*store.Tenant) {
	return func(tn *store.Tenant) {
		tn.ContentSearch = store.SearchFullText
		tn.CeilingMode = "m3"
		tn.KeyCustody = store.CustodyVendor
	}
}

// ---------------------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------------------

func assertDenial(t *testing.T, err error, want vault.DenialReason) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected a refusal with reason %q, got success", want)
	}
	d, ok := vault.IsDenial(err)
	if !ok {
		t.Fatalf("expected a refusal with reason %q, got %v", want, err)
	}
	if d.Reason != want {
		t.Fatalf("refusal reason is %q, want %q (%v)", d.Reason, want, err)
	}
	if !d.Reason.Valid() {
		t.Errorf("refusal reason %q is outside the closed set", d.Reason)
	}
}

func contains(haystack, needle string) bool {
	return bytes.Contains([]byte(haystack), []byte(needle))
}

type recordingStore struct {
	*store.Memory
	mu    sync.Mutex
	calls []string
}

func (r *recordingStore) record(name string) {
	r.mu.Lock()
	r.calls = append(r.calls, name)
	r.mu.Unlock()
}

func (r *recordingStore) AppendAudit(ctx context.Context, e store.AuditEntry) error {
	r.record("AppendAudit")
	return r.Memory.AppendAudit(ctx, e)
}

func (r *recordingStore) ObjectForEvent(ctx context.Context, tenantID, eventID string) (store.ContentObject, error) {
	r.record("ObjectForEvent")
	return r.Memory.ObjectForEvent(ctx, tenantID, eventID)
}

type failingAuditStore struct{ *store.Memory }

func (f *failingAuditStore) AppendAudit(context.Context, store.AuditEntry) error {
	return fmt.Errorf("audit storage is unavailable")
}

func (f *failingAuditStore) SearchAudited(context.Context, store.SearchQuery, store.AuditEntry) ([]store.SearchHit, error) {
	return nil, fmt.Errorf("audit storage is unavailable")
}
