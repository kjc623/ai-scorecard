package content

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// HTTPVault calls content-vault on its internal ingress. The three identity headers are the ones
// the vault's header authenticator reads; they are trusted there only because the ingress admits
// nothing but control-api and query-api.
type HTTPVault struct {
	base string
	http *http.Client
}

// NewHTTPVault builds the client for a vault base URL such as http://content-vault:8080.
func NewHTTPVault(base string) *HTTPVault {
	return &HTTPVault{base: strings.TrimRight(base, "/"), http: &http.Client{Timeout: 15 * time.Second}}
}

// Prepare implements Vault: POST /v1/content/object.
func (v *HTTPVault) Prepare(ctx context.Context, tenantID, subject, objectID, submissionID, eventID string, expiresAt time.Time, plaintextSize int64) (*PreparedKey, error) {
	req := map[string]any{
		"object_id":                     objectID,
		"event_id":                      eventID,
		"retention_class":               RetentionClass,
		"expected_plaintext_size_bytes": plaintextSize,
	}
	if submissionID != "" {
		req["submission_id"] = submissionID
	}
	if !expiresAt.IsZero() {
		req["expires_at"] = expiresAt.UTC().Format(time.RFC3339Nano)
	}
	var resp struct {
		ObjectKeyB64  string `json:"object_key_b64"`
		WrappedDEKB64 string `json:"wrapped_dek_b64"`
		KEKID         string `json:"kek_id"`
		KEKVersion    string `json:"kek_version"`
	}
	if err := v.call(ctx, tenantID, subject, "/v1/content/object", req, &resp); err != nil {
		return nil, err
	}
	if resp.ObjectKeyB64 == "" || resp.WrappedDEKB64 == "" {
		return nil, fmt.Errorf("vault prepare returned no key")
	}
	return &PreparedKey{ObjectKeyB64: resp.ObjectKeyB64, WrappedDEK: resp.WrappedDEKB64, KEKID: resp.KEKID, KEKVersion: resp.KEKVersion}, nil
}

// Finalise implements Vault: POST /v1/content/object/finalise.
func (v *HTTPVault) Finalise(ctx context.Context, tenantID, subject string, obj StoredObject) error {
	req := map[string]any{
		"object_id":            obj.ObjectID,
		"event_id":             obj.EventID,
		"blob_path":            obj.BlobPath,
		"ciphertext_sha256":    obj.CiphertextSHA256,
		"plaintext_size_bytes": obj.PlaintextSizeBytes,
		"wrapped_dek_b64":      obj.WrappedDEK,
		"kek_id":               obj.KEKID,
		"kek_version":          obj.KEKVersion,
		"retention_class":      RetentionClass,
	}
	if obj.SubmissionID != "" {
		req["submission_id"] = obj.SubmissionID
	}
	if !obj.ExpiresAt.IsZero() {
		req["expires_at"] = obj.ExpiresAt.UTC().Format(time.RFC3339Nano)
	}
	return v.call(ctx, tenantID, subject, "/v1/content/object/finalise", req, nil)
}

func (v *HTTPVault) call(ctx context.Context, tenantID, subject, path string, body any, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, v.base+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Sac-Service", "control-api")
	req.Header.Set("X-Sac-Subject", subject)
	req.Header.Set("X-Sac-Tenant", tenantID)
	resp, err := v.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("vault %s: status %d: %s", path, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(b, out)
}
