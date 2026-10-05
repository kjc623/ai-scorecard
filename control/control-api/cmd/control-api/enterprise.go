package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/shadow-ai-capture/control-api/internal/deploy"
	"github.com/shadow-ai-capture/control-api/internal/directory"
	"github.com/shadow-ai-capture/control-api/internal/enrol"
	"github.com/shadow-ai-capture/control-api/internal/entraapp"
	"github.com/shadow-ai-capture/control-api/internal/identity"
	"github.com/shadow-ai-capture/control-api/internal/intune"
	"github.com/shadow-ai-capture/control-api/internal/onboard"
	"github.com/shadow-ai-capture/control-api/internal/policyserve"
	"github.com/shadow-ai-capture/control-api/internal/scim"
	"github.com/shadow-ai-capture/control-api/internal/session"
	"github.com/shadow-ai-capture/control-api/internal/store"
)

// enterprise is everything a customer deployment needs beyond the device protocol: the identity
// service (sign-in, sessions, product tokens, onboarding), SCIM, deployment keys with the Intune
// check, policy delivery and the admin API. Each part is built only when its settings are present,
// and every part that is off says so in one log line, because a deployment that half-runs one of
// these silently is worse than one that refuses it loudly.
type enterprise struct {
	// For enrol.Config. Interface-typed and left nil when off: a typed nil in an interface would
	// read as "configured" to the enrolment service.
	deployment  store.DeploymentStore
	intune      intune.Checker
	userRefKeys enrol.UserRefKeys
	policyETag  enrol.PolicyETagSource
	keyRate     float64
	keyBurst    int

	// For the HTTP server.
	policy *policyserve.Service
	admin  *deploy.Handler
	scim   http.Handler
	// extra is mounted beside the device routes: /internal/v1/auth/*, /onboard/*, /.well-known/*.
	extra map[string]http.Handler
}

// wireEnterprise builds the enterprise surface over the store and, when there is one, the database.
// Identity and SCIM need the database (sessions, connections and provisioned people persist);
// deployment keys and policy work over either store.
func wireEnterprise(st store.Store, db *sql.DB, logger *slog.Logger) (*enterprise, error) {
	e := &enterprise{extra: map[string]http.Handler{}}
	getenv := os.Getenv
	publicURL := strings.TrimRight(getenv(EnvPublicURL), "/")

	deployStore, ok := st.(store.DeploymentStore)
	if !ok {
		return nil, errors.New("the store does not implement the deployment tables")
	}
	policyStore, ok := st.(store.PolicyStore)
	if !ok {
		return nil, errors.New("the store does not implement the policy tables")
	}
	e.deployment = deployStore
	var err error
	if e.keyRate, err = envFloat(EnvDeploymentKeyRate, 0); err != nil {
		return nil, err
	}
	if e.keyBurst, err = envInt(EnvDeploymentKeyBurst, 0); err != nil {
		return nil, err
	}

	// The directory key seals every *_enc column; SCIM, the user-reference keys and identity's
	// sealed secrets all need it.
	var cipher *directory.Cipher
	if raw := getenv(EnvDirectoryKey); raw != "" {
		k, err := directory.DecodeKey(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", EnvDirectoryKey, err)
		}
		if cipher, err = directory.NewCipher(k); err != nil {
			return nil, fmt.Errorf("%s: %w", EnvDirectoryKey, err)
		}
	}

	// User-reference keys and SCIM.
	var scimAdmin deploy.ScimTokens
	if cipher == nil {
		logger.Warn("SCIM and user-reference keys are off: no directory key", "env", EnvDirectoryKey)
	} else {
		var keyStore directory.UserRefKeyStore = directory.NewMemoryKeyStore()
		var scimStore scim.Store = scim.NewMemory()
		if db != nil {
			keyStore, scimStore = directory.NewSQL(db), scim.NewSQL(db)
		}
		refKeys, err := directory.NewUserRefKeys(keyStore, cipher)
		if err != nil {
			return nil, err
		}
		e.userRefKeys = refKeys
		cfg := scim.Config{PopulationAttribute: getenv(EnvSCIMPopulationAttribute)}
		if publicURL != "" {
			cfg.BaseURL = publicURL + "/scim/v2"
		}
		svc, err := scim.NewService(scimStore, refKeys, cipher, cfg)
		if err != nil {
			return nil, err
		}
		svc.Logger = logger
		e.scim = scim.NewHandler(svc, "/scim/v2", logger)
		scimAdmin = scimTokenAdmin{scim.NewTokens(scimStore)}
	}

	// Policy delivery.
	if path := getenv(EnvPolicySigningKeyFile); path == "" {
		logger.Warn("policy delivery is off: GET /v1/policy answers 503", "env", EnvPolicySigningKeyFile)
	} else {
		key, err := policyserve.LoadSigningKey(path)
		if err != nil {
			return nil, err
		}
		recheck, err := envDuration(EnvPolicyRecheck, 0)
		if err != nil {
			return nil, err
		}
		svc, err := policyserve.New(policyStore, key, policyserve.Config{
			KeyID:           getenv(EnvPolicySigningKeyID),
			RecheckInterval: recheck,
			Logger:          logger,
		})
		if err != nil {
			return nil, err
		}
		e.policy, e.policyETag = svc, svc
		// The public half is the trust anchor the generic MSI must pin; logging it lets an operator
		// compare it with release.json's without reading a key file.
		logger.Info("policy delivery enabled", "kid", svc.KeyID(), "public_key_hex", svc.PublicKeyHex())
	}

	// The vendor's Entra app: sign-in for Entra tenants, the consent probe, and the Intune check.
	app, err := entraapp.New(entraapp.ConfigFromEnv(getenv))
	switch {
	case errors.Is(err, entraapp.ErrNotConfigured):
		app = nil
		logger.Info("Entra is off: Entra sign-in, consent and the Intune check are unavailable", "env", EnvEntraClientID)
	case err != nil:
		return nil, err
	default:
		gc, err := intune.NewGraphChecker(app, nil)
		if err != nil {
			return nil, err
		}
		if u := getenv(EnvGraphURL); u != "" {
			gc.BaseURL = u
		}
		e.intune = gc
		logger.Info("Entra app configured", "client_id", app.ClientID(), "credential", app.Credential())
	}

	// The identity service and everything that authenticates people with it.
	iss := getenv(EnvAuthIssuer)
	if iss == "" {
		logger.Warn("the identity service is off: no sign-in, no onboarding, no admin API", "env", EnvAuthIssuer)
		return e, nil
	}
	if db == nil {
		return nil, fmt.Errorf("%s needs -store sql: sessions, connections and invites persist", EnvAuthIssuer)
	}
	if cipher == nil {
		return nil, fmt.Errorf("%s needs %s: identity seals PKCE verifiers, client secrets and refresh tokens", EnvAuthIssuer, EnvDirectoryKey)
	}
	if publicURL == "" {
		return nil, fmt.Errorf("%s needs %s: redirect URIs and onboarding links are built from it", EnvAuthIssuer, EnvPublicURL)
	}
	keys, err := session.LoadKeyFile(getenv(EnvSessionSigningKeyFile))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", EnvSessionSigningKeyFile, err)
	}
	ttl, err := envDuration(EnvAuthTokenTTL, 0)
	if err != nil {
		return nil, err
	}
	issuer, err := session.NewIssuer(keys, session.IssuerConfig{Issuer: iss, TTL: ttl})
	if err != nil {
		return nil, err
	}
	mgr, err := session.NewManager(session.NewSQL(db), session.ManagerConfig{})
	if err != nil {
		return nil, err
	}
	insecure := getenv(EnvAuthAllowInsecureIdP) == "1"
	if insecure {
		logger.Warn("http issuers and private addresses are allowed for identity providers: a lab setting", "env", EnvAuthAllowInsecureIdP)
	}
	var entraID *identity.EntraConfig
	var entraOB *onboard.EntraConfig
	if app != nil {
		entraID = &identity.EntraConfig{ClientID: app.ClientID(), Auth: app, LoginBaseURL: app.LoginBase()}
		entraOB = &onboard.EntraConfig{ClientID: app.ClientID(), LoginBaseURL: app.LoginBase(),
			ConsentProbe: func(ctx context.Context, tid string) error {
				_, err := app.Token(ctx, tid, entraapp.GraphScope)
				return err
			}}
	}
	idStore := identity.NewSQL(db)
	idSvc, err := identity.New(identity.Config{
		Store: idStore, Sessions: mgr, Issuer: issuer, Cipher: cipher, Entra: entraID,
		AllowInsecureIdP: insecure, AllowedRedirectURIs: splitList(getenv(EnvAuthRedirectURIs)), Logger: logger,
	})
	if err != nil {
		return nil, err
	}
	internalH, err := idSvc.InternalHandler(getenv(EnvInternalToken))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", EnvInternalToken, err)
	}
	ob, err := onboard.New(onboard.Config{
		Store: idStore, Cipher: cipher, PublicURL: publicURL, Entra: entraOB,
		AllowInsecureIssuers: insecure, Logger: logger,
	})
	if err != nil {
		return nil, err
	}
	e.extra["/internal/v1/auth/"] = internalH
	e.extra["/onboard/"] = ob.Handler()
	e.extra["/.well-known/"] = issuer.WellKnownHandler()

	// The admin API authenticates with the product token, audience sac-control, and requires
	// admin; the principal it audits comes from the verified token, never from a header.
	verifier := session.NewVerifier(issuer)
	adminAuth := func(r *http.Request) (deploy.Principal, error) {
		h := r.Header.Get("Authorization")
		if len(h) < 7 || !strings.EqualFold(h[:7], "bearer ") {
			return deploy.Principal{}, errors.New("no bearer token")
		}
		p, err := verifier.Verify(strings.TrimSpace(h[7:]), session.AudienceControl)
		if err != nil {
			return deploy.Principal{}, err
		}
		return deploy.Principal{Tenant: p.Tenant, Actor: p.Actor, Subject: p.Subject, Roles: p.Roles}, nil
	}
	keyTTL, err := envDuration(EnvDeploymentKeyTTL, 0)
	if err != nil {
		return nil, err
	}
	scimBase := ""
	if publicURL != "" {
		scimBase = publicURL + "/scim/v2"
	}
	if e.admin, err = deploy.NewHandler(deployStore, adminAuth, scimAdmin, deploy.Config{
		ReleaseDir:     getenv(EnvAgentReleaseDir),
		DeviceEndpoint: getenv(EnvPublicDeviceEndpoint),
		ScimBaseURL:    scimBase,
		KeyTTL:         keyTTL,
		Logger:         logger,
	}); err != nil {
		return nil, err
	}
	logger.Info("identity service enabled", "issuer", issuer.Issuer(), "entra", app != nil, "kid", keys.SigningKeyID())
	return e, nil
}

// scimTokenAdmin adapts SCIM's token store to the admin API's interface; scim does not import
// deploy, so the adapter lives where both are wired.
type scimTokenAdmin struct{ t *scim.Tokens }

func (a scimTokenAdmin) Create(ctx context.Context, tenantID, label, by string) (string, string, error) {
	return a.t.Create(ctx, tenantID, label, by)
}

func (a scimTokenAdmin) Revoke(ctx context.Context, tenantID, id, by string) error {
	err := a.t.Revoke(ctx, tenantID, id, by)
	if scim.IsNotFound(err) {
		return fmt.Errorf("scim token %s: %w", id, deploy.ErrNotFound)
	}
	return err
}

func (a scimTokenAdmin) List(ctx context.Context, tenantID string) ([]deploy.ScimToken, error) {
	list, err := a.t.List(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	out := make([]deploy.ScimToken, 0, len(list))
	for _, t := range list {
		out = append(out, deploy.ScimToken{TokenID: t.TokenID, Label: t.Label, CreatedAt: t.CreatedAt, RevokedAt: t.RevokedAt})
	}
	return out, nil
}

// handler mounts the enterprise routes beside the device routes on the one listener.
func (e *enterprise) handler(device http.Handler) http.Handler {
	if len(e.extra) == 0 {
		return device
	}
	root := http.NewServeMux()
	root.Handle("/", device)
	for prefix, h := range e.extra {
		root.Handle(prefix, h)
	}
	return root
}

func envDuration(name string, def time.Duration) (time.Duration, error) {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s=%q is not a duration: %w", name, v, err)
	}
	return d, nil
}

func envFloat(name string, def float64) (float64, error) {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, fmt.Errorf("%s=%q is not a number: %w", name, v, err)
	}
	return f, nil
}

func envInt(name string, def int) (int, error) {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s=%q is not an integer: %w", name, v, err)
	}
	return n, nil
}
