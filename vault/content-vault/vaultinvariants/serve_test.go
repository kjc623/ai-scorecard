package vaultinvariants

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/shadow-ai-capture/content-vault/internal/keys"
	"github.com/shadow-ai-capture/content-vault/internal/store"
	"github.com/shadow-ai-capture/content-vault/internal/vault"
	"github.com/shadow-ai-capture/device/protocol"
)

// With a blob store wired, a granted redemption returns the content the device sealed — opened
// with the key the vault unwrapped — and refuses an object that is not the bytes that were
// finalised. Ciphertext is never served under the name of content.
func TestRedeemOpensTheStoredObject(t *testing.T) {
	plaintext := []byte("the prompt as the person typed it")
	blobs := map[string][]byte{}

	mem := store.NewMemory()
	mem.PutTenant(store.Tenant{
		TenantID: tenantID, Name: "integration tenant", Status: "active", KeyCustody: store.CustodyVendor,
		KEKID: "kek-integration", CeilingMode: protocol.ModeM3, ContentSearch: store.SearchDisabled,
		IngestEnabled: true, ReadEnabled: true,
	})
	svc, err := vault.New(vault.Options{
		Store: mem, Keys: keys.NewLocal(), Now: time.Now, RetrievalGrantTTL: 5 * time.Minute,
		FetchBlob: func(path string) ([]byte, error) { return blobs[path], nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	prep, err := svc.PrepareObject(ctx, vault.PrepareRequest{
		TenantID: tenantID, ObjectID: objectID, EventID: eventID, RetentionClass: "content",
		ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := protocol.SealContent(prep.DEK, eventID, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	blobs["objects/one"] = sealed
	if _, err := svc.FinaliseObject(ctx, vault.FinaliseRequest{
		TenantID: tenantID, ObjectID: objectID, SubmissionID: "50000000-0000-7000-8000-000000000001", EventID: eventID,
		BlobPath: "objects/one", CiphertextSHA256: protocol.RawDigest(sealed), PlaintextSizeBytes: int64(len(plaintext)),
		WrappedDEK: prep.WrappedDEK, KEKID: prep.KEKID, KEKVersion: prep.KEKVersion,
	}); err != nil {
		t.Fatal(err)
	}

	redeem := func() (vault.RedeemResult, error) {
		res, err := svc.Retrieve(ctx, vault.RetrieveRequest{
			TenantID: tenantID, EventID: eventID, Principal: analyst,
			CaseReference: caseRef, SecondApprover: other, Justification: "test",
		})
		if err != nil {
			t.Fatalf("Retrieve: %v", err)
		}
		return svc.Redeem(ctx, vault.RedeemRequest{TenantID: tenantID, GrantID: res.GrantID, Principal: analyst, EventID: eventID})
	}

	out, err := redeem()
	if err != nil {
		t.Fatalf("Redeem: %v", err)
	}
	if !bytes.Equal(out.Plaintext, plaintext) {
		t.Fatalf("redeem returned %q, want the content the device sealed", out.Plaintext)
	}

	blobs["objects/one"] = append([]byte(nil), sealed[:len(sealed)-1]...)
	if out, err := redeem(); err == nil || !strings.Contains(err.Error(), "not the bytes that were finalised") {
		t.Fatalf("a substituted object was served: %q, %v", out.Plaintext, err)
	}
}
