package httpapi_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/shadow-ai-capture/content-vault/internal/testrig"
	"github.com/shadow-ai-capture/content-vault/internal/vault"
	"github.com/shadow-ai-capture/device/protocol"
)

// mapBlobs is the storage seam the HTTP test wires, so the redemption reads through Options.Blobs
// rather than the tests' FetchBlob.
type mapBlobs map[string][]byte

func (m mapBlobs) Get(_ context.Context, path string) ([]byte, error) {
	b, ok := m[path]
	if !ok {
		return nil, fmt.Errorf("no blob at %s", path)
	}
	return b, nil
}

// TestRetrievalURLOverHTTP is the browser's half of §11 at the surface: a retrieval returns a URL
// and no content, the URL serves the sealed bytes without any caller identity, and a replayed URL
// is refused.
func TestRetrievalURLOverHTTP(t *testing.T) {
	blobs := mapBlobs{}
	ts, rig := server(t, testrig.Options{AdjustVault: func(o *vault.Options) { o.Blobs = blobs }})
	ctx := context.Background()

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
		t.Fatalf("sealing: %v", err)
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

	status, body := post(t, ts, "/v1/content/retrieval", `{
		"event_id": "`+testrig.EventA+`", "case_reference": "CASE-1",
		"second_approver": "approver@example.com", "justification": "investigation"}`)
	if status != http.StatusOK {
		t.Fatalf("retrieval returned %d (%v)", status, body)
	}
	url, _ := body["retrieval_url"].(string)
	if url == "" {
		t.Fatalf("retrieval returned no retrieval_url: %v", body)
	}
	if _, sent := body["content"]; sent {
		t.Fatal("the retrieval response carried content")
	}

	// The browser fetches the URL itself, with no service headers: the grant is the credential.
	resp, err := http.Get(ts.URL + url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s returned %d (%s)", url, resp.StatusCode, got)
	}
	if string(got) != string(plaintext) {
		t.Fatalf("GET %s returned %q, want the sealed prompt", url, got)
	}

	// A replayed URL cannot serve twice.
	again, err := http.Get(ts.URL + url)
	if err != nil {
		t.Fatalf("replayed GET: %v", err)
	}
	again.Body.Close()
	if again.StatusCode != http.StatusForbidden {
		t.Fatalf("a replayed retrieval URL returned %d, want 403", again.StatusCode)
	}
}

// TestRetrievalURLNamesOnlyOpaqueIdentifiers: a malformed URL is a transport refusal, never a store
// lookup with an unvalidated tenant.
func TestRetrievalURLNamesOnlyOpaqueIdentifiers(t *testing.T) {
	ts, _ := server(t, testrig.Options{})
	resp, err := http.Get(ts.URL + "/v1/content/retrieval/not-a-tenant/not-a-grant")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a malformed retrieval URL returned %d, want 400", resp.StatusCode)
	}
}
