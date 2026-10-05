package entraapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"
)

// managedCredential presents the container's managed identity as a federated identity credential:
// the platform issues the identity a token for api://AzureADTokenExchange, and the app registration
// trusts that identity's issuer and subject, so the token is a client assertion. The app holds no
// secret and no key at all.
type managedCredential struct {
	endpoint string // IDENTITY_ENDPOINT (Container Apps, App Service); empty uses IMDS
	header   string // IDENTITY_HEADER
	imds     string
	clientID string // a user-assigned identity's client id, or empty
	client   *http.Client
	now      func() time.Time

	mu      sync.Mutex
	token   string
	expires time.Time
}

func (m *managedCredential) kind() string { return "managed-identity-fic" }

func (m *managedCredential) params(ctx context.Context, _ string) (url.Values, error) {
	tok, err := m.assertion(ctx)
	if err != nil {
		return nil, err
	}
	return url.Values{"client_assertion_type": {AssertionType}, "client_assertion": {tok}}, nil
}

// assertion returns a cached identity token, refreshed five minutes before it expires.
func (m *managedCredential) assertion(ctx context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.token != "" && m.now().Before(m.expires.Add(-5*time.Minute)) {
		return m.token, nil
	}
	var req *http.Request
	var err error
	q := url.Values{"resource": {FICAudience}}
	if m.clientID != "" {
		q.Set("client_id", m.clientID)
	}
	if m.endpoint != "" {
		q.Set("api-version", "2019-08-01")
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, m.endpoint+"?"+q.Encode(), nil)
		if err == nil {
			req.Header.Set("X-IDENTITY-HEADER", m.header)
		}
	} else {
		q.Set("api-version", "2018-02-01")
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, m.imds+"?"+q.Encode(), nil)
		if err == nil {
			req.Header.Set("Metadata", "true")
		}
	}
	if err != nil {
		return "", fmt.Errorf("entraapp: managed identity request: %w", err)
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("entraapp: managed identity endpoint unreachable: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("entraapp: managed identity endpoint answered %d", resp.StatusCode)
	}
	var body struct {
		AccessToken string          `json:"access_token"`
		ExpiresOn   json.RawMessage `json:"expires_on"`
	}
	if err := json.Unmarshal(raw, &body); err != nil || body.AccessToken == "" {
		return "", errors.New("entraapp: managed identity endpoint returned no token")
	}
	m.token = body.AccessToken
	m.expires = m.now().Add(30 * time.Minute)
	// expires_on is unix seconds, as a string (IMDS, App Service) or a number.
	var s string
	if json.Unmarshal(body.ExpiresOn, &s) != nil {
		s = string(body.ExpiresOn)
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil && n > 0 {
		m.expires = time.Unix(n, 0)
	}
	return m.token, nil
}
