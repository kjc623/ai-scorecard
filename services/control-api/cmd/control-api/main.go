// Command control-api is the product's control plane: device enrolment, policy, health and content
// grants for devices; the sign-in, session and product-token service, onboarding and SCIM for
// people; and the deployment admin API.
//
//	control-api                      serve (configured by environment)
//	control-api tenant create ...    create a tenant (see tenant.go)
//	control-api tenant invite ...    issue a tenant's onboarding link
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/shadow-ai-capture/platform/postgres"

	"github.com/shadow-ai-capture/control-api/internal/content"
	"github.com/shadow-ai-capture/control-api/internal/deploy"
	"github.com/shadow-ai-capture/control-api/internal/deviceca"
	"github.com/shadow-ai-capture/control-api/internal/directory"
	"github.com/shadow-ai-capture/control-api/internal/enrol"
	"github.com/shadow-ai-capture/control-api/internal/entraapp"
	"github.com/shadow-ai-capture/control-api/internal/health"
	"github.com/shadow-ai-capture/control-api/internal/httpapi"
	"github.com/shadow-ai-capture/control-api/internal/identity"
	"github.com/shadow-ai-capture/control-api/internal/intune"
	"github.com/shadow-ai-capture/control-api/internal/onboard"
	"github.com/shadow-ai-capture/control-api/internal/policyserve"
	"github.com/shadow-ai-capture/control-api/internal/scim"
	"github.com/shadow-ai-capture/control-api/internal/session"
	"github.com/shadow-ai-capture/control-api/internal/store"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	var err error
	if len(os.Args) > 1 && os.Args[1] == "tenant" {
		err = runTenant(os.Args[2:])
	} else {
		err = serve(logger)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "control-api:", err)
		os.Exit(1)
	}
}

// openDatabase opens the pool from the SAC_PG_* environment.
func openDatabase() (*sql.DB, error) {
	cfg, err := postgres.ConfigFromEnv()
	if err != nil {
		return nil, err
	}
	return postgres.Open(cfg)
}

func serve(logger *slog.Logger) error {
	cfg, err := loadConfig(os.Getenv)
	if err != nil {
		return fmt.Errorf("configuration:\n%w", err)
	}
	db, err := openDatabase()
	if err != nil {
		return err
	}
	defer db.Close()

	srv, issuer, err := wire(cfg, db, logger)
	if err != nil {
		return err
	}
	httpServer := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       90 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		logger.Info("control-api listening", "addr", cfg.HTTPAddr, "region", cfg.Region,
			"issuer", issuer.Issuer(), "entra", cfg.EntraClientID != "")
		errCh <- httpServer.ListenAndServe()
	}()
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-stop:
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	logger.Info("shutting down")
	return httpServer.Shutdown(ctx)
}

// wire builds every service over the database and mounts it on one server.
func wire(cfg config, db *sql.DB, logger *slog.Logger) (*httpapi.Server, *session.Issuer, error) {
	st := store.NewSQL(db)

	ca, err := deviceca.New(cfg.CACertPEM, cfg.CAKeyPEM)
	if err != nil {
		return nil, nil, fmt.Errorf("%s/%s: %w", EnvCACertPEM, EnvCAKeyPEM, err)
	}
	key, err := directory.DecodeKey(cfg.DirectoryKey)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", EnvDirectoryKey, err)
	}
	cipher, err := directory.NewCipher(key)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", EnvDirectoryKey, err)
	}
	userRefKeys, err := directory.NewUserRefKeys(directory.NewKeyStore(db), cipher)
	if err != nil {
		return nil, nil, err
	}

	keys, err := session.LoadKeyFile(cfg.SessionSigningKey)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", EnvSessionSigningKey, err)
	}
	issuer, err := session.NewIssuer(keys, session.IssuerConfig{Issuer: cfg.AuthIssuer})
	if err != nil {
		return nil, nil, err
	}

	policyKey, err := policyserve.LoadSigningKey(cfg.PolicySigningKey)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", EnvPolicySigningKey, err)
	}
	policy, err := policyserve.New(st, policyKey, policyserve.Config{KeyID: cfg.PolicySigningKeyID, Logger: logger})
	if err != nil {
		return nil, nil, err
	}
	logger.Info("policy signing key loaded", "kid", policy.KeyID(), "public_key_hex", policy.PublicKeyHex())

	// The vendor's Entra app is optional: a deployment whose customers all use another OpenID
	// Connect provider has none.
	app, err := entraapp.New(entraapp.Config{ClientID: cfg.EntraClientID, ClientSecret: cfg.EntraClientSecret, FIC: cfg.EntraFIC})
	switch {
	case errors.Is(err, entraapp.ErrNotConfigured):
		app = nil
	case err != nil:
		return nil, nil, err
	}
	var checker intune.Checker
	var entraID *identity.EntraConfig
	var entraOnboard *onboard.EntraConfig
	if app != nil {
		gc, err := intune.NewGraphChecker(app, nil)
		if err != nil {
			return nil, nil, err
		}
		checker = gc
		entraID = &identity.EntraConfig{ClientID: app.ClientID(), Auth: app}
		entraOnboard = &onboard.EntraConfig{ClientID: app.ClientID(), ConsentProbe: func(ctx context.Context, tid string) error {
			_, err := app.Token(ctx, tid, entraapp.GraphScope)
			return err
		}}
		logger.Info("Entra app configured", "client_id", app.ClientID(), "credential", app.Credential())
	}
	if cfg.AllowInsecureIdP {
		logger.Warn("identity providers may use http and private addresses; this is for the local lab only", "env", EnvAuthAllowInsecureIdP)
	}

	enrolSvc, err := enrol.New(st, ca, enrol.Config{
		Region: cfg.Region, Intune: checker, UserRefKeys: userRefKeys, PolicyETag: policy,
	})
	if err != nil {
		return nil, nil, err
	}
	healthSvc, err := health.New(st, health.Config{})
	if err != nil {
		return nil, nil, err
	}
	contentSvc, err := content.New(content.NewSQL(db), content.NewHTTPVault(cfg.ContentVaultURL, issuer))
	if err != nil {
		return nil, nil, err
	}

	sessions, err := session.NewManager(session.NewSQL(db), session.ManagerConfig{})
	if err != nil {
		return nil, nil, err
	}
	idStore := identity.NewSQL(db)
	idSvc, err := identity.New(identity.Config{
		Store: idStore, Sessions: sessions, Issuer: issuer, Cipher: cipher, Entra: entraID,
		RedirectURIs: cfg.RedirectURIs, AllowInsecureIdP: cfg.AllowInsecureIdP, Logger: logger,
	})
	if err != nil {
		return nil, nil, err
	}
	internalAPI, err := idSvc.InternalHandler(cfg.InternalToken)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", EnvInternalToken, err)
	}
	onboarding, err := onboard.New(onboard.Config{
		Store: idStore, Cipher: cipher, PublicURL: cfg.PublicURL, Entra: entraOnboard,
		AllowInsecureIssuers: cfg.AllowInsecureIdP, Logger: logger,
	})
	if err != nil {
		return nil, nil, err
	}

	scimStore := scim.NewSQL(db)
	scimBase := cfg.PublicURL + "/scim/v2"
	scimSvc, err := scim.NewService(scimStore, userRefKeys, cipher, scim.Config{BaseURL: scimBase})
	if err != nil {
		return nil, nil, err
	}
	scimSvc.Logger = logger

	verifier := session.NewVerifier(issuer)
	admin, err := deploy.NewHandler(st, adminAuthenticator(verifier), scimTokens{scim.NewTokens(scimStore)}, deploy.Config{
		ReleaseDir: cfg.AgentReleaseDir, DeviceEndpoint: cfg.PublicDeviceEndpoint, ScimBaseURL: scimBase, Logger: logger,
	})
	if err != nil {
		return nil, nil, err
	}

	return &httpapi.Server{
		Store: st, CA: ca, Enrol: enrolSvc, Health: healthSvc, Policy: policy, Content: contentSvc,
		Admin:      admin,
		Extensions: deploy.NewExtensions(cfg.AgentReleaseDir, cfg.PublicDeviceEndpoint, logger),
		SCIM:       scim.NewHandler(scimSvc, "/scim/v2", logger),
		Mounts: map[string]http.Handler{
			"/internal/v1/auth/": internalAPI,
			onboard.PathPrefix:   onboarding.Handler(),
			"/.well-known/":      issuer.WellKnownHandler(),
		},
		Logger: logger,
	}, issuer, nil
}

// adminAuthenticator resolves an admin request's principal from its product access token
// (audience sac-control). The audited actor comes from the verified token, never from a header.
func adminAuthenticator(v *session.Verifier) deploy.Authenticator {
	return func(r *http.Request) (deploy.Principal, error) {
		h := r.Header.Get("Authorization")
		scheme, token, ok := strings.Cut(h, " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") {
			return deploy.Principal{}, errors.New("no bearer token")
		}
		p, err := v.Verify(strings.TrimSpace(token), session.AudienceControl)
		if err != nil {
			return deploy.Principal{}, err
		}
		return deploy.Principal{Tenant: p.Tenant, Actor: p.Actor, Subject: p.Subject, Roles: p.Roles}, nil
	}
}

// scimTokens adapts SCIM's token store to the admin API.
type scimTokens struct{ t *scim.Tokens }

func (a scimTokens) Create(ctx context.Context, tenantID, label, by string) (string, string, error) {
	return a.t.Create(ctx, tenantID, label, by)
}

func (a scimTokens) Revoke(ctx context.Context, tenantID, id, by string) error {
	err := a.t.Revoke(ctx, tenantID, id, by)
	if scim.IsNotFound(err) {
		return fmt.Errorf("scim token %s: %w", id, deploy.ErrNotFound)
	}
	return err
}

func (a scimTokens) List(ctx context.Context, tenantID string) ([]deploy.ScimToken, error) {
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
