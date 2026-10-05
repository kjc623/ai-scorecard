package identity

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// NewHTTPClient is the client every outbound identity call uses: discovery, keys, the token endpoint.
// A customer types an issuer during onboarding and control-api then fetches from it, so unless the
// deployment allows it (the lab, whose stand-in provider is on a private network) the dialer refuses
// loopback, private, link-local and unspecified addresses at connect time — after DNS, so a name that
// later resolves inward is refused too. No proxy is consulted, for the same reason.
func NewHTTPClient(allowPrivate bool, timeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	if !allowPrivate {
		dialer.Control = func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip := net.ParseIP(host)
			if ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
				ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
				return fmt.Errorf("identity: refusing to connect to non-public address %s", host)
			}
			return nil
		}
	}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:               nil,
			DialContext:         dialer.DialContext,
			TLSHandshakeTimeout: 5 * time.Second,
			MaxIdleConns:        16,
			IdleConnTimeout:     90 * time.Second,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return errors.New("identity: too many redirects")
			}
			return nil
		},
	}
}

// ProviderMetadata is the subset of an OpenID provider's discovery document this service reads.
type ProviderMetadata struct {
	Issuer                string   `json:"issuer"`
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	TokenEndpoint         string   `json:"token_endpoint"`
	JWKSURI               string   `json:"jwks_uri"`
	ResponseTypes         []string `json:"response_types_supported"`
	SigningAlgs           []string `json:"id_token_signing_alg_values_supported"`
	CodeChallengeMethods  []string `json:"code_challenge_methods_supported"`
	TokenAuthMethods      []string `json:"token_endpoint_auth_methods_supported"`
}

// DiscoveryURL is where an issuer's discovery document lives (OpenID Connect Discovery §4).
func DiscoveryURL(issuer string) string {
	return strings.TrimRight(issuer, "/") + "/.well-known/openid-configuration"
}

// getJSON fetches a small JSON document.
func getJSON(ctx context.Context, client *http.Client, address string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("fetch %s: %w", address, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read %s: %w", address, err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %d", address, resp.StatusCode)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%s is not JSON: %w", address, err)
	}
	return nil
}

// checkEndpoint requires an absolute URL with no fragment, over https unless insecure is allowed.
func checkEndpoint(name, raw string, allowInsecure bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Fragment != "" || u.User != nil {
		return fmt.Errorf("%s %q is not an absolute URL", name, raw)
	}
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && allowInsecure:
	default:
		return fmt.Errorf("%s %q must use https", name, raw)
	}
	return nil
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// ValidateIssuer is the onboarding check of a customer-entered OIDC provider: its discovery document
// names exactly this issuer, publishes the endpoints the authorization-code flow needs, does not rule
// out what this service requires (code, RS256, S256), and its JWKS holds at least one RSA signing
// key. A provider that passes may still refuse the client id; the activation sign-in is what proves
// the whole chain.
func ValidateIssuer(ctx context.Context, client *http.Client, issuer string, allowInsecure bool) (ProviderMetadata, error) {
	if err := checkEndpoint("issuer", issuer, allowInsecure); err != nil {
		return ProviderMetadata{}, err
	}
	if u, _ := url.Parse(issuer); u.RawQuery != "" {
		return ProviderMetadata{}, fmt.Errorf("issuer %q must not carry a query", issuer)
	}
	var meta ProviderMetadata
	if err := getJSON(ctx, client, DiscoveryURL(issuer), &meta); err != nil {
		return ProviderMetadata{}, fmt.Errorf("discovery: %w", err)
	}
	if err := checkMetadata(meta, issuer, allowInsecure); err != nil {
		return ProviderMetadata{}, err
	}
	keys, err := fetchJWKS(ctx, client, meta.JWKSURI)
	if err != nil {
		return ProviderMetadata{}, err
	}
	if len(keys) == 0 {
		return ProviderMetadata{}, errors.New("the provider's JWKS holds no RSA signing key")
	}
	return meta, nil
}

func checkMetadata(meta ProviderMetadata, issuer string, allowInsecure bool) error {
	if meta.Issuer != issuer {
		return fmt.Errorf("discovery names issuer %q, not %q; the issuer must match exactly", meta.Issuer, issuer)
	}
	for _, e := range []struct{ name, v string }{
		{"authorization_endpoint", meta.AuthorizationEndpoint},
		{"token_endpoint", meta.TokenEndpoint},
		{"jwks_uri", meta.JWKSURI},
	} {
		if err := checkEndpoint(e.name, e.v, allowInsecure); err != nil {
			return err
		}
	}
	if len(meta.ResponseTypes) > 0 && !contains(meta.ResponseTypes, "code") {
		return errors.New("the provider does not support the authorization-code flow")
	}
	if len(meta.SigningAlgs) > 0 && !contains(meta.SigningAlgs, "RS256") {
		return errors.New("the provider does not sign id_tokens with RS256")
	}
	if len(meta.CodeChallengeMethods) > 0 && !contains(meta.CodeChallengeMethods, "S256") {
		return errors.New("the provider does not support PKCE S256")
	}
	return nil
}

// tokenResponse is the token endpoint's answer for the code and refresh grants.
type tokenResponse struct {
	IDToken      string `json:"id_token"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
}

// oauthError is a token endpoint's refusal: the protocol code only, never the request, so it is safe
// to log (a request holds the code, the verifier and possibly a secret).
type oauthError struct {
	status int
	code   string
}

func (e *oauthError) Error() string {
	return fmt.Sprintf("token endpoint answered %d %s", e.status, e.code)
}

// postToken sends a token request. basic, when set, is client_secret_basic (RFC 6749 §2.3.1 form-
// encodes both halves before base64).
func postToken(ctx context.Context, client *http.Client, endpoint string, form url.Values, basic *[2]string) (tokenResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return tokenResponse{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if basic != nil {
		cred := url.QueryEscape(basic[0]) + ":" + url.QueryEscape(basic[1])
		req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(cred)))
	}
	resp, err := client.Do(req)
	if err != nil {
		return tokenResponse{}, fmt.Errorf("token endpoint unreachable: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return tokenResponse{}, fmt.Errorf("reading the token response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		return tokenResponse{}, &oauthError{status: resp.StatusCode, code: e.Error}
	}
	var out tokenResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return tokenResponse{}, fmt.Errorf("the token response is not JSON: %w", err)
	}
	return out, nil
}
