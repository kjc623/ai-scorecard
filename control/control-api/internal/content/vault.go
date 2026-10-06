package content

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/shadow-ai-capture/device/protocol"
)

// Service-token parameters for calls to content-vault.
const (
	VaultAudience  = "sac-vault"
	ServiceSubject = "control-api"
)

// ServiceTokens mints the short-lived service token control-api presents to content-vault.
type ServiceTokens interface {
	ServiceToken(audience, service string) (string, error)
}

// HTTPVault calls content-vault on its internal ingress, authenticated with a service token signed
// by control-api's session signing key.
type HTTPVault struct {
	base   string
	tokens ServiceTokens
	client *http.Client
}

// NewHTTPVault builds the client for a content-vault base URL.
func NewHTTPVault(base string, tokens ServiceTokens) *HTTPVault {
	return &HTTPVault{
		base:   strings.TrimRight(base, "/"),
		tokens: tokens,
		client: &http.Client{Timeout: 60 * time.Second},
	}
}

// Put implements Vault: PUT /internal/v1/tenants/{tenant}/content/{event}. content-vault answers
// 201 when it stores the content and 200 when the same grant and digest were already stored.
func (v *HTTPVault) Put(ctx context.Context, tenantID, eventID, grantID, rawDigest string, body []byte) (bool, error) {
	token, err := v.tokens.ServiceToken(VaultAudience, ServiceSubject)
	if err != nil {
		return false, fmt.Errorf("service token: %w", err)
	}
	endpoint := v.base + "/internal/v1/tenants/" + url.PathEscape(tenantID) + "/content/" + url.PathEscape(eventID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set(protocol.HeaderContentGrantID, grantID)
	req.Header.Set(protocol.HeaderContentRawDigest, rawDigest)
	resp, err := v.client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp.StatusCode == http.StatusCreated, nil
	}
	var e struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &e)
	return false, &VaultError{Status: resp.StatusCode, Code: e.Error.Code}
}
