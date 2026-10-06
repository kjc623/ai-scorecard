// Package azureidentity obtains Microsoft Entra access tokens for the user-assigned managed
// identity a container app or job runs as.
//
// Azure Container Apps exposes the identity through a local token endpoint: the platform sets
// IDENTITY_ENDPOINT and IDENTITY_HEADER in every replica, and AZURE_CLIENT_ID names which of the
// app's user-assigned identities to ask for.
package azureidentity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"sync"
	"time"
)

// Resources the services request tokens for.
const (
	ResourcePostgres      = "https://ossrdbms-aad.database.windows.net"
	ResourceStorage       = "https://storage.azure.com"
	ResourceKeyVault      = "https://vault.azure.net"
	ResourceTokenExchange = "api://AzureADTokenExchange"
)

// refreshSkew renews a cached token this long before it expires, so a token is never presented at
// the moment it lapses.
const refreshSkew = 5 * time.Minute

// TokenSource returns an access token for a resource. *Credential implements it; tests substitute
// their own.
type TokenSource interface {
	Token(ctx context.Context, resource string) (string, error)
}

// Credential is one managed identity. It is safe for concurrent use and caches one token per
// resource.
type Credential struct {
	endpoint string
	header   string
	clientID string
	client   *http.Client
	now      func() time.Time

	mu    sync.Mutex
	cache map[string]cachedToken
}

type cachedToken struct {
	value   string
	expires time.Time
}

// FromEnvironment returns the identity the Container Apps platform injected. It fails when the
// process is not running with a managed identity.
func FromEnvironment() (*Credential, error) {
	endpoint := os.Getenv("IDENTITY_ENDPOINT")
	header := os.Getenv("IDENTITY_HEADER")
	if endpoint == "" || header == "" {
		return nil, errors.New("azureidentity: IDENTITY_ENDPOINT and IDENTITY_HEADER are not set; the process has no managed identity")
	}
	return New(endpoint, header, os.Getenv("AZURE_CLIENT_ID"), nil), nil
}

// New returns a credential for the identity endpoint at endpoint. clientID selects a user-assigned
// identity; client defaults to one with a ten-second timeout.
func New(endpoint, header, clientID string, client *http.Client) *Credential {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &Credential{
		endpoint: endpoint,
		header:   header,
		clientID: clientID,
		client:   client,
		now:      time.Now,
		cache:    map[string]cachedToken{},
	}
}

// Token returns an access token for resource, from the cache while it is comfortably valid.
func (c *Credential) Token(ctx context.Context, resource string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if t, ok := c.cache[resource]; ok && c.now().Before(t.expires.Add(-refreshSkew)) {
		return t.value, nil
	}
	t, err := c.fetch(ctx, resource)
	if err != nil {
		return "", err
	}
	c.cache[resource] = t
	return t.value, nil
}

func (c *Credential) fetch(ctx context.Context, resource string) (cachedToken, error) {
	q := url.Values{"api-version": {"2019-08-01"}, "resource": {resource}}
	if c.clientID != "" {
		q.Set("client_id", c.clientID)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint+"?"+q.Encode(), nil)
	if err != nil {
		return cachedToken{}, fmt.Errorf("azureidentity: %w", err)
	}
	req.Header.Set("X-IDENTITY-HEADER", c.header)
	resp, err := c.client.Do(req)
	if err != nil {
		return cachedToken{}, fmt.Errorf("azureidentity: token endpoint unreachable: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return cachedToken{}, fmt.Errorf("azureidentity: reading the token response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return cachedToken{}, fmt.Errorf("azureidentity: token endpoint answered %d for %s", resp.StatusCode, resource)
	}
	var body struct {
		AccessToken string          `json:"access_token"`
		ExpiresOn   json.RawMessage `json:"expires_on"`
	}
	if err := json.Unmarshal(raw, &body); err != nil || body.AccessToken == "" {
		return cachedToken{}, fmt.Errorf("azureidentity: token endpoint returned no token for %s", resource)
	}
	return cachedToken{value: body.AccessToken, expires: expiry(body.ExpiresOn, c.now())}, nil
}

// expiry reads expires_on, which the endpoint sends as Unix seconds in either a string or a
// number. A token whose expiry cannot be read is treated as valid for ten minutes.
func expiry(raw json.RawMessage, now time.Time) time.Time {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		s = string(raw)
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil && n > 0 {
		return time.Unix(n, 0)
	}
	return now.Add(10 * time.Minute)
}
