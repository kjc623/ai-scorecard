package main

import (
	"flag"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Configuration: one vocabulary, two sources.
//
// Every setting is settable by a flag and by an environment variable, and a flag wins. That is what a
// container platform needs (it configures by environment, and its module has no command/args
// parameter) and what a person on a laptop expects (they type a flag). Precedence is decided by
// asking the flag package which flags were actually *passed*, so `--region ""` is a request rather
// than an absence.
//
// The environment names are the deployment's names: azure/main.bicep passes SAC_ROLE, SAC_PG_HOST,
// SAC_PG_DATABASE, SAC_KEYVAULT_URI and SAC_APPINSIGHTS to the control-api container app, and
// cmd/control-api/infra_agreement_test.go fails if this binary and that deployment disagree in
// either direction.
const (
	// EnvHTTPAddr is the listen address. Set by the image, because a container's loopback is
	// unreachable by design.
	EnvHTTPAddr = "SAC_HTTP_ADDR"
	// EnvStore selects the persistence mode: memory | sql.
	EnvStore = "SAC_STORE"
	// EnvPGHost, EnvPGDatabase and EnvRole are the database coordinates the deployment passes.
	EnvPGHost     = "SAC_PG_HOST"
	EnvPGDatabase = "SAC_PG_DATABASE"
	// EnvRole is the identity the deployment intends this process to run as (control-api).
	EnvRole = "SAC_ROLE"
	// EnvRegion pins the deployment's region for the §12 refusal.
	EnvRegion = "SAC_REGION"
	// EnvKeyVaultURI is the Key Vault / Managed HSM URI the deployment passes. It selects the
	// KeyVaultSigner, which refuses clearly until this build carries a vault client rather than
	// signing with a local key and calling it custody.
	EnvKeyVaultURI = "SAC_KEYVAULT_URI"
	// EnvCACertPEM and EnvCAKeyPEM are the LocalCA key pair as PEM text, the form a deployment
	// injects from Key Vault (the Container Apps module has no volume mount). Empty on a laptop,
	// where the CA is generated fresh, or when the flags point at files.
	EnvCACertPEM = "SAC_CA_CERT_PEM"
	EnvCAKeyPEM  = "SAC_CA_KEY_PEM"
	// EnvTokenIssuer and EnvTokenAudience are the iss and aud of issued access tokens.
	EnvTokenIssuer   = "SAC_TOKEN_ISSUER"
	EnvTokenAudience = "SAC_TOKEN_AUDIENCE"
	// EnvDPoPTokenKeyPEM is the ES256 access-token signing key as PEM text. The service refuses to
	// start without it: an endpoint that issued tokens under an unstated key would be a different
	// authority from the one the deployment configured.
	EnvDPoPTokenKeyPEM = "SAC_DPOP_TOKEN_KEY_PEM"
	// EnvEnrolmentTokenTTL is the life an operator-side token-minting path would give a token. It is
	// read and recorded so the vocabulary is complete; the request path only verifies tokens the MDM
	// profile carried, and that path is not built here.
	EnvEnrolmentTokenTTL = "SAC_ENROLMENT_TOKEN_TTL"
	// EnvCredentialTTL is the life of an issued credential.
	EnvCredentialTTL = "SAC_CREDENTIAL_TTL"
	// EnvAppInsights is the Application Insights connection string. It is a credential: it is read
	// only to report whether it is configured, and its value is never logged.
	EnvAppInsights = "SAC_APPINSIGHTS"
)

// The enterprise surface (enterprise.go): the product's identity service, SCIM, deployment keys,
// policy delivery and the admin API. Each part is off until its settings are present, and says so
// at startup, so a deployment never half-runs one silently.
const (
	// EnvAuthIssuer turns on the identity service: the exact `iss` of product access tokens, which
	// query-api and content-vault pin too.
	EnvAuthIssuer = "SAC_AUTH_ISSUER"
	// EnvSessionSigningKeyFile is the P-256 key that signs product access tokens (several PEM blocks
	// publish rotation keys). It is not the device token key.
	EnvSessionSigningKeyFile = "SAC_SESSION_SIGNING_KEY_FILE"
	// EnvInternalToken authenticates the dashboard's server on /internal/v1/auth/*.
	EnvInternalToken = "SAC_INTERNAL_TOKEN"
	// EnvPublicURL is the browser-facing origin: redirect URIs, onboarding links, SCIM base URL.
	EnvPublicURL = "SAC_PUBLIC_URL"
	// EnvPublicDeviceEndpoint is the device edge written into every tenant package.
	EnvPublicDeviceEndpoint = "SAC_PUBLIC_DEVICE_ENDPOINT"
	// EnvDirectoryKey seals every *_enc column and derives the tenant user-reference keys.
	EnvDirectoryKey = "SAC_DIRECTORY_KEY"
	// EnvAuthAllowInsecureIdP admits http issuers and private addresses: the lab's stand-in only.
	EnvAuthAllowInsecureIdP = "SAC_AUTH_ALLOW_INSECURE_IDP"
	// EnvAuthRedirectURIs is the exact set of redirect URIs sign-in accepts (comma-separated).
	EnvAuthRedirectURIs = "SAC_AUTH_REDIRECT_URIS"
	// EnvAuthTokenTTL is the product token life, clamped to ten minutes.
	EnvAuthTokenTTL = "SAC_AUTH_TOKEN_TTL"
	// The vendor's multi-tenant Entra app, read by internal/entraapp. Exactly one credential:
	// a federated managed identity (preferred), a certificate, or a secret (lab only).
	EnvEntraClientID     = "SAC_ENTRA_CLIENT_ID"
	EnvEntraClientSecret = "SAC_ENTRA_CLIENT_SECRET"
	EnvEntraCertFile     = "SAC_ENTRA_CERT_FILE"
	EnvEntraFIC          = "SAC_ENTRA_FIC"
	EnvEntraMIClientID   = "SAC_ENTRA_MI_CLIENT_ID"
	EnvEntraLoginBase    = "SAC_ENTRA_LOGIN_BASE"
	// EnvGraphURL points the Intune check at a Graph stand-in; empty is Microsoft Graph.
	EnvGraphURL = "SAC_GRAPH_URL"
	// EnvPolicySigningKeyFile signs policy bundles; its public half is the vendor trust anchor the
	// generic MSI pins. EnvPolicySigningKeyID must equal the key id the MSI pins.
	EnvPolicySigningKeyFile = "SAC_POLICY_SIGNING_KEY_FILE"
	EnvPolicySigningKeyID   = "SAC_POLICY_SIGNING_KEY_ID"
	EnvPolicyRecheck        = "SAC_POLICY_RECHECK"
	// EnvAgentReleaseDir holds the generic ShadowAICapture.msi and its release.json.
	EnvAgentReleaseDir = "SAC_AGENT_RELEASE_DIR"
	// Deployment-key limits: life (0 = no expiry; revocation is the control) and per-key rate.
	EnvDeploymentKeyTTL   = "SAC_DEPLOYMENT_KEY_TTL"
	EnvDeploymentKeyRate  = "SAC_DEPLOYMENT_KEY_RATE"
	EnvDeploymentKeyBurst = "SAC_DEPLOYMENT_KEY_BURST"
	// EnvSCIMPopulationAttribute names the SCIM attribute copied to ops.user_dim.population.
	EnvSCIMPopulationAttribute = "SAC_SCIM_POPULATION_ATTRIBUTE"
)

// flagSet records which flags the operator passed.
type flagSet map[string]bool

// visited captures the flags that were set on the command line.
func visited(fs *flag.FlagSet) flagSet {
	m := flagSet{}
	fs.Visit(func(f *flag.Flag) { m[f.Name] = true })
	return m
}

// str resolves a string setting: flag (when passed) > environment > default.
func (s flagSet) str(name, current, env, def string) string {
	if s[name] {
		return current
	}
	if v, ok := os.LookupEnv(env); ok && v != "" {
		return v
	}
	return def
}

// boolean resolves a boolean setting the same way, so `--flag=false` beats an environment true.
func (s flagSet) boolean(name string, current bool, env string, def bool) bool {
	if s[name] {
		return current
	}
	if v, ok := os.LookupEnv(env); ok {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "1", "true", "yes", "on":
			return true
		case "0", "false", "no", "off":
			return false
		}
	}
	return def
}

// duration resolves a duration as flag > environment > default.
func (s flagSet) duration(name string, current time.Duration, env string, def time.Duration) time.Duration {
	if s[name] {
		return current
	}
	if v, ok := os.LookupEnv(env); ok && v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

// postgresDSN renders the connection string this build would use, for the refusal and the startup
// log. There is no password in it: a deployed PostgreSQL authenticates the managed identity, so the
// credential is a token obtained from the platform, not a secret in the environment.
func postgresDSN(host, port, database, role string) string {
	if host == "" {
		return ""
	}
	if port == "" {
		port = "5432"
	}
	if database == "" {
		database = "shadow"
	}
	return fmt.Sprintf("postgres://%s@%s:%s/%s?sslmode=require", url.User(role).String(), host, port, database)
}

// validateHTTPAddr rejects an address the server cannot bind before anything else starts.
func validateHTTPAddr(addr string) error {
	if addr == "" {
		return fmt.Errorf("listen address is empty")
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("listen address %q is not host:port: %w", addr, err)
	}
	_ = host // an empty host means every interface, which is what a container wants
	n, err := strconv.Atoi(port)
	if err != nil {
		return fmt.Errorf("listen address %q has a non-numeric port", addr)
	}
	if n < 1 || n > 65535 {
		return fmt.Errorf("listen address %q has port %d outside 1-65535", addr, n)
	}
	return nil
}

// checkURL validates a configured endpoint without using it, so a typo fails at boot.
func checkURL(name, value string) error {
	if value == "" {
		return nil
	}
	u, err := url.Parse(value)
	if err != nil {
		return fmt.Errorf("%s %q is not a URL: %w", name, value, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("%s %q is not an absolute URL", name, value)
	}
	return nil
}

// redactDSN removes the password from a DSN before it can reach a log.
func redactDSN(dsn string) string {
	at := strings.LastIndex(dsn, "@")
	proto := strings.Index(dsn, "://")
	if at < 0 || proto < 0 || at < proto {
		return dsn
	}
	creds := dsn[proto+3 : at]
	if i := strings.Index(creds, ":"); i >= 0 {
		creds = creds[:i] + ":<redacted>"
	}
	return dsn[:proto+3] + creds + dsn[at:]
}
