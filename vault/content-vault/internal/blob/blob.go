// Package blob is the vault's read side of ciphertext storage.
//
// docs/02 §11 has the vault unwrap an object's key and serve the plaintext; the ciphertext itself
// lives in a storage account the vault must present a credential to reach. In a deployment that
// credential is the container app's user-assigned managed identity, exchanged for a
// storage-scoped access token (docs/06 §4.4, §5.2). The lab has no identity provider, so it
// configures a static bearer the storage stand-in checks — the same stand-in shape as
// control-api's upload signing key.
//
// The package deliberately carries no Azure SDK: the vault builds offline with the standard
// library only (ADR 0016). What it does carry is the two things that make the read "with its own
// identity" rather than anonymous: a credential that authorizes the request, and a reader that
// refuses to treat an uncredentialed or refused read as content.
package blob

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Errors callers branch on. A storage failure that is none of these is infrastructure and
// retryable, and the vault reports it as an error rather than as content that is gone.
var (
	// ErrNoEndpoint is returned when no storage endpoint is configured: the read cannot be made,
	// which is different from the object being absent.
	ErrNoEndpoint = errors.New("blob: no storage endpoint is configured")
	// ErrUnauthorized means the storage layer refused this reader's credential (HTTP 401/403).
	ErrUnauthorized = errors.New("blob: the storage credential was refused")
	// ErrNotFound means the object does not exist (HTTP 404).
	ErrNotFound = errors.New("blob: the object does not exist")
)

// maxResponseBytes bounds one stored object, matching the vault's own ceiling.
const maxResponseBytes = 64<<20 + 1024

// Credential authorizes one outgoing storage request. It is an interface rather than a token so the
// deployment's managed identity and the lab's static bearer are the same seam, and so a credential
// that must fetch and refresh a token can do so inside Authorize.
type Credential interface {
	Authorize(ctx context.Context, req *http.Request) error
}

// StaticBearer is a fixed token. It is the lab's stand-in for a managed identity and the shape a
// deployment test can use; it must never carry a production secret, because a secret in an
// environment variable is what managed identity exists to avoid (docs/06 §5.4).
type StaticBearer struct{ Token string }

// Authorize implements Credential.
func (s StaticBearer) Authorize(_ context.Context, req *http.Request) error {
	if strings.TrimSpace(s.Token) == "" {
		return errors.New("blob: the static credential has no token")
	}
	req.Header.Set("Authorization", "Bearer "+s.Token)
	return nil
}

// ManagedIdentity exchanges the process's Azure managed identity for a storage access token, so the
// vault reads blobs as itself and no storage key is stored anywhere. It is the deployment's
// credential; the IMDS endpoint it calls is the platform's, and the lab exercises the token fetch
// against a stand-in server rather than reaching 169.254.169.254.
type ManagedIdentity struct {
	// Endpoint is the IMDS token endpoint. Empty means the Azure instance-metadata default.
	Endpoint string
	// Resource is the AAD resource the token is for. Empty means Azure Storage.
	Resource string
	// ClientID selects a user-assigned identity. Empty means the system-assigned one.
	ClientID string
	// Client is the HTTP client. Empty means http.DefaultClient.
	Client *http.Client
	// Now is the clock, injectable so expiry and refresh are tested without sleeping.
	Now func() time.Time

	mu     sync.Mutex
	token  string
	expiry time.Time
}

const (
	defaultIMDSEndpoint = "http://169.254.169.254/metadata/identity/oauth2/token"
	defaultStorageScope = "https://storage.azure.com/"
	imdsAPIVersion      = "2018-02-01"
	// refreshSkew refetches a token this long before it expires, so a token is never presented at
	// the moment it lapses.
	refreshSkew = 5 * time.Minute
)

// Authorize implements Credential.
func (m *ManagedIdentity) Authorize(ctx context.Context, req *http.Request) error {
	token, err := m.tokenFor(ctx)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return nil
}

func (m *ManagedIdentity) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

// tokenFor returns a cached token when it is still comfortably valid, and fetches one otherwise.
func (m *ManagedIdentity) tokenFor(ctx context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.token != "" && m.now().Before(m.expiry.Add(-refreshSkew)) {
		return m.token, nil
	}
	token, expiry, err := m.fetch(ctx)
	if err != nil {
		return "", err
	}
	m.token, m.expiry = token, expiry
	return token, nil
}

// fetch asks IMDS for one access token.
func (m *ManagedIdentity) fetch(ctx context.Context) (string, time.Time, error) {
	endpoint := m.Endpoint
	if endpoint == "" {
		endpoint = defaultIMDSEndpoint
	}
	resource := m.Resource
	if resource == "" {
		resource = defaultStorageScope
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("blob: the managed-identity endpoint %q is not a URL: %w", endpoint, err)
	}
	q := u.Query()
	q.Set("api-version", imdsAPIVersion)
	q.Set("resource", resource)
	if m.ClientID != "" {
		q.Set("client_id", m.ClientID)
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", time.Time{}, err
	}
	// The header is what tells the instance metadata service the request came from the VM rather
	// than from a redirect an application followed.
	req.Header.Set("Metadata", "true")
	req.Header.Set("Accept", "application/json")

	client := m.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("blob: asking the managed identity for a token: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("blob: reading the managed-identity response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", time.Time{}, fmt.Errorf("blob: the managed identity answered %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresOn   string `json:"expires_on"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", time.Time{}, fmt.Errorf("blob: the managed-identity response is not JSON: %w", err)
	}
	if out.AccessToken == "" {
		return "", time.Time{}, errors.New("blob: the managed identity returned no access token")
	}
	expiry := m.now().Add(time.Hour)
	if secs, err := strconv.ParseInt(out.ExpiresOn, 10, 64); err == nil {
		expiry = time.Unix(secs, 0)
	}
	return out.AccessToken, expiry, nil
}

// HTTPReader reads one object at a time from a blob endpoint, presenting a credential. It is the
// only reader the product wires; FetchBlob in the vault is the tests' offline seam.
type HTTPReader struct {
	// Endpoint is the blob base URL, with no trailing slash.
	Endpoint string
	// Credential authorizes every read. A nil credential is an anonymous read, which only a lab
	// stand-in accepts; a deployment must set one.
	Credential Credential
	// Client is the HTTP client. Empty means a 30-second default.
	Client *http.Client
	// MaxBytes bounds one response; zero means maxResponseBytes.
	MaxBytes int64
	// APIVersion is the storage REST version. Empty means a recent Azure Blob version; the lab
	// stand-in ignores it.
	APIVersion string
}

const defaultBlobAPIVersion = "2021-08-06"

// Get reads one object. A refusal or an absence is an error, never empty bytes presented as
// content: the vault must not open an empty object and report a prompt that is not there.
func (r *HTTPReader) Get(ctx context.Context, blobPath string) ([]byte, error) {
	if strings.TrimSpace(r.Endpoint) == "" {
		return nil, ErrNoEndpoint
	}
	u := strings.TrimRight(r.Endpoint, "/") + "/" + strings.TrimLeft(blobPath, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	version := r.APIVersion
	if version == "" {
		version = defaultBlobAPIVersion
	}
	req.Header.Set("x-ms-version", version)
	if r.Credential != nil {
		if err := r.Credential.Authorize(ctx, req); err != nil {
			return nil, err
		}
	}
	client := r.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("blob: reading %s: %w", blobPath, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, fmt.Errorf("%w: %s answered %d", ErrUnauthorized, blobPath, resp.StatusCode)
	case http.StatusNotFound:
		return nil, fmt.Errorf("%w: %s", ErrNotFound, blobPath)
	default:
		return nil, fmt.Errorf("blob: %s answered %d", blobPath, resp.StatusCode)
	}
	limit := r.MaxBytes
	if limit <= 0 {
		limit = maxResponseBytes
	}
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}
