// Package entraapp is the vendor's multi-tenant Microsoft Entra application as a client: the
// credential it authenticates with, and the app-only tokens it obtains in a customer's tenant
// (contract §3, §5).
//
// One application serves both sign-in (internal/identity redeems authorization codes with it) and the
// one Graph application permission the product asks for, DeviceManagementManagedDevices.Read.All,
// which the enrolment path uses for the Intune check. The customer grants it by admin consent during
// onboarding; nothing here asks for User.Read.All, because people reach the product through SCIM.
//
// Three credentials, exactly one configured:
//
//   - a client secret (SAC_ENTRA_CLIENT_SECRET) — the lab's, because it is a password in an
//     environment variable;
//   - a certificate (SAC_ENTRA_CERT_FILE) — a client-assertion JWT signed with the certificate's
//     key, so no secret crosses the wire;
//   - a managed identity as a federated credential (SAC_ENTRA_FIC=managed) — the platform issues the
//     container's identity a token for api://AzureADTokenExchange, and that token is the assertion.
//     Nothing secret is stored anywhere, which is why it is the production choice.
package entraapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Environment names. The binary reads them through ConfigFromEnv.
const (
	EnvClientID     = "SAC_ENTRA_CLIENT_ID"
	EnvClientSecret = "SAC_ENTRA_CLIENT_SECRET"
	EnvCertFile     = "SAC_ENTRA_CERT_FILE"
	EnvFIC          = "SAC_ENTRA_FIC"
	// EnvMIClientID names a user-assigned managed identity; empty uses AZURE_CLIENT_ID, then the
	// system-assigned identity.
	EnvMIClientID = "SAC_ENTRA_MI_CLIENT_ID"
	// EnvLoginBase overrides the Microsoft identity platform host, for a sovereign cloud or a test.
	EnvLoginBase = "SAC_ENTRA_LOGIN_BASE"
)

const (
	// DefaultLoginBase is the public cloud's identity platform.
	DefaultLoginBase = "https://login.microsoftonline.com"
	// FICAudience is the audience a federated identity credential's assertion must carry.
	FICAudience = "api://AzureADTokenExchange"
	// AssertionType is RFC 7523's client assertion type.
	AssertionType = "urn:ietf:params:oauth:client-assertion-type:jwt-bearer"
	// GraphScope is the app-only scope for Microsoft Graph.
	GraphScope = "https://graph.microsoft.com/.default"
	// DefaultIMDS is the Azure instance metadata token endpoint.
	DefaultIMDS = "http://169.254.169.254/metadata/identity/oauth2/token"
)

// ErrNotConfigured is returned by New when no client id is configured: Entra is then off, which is a
// valid deployment (a tenant on another OIDC provider needs none of this).
var ErrNotConfigured = errors.New("entraapp: no Entra application is configured")

// TokenSource obtains an app-only access token in a customer's Entra tenant.
type TokenSource interface {
	Token(ctx context.Context, customerTenantID, scope string) (string, error)
}

// Config selects and configures the credential.
type Config struct {
	ClientID     string
	ClientSecret string
	CertFile     string
	// FIC is "managed" for a managed identity used as a federated credential; empty otherwise.
	FIC string
	// ManagedIdentityClientID selects a user-assigned identity; empty is the default identity.
	ManagedIdentityClientID string
	LoginBaseURL            string
	// IdentityEndpoint and IdentityHeader are the Container Apps / App Service managed identity
	// endpoint the platform sets (IDENTITY_ENDPOINT, IDENTITY_HEADER). Empty falls back to IMDS.
	IdentityEndpoint string
	IdentityHeader   string
	IMDSURL          string
	HTTPClient       *http.Client
	Now              func() time.Time
}

// ConfigFromEnv reads the SAC_ENTRA_* names and the platform's managed-identity variables.
func ConfigFromEnv(getenv func(string) string) Config {
	mi := strings.TrimSpace(getenv(EnvMIClientID))
	if mi == "" {
		mi = strings.TrimSpace(getenv("AZURE_CLIENT_ID"))
	}
	return Config{
		ClientID:                strings.TrimSpace(getenv(EnvClientID)),
		ClientSecret:            getenv(EnvClientSecret),
		CertFile:                strings.TrimSpace(getenv(EnvCertFile)),
		FIC:                     strings.TrimSpace(getenv(EnvFIC)),
		ManagedIdentityClientID: mi,
		LoginBaseURL:            strings.TrimSpace(getenv(EnvLoginBase)),
		IdentityEndpoint:        strings.TrimSpace(getenv("IDENTITY_ENDPOINT")),
		IdentityHeader:          getenv("IDENTITY_HEADER"),
	}
}

// credential produces the client-authentication parameters for one token request. aud is the token
// endpoint the request goes to, which a client assertion must name.
type credential interface {
	params(ctx context.Context, aud string) (url.Values, error)
	kind() string
}

// App is the configured application.
type App struct {
	clientID  string
	loginBase string
	cred      credential
	client    *http.Client
	now       func() time.Time

	mu    sync.Mutex
	cache map[string]cachedToken
}

type cachedToken struct {
	token   string
	expires time.Time
}

// New builds the application client. Exactly one credential must be configured: two would leave which
// one a deployment uses to chance.
func New(cfg Config) (*App, error) {
	if cfg.ClientID == "" {
		return nil, ErrNotConfigured
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 15 * time.Second}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	base := strings.TrimRight(cfg.LoginBaseURL, "/")
	if base == "" {
		base = DefaultLoginBase
	}
	set := 0
	for _, v := range []string{cfg.ClientSecret, cfg.CertFile, cfg.FIC} {
		if v != "" {
			set++
		}
	}
	if set != 1 {
		return nil, fmt.Errorf("entraapp: configure exactly one credential (%s, %s or %s=managed); %d are set",
			EnvClientSecret, EnvCertFile, EnvFIC, set)
	}
	app := &App{clientID: cfg.ClientID, loginBase: base, client: cfg.HTTPClient, now: cfg.Now, cache: map[string]cachedToken{}}
	switch {
	case cfg.ClientSecret != "":
		app.cred = secretCredential{secret: cfg.ClientSecret}
	case cfg.CertFile != "":
		c, err := loadCertCredential(cfg.CertFile, cfg.ClientID, cfg.Now)
		if err != nil {
			return nil, err
		}
		app.cred = c
	default:
		if cfg.FIC != "managed" {
			return nil, fmt.Errorf("entraapp: %s=%q; the only federated credential this build obtains is \"managed\"", EnvFIC, cfg.FIC)
		}
		imds := cfg.IMDSURL
		if imds == "" {
			imds = DefaultIMDS
		}
		app.cred = &managedCredential{
			endpoint: cfg.IdentityEndpoint, header: cfg.IdentityHeader, imds: imds,
			clientID: cfg.ManagedIdentityClientID, client: cfg.HTTPClient, now: cfg.Now,
		}
	}
	return app, nil
}

// ClientID is the application (client) id, for sign-in and Graph alike.
func (a *App) ClientID() string { return a.clientID }

// Credential names the configured credential kind, for the startup log.
func (a *App) Credential() string { return a.cred.kind() }

// LoginBase is the identity platform host this app talks to.
func (a *App) LoginBase() string { return a.loginBase }

// ClientAuth returns the client-authentication parameters for a request to tokenEndpoint: a secret,
// or a client assertion whose audience is that endpoint. internal/identity uses it to redeem a
// sign-in's authorization code and to refresh.
func (a *App) ClientAuth(ctx context.Context, tokenEndpoint string) (url.Values, error) {
	return a.cred.params(ctx, tokenEndpoint)
}

var tenantRE = regexp.MustCompile(`^[0-9a-zA-Z][0-9a-zA-Z.-]{0,253}$`)

// Token implements TokenSource with the client-credentials grant against the customer's tenant.
// Tokens are cached per tenant and scope until five minutes before they expire.
func (a *App) Token(ctx context.Context, customerTenantID, scope string) (string, error) {
	tid := strings.ToLower(strings.TrimSpace(customerTenantID))
	if !tenantRE.MatchString(tid) || tid == "common" || tid == "organizations" || tid == "consumers" {
		return "", fmt.Errorf("entraapp: %q is not a customer tenant id", customerTenantID)
	}
	if scope == "" {
		return "", errors.New("entraapp: a scope is required")
	}
	key := tid + " " + scope
	a.mu.Lock()
	if c, ok := a.cache[key]; ok && a.now().Before(c.expires) {
		a.mu.Unlock()
		return c.token, nil
	}
	a.mu.Unlock()

	endpoint := a.loginBase + "/" + url.PathEscape(tid) + "/oauth2/v2.0/token"
	form, err := a.cred.params(ctx, endpoint)
	if err != nil {
		return "", err
	}
	form.Set("grant_type", "client_credentials")
	form.Set("client_id", a.clientID)
	form.Set("scope", scope)
	var body struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := postForm(ctx, a.client, endpoint, form, &body); err != nil {
		return "", fmt.Errorf("entraapp: app-only token in tenant %s: %w", tid, err)
	}
	if body.AccessToken == "" {
		return "", fmt.Errorf("entraapp: app-only token in tenant %s: the response carried no token", tid)
	}
	life := time.Duration(body.ExpiresIn) * time.Second
	a.mu.Lock()
	a.cache[key] = cachedToken{token: body.AccessToken, expires: a.now().Add(life - 5*time.Minute)}
	a.mu.Unlock()
	return body.AccessToken, nil
}

type secretCredential struct{ secret string }

func (s secretCredential) params(context.Context, string) (url.Values, error) {
	return url.Values{"client_secret": {s.secret}}, nil
}
func (secretCredential) kind() string { return "client-secret" }

// OAuthError is a token endpoint's refusal. It carries the protocol error code and the AADSTS number,
// never the request, so it is safe to log.
type OAuthError struct {
	Status int
	Code   string
	AADSTS string
}

func (e *OAuthError) Error() string {
	msg := fmt.Sprintf("token endpoint answered %d", e.Status)
	if e.Code != "" {
		msg += " " + e.Code
	}
	if e.AADSTS != "" {
		msg += " (" + e.AADSTS + ")"
	}
	return msg
}

var aadstsRE = regexp.MustCompile(`AADSTS\d+`)

// postForm sends a form to a token endpoint and decodes a 200 into out.
func postForm(ctx context.Context, client *http.Client, endpoint string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("token endpoint unreachable: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("reading the token response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error       string `json:"error"`
			Description string `json:"error_description"`
		}
		_ = json.Unmarshal(raw, &e)
		return &OAuthError{Status: resp.StatusCode, Code: e.Error, AADSTS: aadstsRE.FindString(e.Description)}
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("the token response is not JSON: %w", err)
	}
	return nil
}
