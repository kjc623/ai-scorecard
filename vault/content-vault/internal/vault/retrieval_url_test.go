package vault_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/shadow-ai-capture/content-vault/internal/testrig"
	"github.com/shadow-ai-capture/content-vault/internal/vault"
	"github.com/shadow-ai-capture/device/protocol"
)

// mapBlobs is the deployment's storage seam, stood in for by a map: the vault still reads through
// Options.Blobs, so the test exercises the reader the binary wires rather than FetchBlob.
type mapBlobs map[string][]byte

func (m mapBlobs) Get(_ context.Context, path string) ([]byte, error) {
	b, ok := m[path]
	if !ok {
		return nil, fmt.Errorf("no blob at %s", path)
	}
	return b, nil
}

// TestRetrievalURLIsMintedAndRedeemedOnce is the whole path: Retrieve returns a URL and no content;
// redeeming the URL serves the object the device sealed; the same URL cannot be redeemed twice.
func TestRetrievalURLIsMintedAndRedeemedOnce(t *testing.T) {
	const principal = "analyst@example.com"
	ctx := context.Background()
	blobs := mapBlobs{}
	rig := testrig.New(t, testrig.Options{GrantTTL: time.Minute, AdjustVault: func(o *vault.Options) { o.Blobs = blobs }})

	// The two-phase store path, done by hand so the recorded digest is the real digest of the bytes
	// the device sealed: the redeemer checks exactly that, and testrig.Store's placeholder digest
	// would exercise the mismatch path instead.
	prep, err := rig.Service.PrepareObject(ctx, vault.PrepareRequest{
		TenantID: testrig.TenantID, ObjectID: testrig.ObjectA, SubmissionID: testrig.SubmissionA,
		EventID: testrig.EventA, ExpiresAt: rig.Clock.T.Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("PrepareObject: %v", err)
	}
	plaintext := []byte("What is the capital of Australia?")
	sealed, err := protocol.SealContent(prep.DEK, testrig.EventA, plaintext)
	if err != nil {
		t.Fatalf("sealing the fixture: %v", err)
	}
	const blobPath = "objects/one"
	blobs[blobPath] = sealed
	if _, err := rig.Service.FinaliseObject(ctx, vault.FinaliseRequest{
		TenantID: testrig.TenantID, ObjectID: testrig.ObjectA, SubmissionID: testrig.SubmissionA,
		EventID: testrig.EventA, BlobPath: blobPath, CiphertextSHA256: protocol.RawDigest(sealed),
		PlaintextSizeBytes: int64(len(plaintext)), WrappedDEK: prep.WrappedDEK,
		KEKID: prep.KEKID, KEKVersion: prep.KEKVersion, ExpiresAt: rig.Clock.T.Add(24 * time.Hour),
	}); err != nil {
		t.Fatalf("FinaliseObject: %v", err)
	}

	res, err := rig.Service.Retrieve(ctx, vault.RetrieveRequest{
		TenantID: testrig.TenantID, EventID: testrig.EventA, Principal: principal,
	})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	wantURL := vault.RetrievalPath + testrig.TenantID + "/" + res.GrantID
	if res.RetrievalURL != wantURL {
		t.Fatalf("RetrievalURL = %q, want %q", res.RetrievalURL, wantURL)
	}
	if strings.Contains(res.RetrievalURL, "capital") {
		t.Fatal("the URL carries content")
	}

	out, err := rig.Service.RedeemURL(ctx, testrig.TenantID, res.GrantID)
	if err != nil {
		t.Fatalf("RedeemURL: %v", err)
	}
	if string(out.Plaintext) != string(plaintext) {
		t.Fatalf("RedeemURL returned %q, want the sealed prompt", out.Plaintext)
	}

	// A replayed URL is refused: the grant was consumed by the first read.
	_, err = rig.Service.RedeemURL(ctx, testrig.TenantID, res.GrantID)
	assertDenial(t, err, vault.DenyGrantAlreadyUsed)
}

// TestRetrievalURLBaseIsUsedWhenConfigured: a deployment behind an ingress mints an absolute URL.
func TestRetrievalURLBaseIsUsedWhenConfigured(t *testing.T) {
	ctx := context.Background()
	rig := testrig.New(t, testrig.Options{AdjustVault: func(o *vault.Options) {
		o.RetrievalURLBase = "https://analyst.example.test/"
	}})
	rig.Store(t, testrig.TenantID, testrig.ObjectA, testrig.SubmissionA, testrig.EventA)
	res, err := rig.Service.Retrieve(ctx, vault.RetrieveRequest{
		TenantID: testrig.TenantID, EventID: testrig.EventA, Principal: "analyst@example.com",
	})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if !strings.HasPrefix(res.RetrievalURL, "https://analyst.example.test"+vault.RetrievalPath) {
		t.Fatalf("RetrievalURL = %q, want the configured absolute base", res.RetrievalURL)
	}
}

// TestRedeemURLExpiredAndUnknown cover the two other matrix rows the URL adds.
func TestRedeemURLExpiredAndUnknown(t *testing.T) {
	const principal = "analyst@example.com"
	ctx := context.Background()
	rig := testrig.New(t, testrig.Options{GrantTTL: time.Minute})
	rig.Store(t, testrig.TenantID, testrig.ObjectA, testrig.SubmissionA, testrig.EventA)
	res, err := rig.Service.Retrieve(ctx, vault.RetrieveRequest{
		TenantID: testrig.TenantID, EventID: testrig.EventA, Principal: principal,
	})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}

	rig.Clock.Advance(time.Minute + time.Second)
	_, err = rig.Service.RedeemURL(ctx, testrig.TenantID, res.GrantID)
	assertDenial(t, err, vault.DenyGrantExpired)

	_, err = rig.Service.RedeemURL(ctx, testrig.TenantID, "99999999-9999-4999-8999-999999999999")
	assertDenial(t, err, vault.DenyGrantRequired)
}

// TestRedeemURLReadsThroughTheBlobReader: the storage read the deployment makes is used, and a
// refused storage read is an error rather than content that is gone.
func TestRedeemURLReadsThroughTheBlobReader(t *testing.T) {
	ctx := context.Background()
	blobs := mapBlobs{}
	rig := testrig.New(t, testrig.Options{AdjustVault: func(o *vault.Options) { o.Blobs = blobs }})
	rig.Store(t, testrig.TenantID, testrig.ObjectA, testrig.SubmissionA, testrig.EventA)
	res, err := rig.Service.Retrieve(ctx, vault.RetrieveRequest{
		TenantID: testrig.TenantID, EventID: testrig.EventA, Principal: "analyst@example.com",
	})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}

	// Nothing was stored under the path, so the read fails; it is not reported as an empty object.
	if _, err := rig.Service.RedeemURL(ctx, testrig.TenantID, res.GrantID); err == nil {
		t.Fatal("a missing stored object was served as content")
	} else if _, ok := vault.IsDenial(err); ok {
		t.Fatalf("a storage failure was dressed as a content refusal: %v", err)
	}
}
