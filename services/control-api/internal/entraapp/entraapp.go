// Package entraapp is the vendor's multi-tenant Microsoft Entra application as a client: the
// credential it authenticates with, and the app-only tokens it obtains in a customer's tenant.
//
// One application serves sign-in (internal/identity redeems authorization codes with it) and the
// Graph application permissions the product asks for: DeviceManagementManagedDevices.Read.All for
// the enrolment path's Intune check, and User.Read.All and GroupMember.Read.All for the directory
// pull (internal/graphsync). The customer grants them by admin consent during onboarding.
//
// Two credentials, exactly one configured:
//
//   - the process's managed identity as a federated identity credential (FIC "managed"): the platform
//     issues the identity a token for api://AzureADTokenExchange, and that token is the client
//     assertion. Nothing secret is stored anywhere; this is the production credential.
//   - a client secret, for a local lab pointed at a real Entra tenant.
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

	"github.com/shadow-ai-capture/platform/azureidentity"
)

const (
	// DefaultLoginBase is the public cloud's identity platform.
	DefaultLoginBase = "https://login.microsoftonline.com"
	// AssertionType is RFC 7523's client assertion type.
	AssertionType = "urn:ietf:params:oauth:client-assertion-type:jwt-bearer"
	// GraphScope is the app-only scope for Microsoft Graph.
	GraphScope = "https://graph.microsoft.com/.default"
	// FICManaged selects the managed identity as the federated credential.
	FICManaged = "managed"
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
	// FIC is FICManaged to authenticate with the process's managed identity; empty otherwise.
	FIC string
	// Identity issues the managed identity's tokens. Nil uses azureidentity.FromEnvironment.
	Identity azureidentity.TokenSource
	// LoginBaseURL is the identity platform host; empty is DefaultLoginBase. Tests point it at a fake.
	LoginBaseURL string
	HTTPClient   *http.Client
	Now          func() time.Time
}

// credential produces the client-authentication parameters for one token request.
type credential interface {
	params(ctx context.Context) (url.Values, error)
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
	if (cfg.ClientSecret != "") == (cfg.FIC != "") {
		return nil, errors.New("entraapp: configure exactly one credential: a client secret or the managed identity (FIC managed)")
	}
	app := &App{clientID: cfg.ClientID, loginBase: base, client: cfg.HTTPClient, now: cfg.Now, cache: map[string]cachedToken{}}
	switch {
	case cfg.ClientSecret != "":
		app.cred = secretCredential{secret: cfg.ClientSecret}
	case cfg.FIC != FICManaged:
		return nil, fmt.Errorf("entraapp: federated credential %q is not %q", cfg.FIC, FICManaged)
	default:
		identity := cfg.Identity
		if identity == nil {
			cred, err := azureidentity.FromEnvironment()
			if err != nil {
				return nil, fmt.Errorf("entraapp: %w", err)
			}
			identity = cred
		}
		app.cred = managedCredential{identity: identity}
	}
	return app, nil
}

// ClientID is the application (client) id, for sign-in and Graph alike.
func (a *App) ClientID() string { return a.clientID }

// Credential names the configured credential kind, for the startup log.
func (a *App) Credential() string { return a.cred.kind() }

// LoginBase is the identity platform host this app talks to.
func (a *App) LoginBase() string { return a.loginBase }

// ClientAuth returns the client-authentication parameters for a request to a token endpoint: the
// secret, or the managed identity's client assertion. internal/identity uses it to redeem a
// sign-in's authorization code and to refresh.
func (a *App) ClientAuth(ctx context.Context, _ string) (url.Values, error) {
	return a.cred.params(ctx)
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
	form, err := a.cred.params(ctx)
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

func (s secretCredential) params(context.Context) (url.Values, error) {
	return url.Values{"client_secret": {s.secret}}, nil
}
func (secretCredential) kind() string { return "client-secret" }

// managedCredential presents the managed identity's token for api://AzureADTokenExchange as the
// client assertion. The app registration trusts that identity as a federated credential, so the app
// holds no secret and no key at all. The token source caches the token.
type managedCredential struct{ identity azureidentity.TokenSource }

func (m managedCredential) params(ctx context.Context) (url.Values, error) {
	tok, err := m.identity.Token(ctx, azureidentity.ResourceTokenExchange)
	if err != nil {
		return nil, fmt.Errorf("entraapp: managed identity: %w", err)
	}
	return url.Values{"client_assertion_type": {AssertionType}, "client_assertion": {tok}}, nil
}
func (managedCredential) kind() string { return "managed-identity-fic" }

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
