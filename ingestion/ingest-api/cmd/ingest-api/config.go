package main

import (
	"crypto"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/shadow-ai-capture/device/protocol"
)

// Configuration: one vocabulary, two sources.
//
// Every setting is settable by a flag and by an environment variable, and a flag wins. That is what
// a container platform needs (it configures by environment, and its module has no command/args
// parameter) and what a person on a laptop expects (they type a flag). The precedence rule is
// implemented by asking the flag package which flags were actually *passed* rather than by
// comparing against the default, so `--addr ""` is a request rather than an absence.
//
// The environment names are the deployment's names: they are the SAC_* variables
// infra/main.bicep passes to the container app. That agreement is not a convention here, it is
// asserted by lab/tools/check-config-agreement.mjs, which reads this file, the Dockerfile and the
// Bicep and fails if they disagree in either direction. The seam had already rotted twice by hand.
//
// This file is deliberately duplicated in services/content-vault/cmd/content-vault/config.go: the
// two services are separate Go modules with no shared dependency, and the checker is what keeps the
// two copies of the *vocabulary* equal, which is the part that matters.
const (
	// EnvHTTPAddr is the listen address. Set by the image, because a container's loopback is
	// unreachable by design.
	EnvHTTPAddr = "SAC_HTTP_ADDR"
	// EnvStore selects the persistence mode: memory | sql.
	EnvStore = "SAC_STORE"
	// EnvPGHost, EnvPGDatabase and EnvRole are the database coordinates the deployment passes. They
	// are read even though this build cannot open a connection: the refusal below names the values
	// it would have used, which is what makes the gap actionable rather than mysterious.
	EnvPGHost     = "SAC_PG_HOST"
	EnvPGDatabase = "SAC_PG_DATABASE"
	// EnvRole is the identity the deployment intends this process to run as (ingest-api).
	EnvRole = "SAC_ROLE"
	// EnvRegion pins the deployment's region for the §12 refusal.
	EnvRegion = "SAC_REGION"
	// EnvRoutesFile is ref.route_fidelity as data. §4.4: the ranking is stored, never compiled in.
	EnvRoutesFile = "SAC_ROUTES_FILE"
	// EnvSchema is the contract schema the service validates against.
	EnvSchema = "SAC_SCHEMA"
	// EnvBlobCiphertextEndpoint is the private blob endpoint the deployment configures. Read so a
	// malformed value fails at boot and so the startup log can say plainly that this build performs
	// no blob I/O: the ingest path writes events, and ciphertext moves device→blob under a grant
	// (docs/02 §10). A deployment parameter that nothing reads is worse than one that is read and
	// reported unused, which is what the agreement test enforces.
	EnvBlobCiphertextEndpoint = "SAC_BLOB_CIPHERTEXT_ENDPOINT"
	// EnvAppInsights is the Application Insights connection string. It is a credential: it is read
	// only to report whether it is configured, and its value is never logged.
	EnvAppInsights = "SAC_APPINSIGHTS"
	// EnvAuthModes is the comma list of production modes to serve: a subset of "x509,dpop". Dev is
	// not a wire mode and remains the separate -dev-trust-principal acknowledgement (ADR 0020 §2).
	EnvAuthModes = "SAC_AUTH_MODES"
	// EnvTLSClientCertHeader is the edge-forwarded leaf certificate header. Empty disables the
	// forwarded x509 path; the direct peer-certificate path is unaffected.
	EnvTLSClientCertHeader = "SAC_TLS_CLIENT_CERT_HEADER"
	// EnvDPoPTokenPublicPEM is the deployment access-token signing public key as PEM text.
	EnvDPoPTokenPublicPEM = "SAC_DPOP_TOKEN_PUBLIC_PEM"
	// EnvDPoPIssuer and EnvDPoPAudience are the registered iss and aud a token must carry.
	EnvDPoPIssuer   = "SAC_DPOP_ISSUER"
	EnvDPoPAudience = "SAC_DPOP_AUDIENCE"
)

// authMaterial is what the deployment actually configured for device authentication. It is the
// input to checkAuthMaterial, so "a mode was enabled without its material" is a startup refusal
// with a specific message rather than a request-time failure.
type authMaterial struct {
	// clientCA is the bundle forwarded and direct x509 chains verify against.
	clientCA *x509.CertPool
	// directTLS is true when the process holds a server key pair and can terminate client TLS itself.
	directTLS bool
	// forwardedHeader is the forwarding header; empty means the forwarded path is disabled.
	forwardedHeader string
	tokenPublicKey  crypto.PublicKey
	issuer          string
	audience        string
}

// parseAuthModes parses SAC_AUTH_MODES. The empty string means "not configured", which lets main
// infer a legacy default from the material present. Anything outside the closed x509/dpop set is
// refused rather than defaulted: a typo must not silently disable device authentication.
func parseAuthModes(raw string) ([]protocol.AuthMode, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	seen := map[protocol.AuthMode]bool{}
	var modes []protocol.AuthMode
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, fmt.Errorf("auth mode list %q has an empty entry", raw)
		}
		mode := protocol.AuthMode(part)
		if !mode.Valid() {
			return nil, fmt.Errorf("auth mode %q is not one of x509, dpop (dev is -dev-trust-principal)", part)
		}
		if seen[mode] {
			continue
		}
		seen[mode] = true
		modes = append(modes, mode)
	}
	return modes, nil
}

// inferAuthModes is the backward-compatible default when SAC_AUTH_MODES is unset: serve x509 when
// any certificate material is present, dpop when the token key is, and nothing otherwise. A
// deployment that wants a mode with no material still has to name it and be refused.
func inferAuthModes(m authMaterial) []protocol.AuthMode {
	var modes []protocol.AuthMode
	if m.directTLS || m.forwardedHeader != "" {
		modes = append(modes, protocol.AuthModeX509)
	}
	if m.tokenPublicKey != nil {
		modes = append(modes, protocol.AuthModeDPoP)
	}
	return modes
}

// checkAuthMaterial refuses a mode set the configured material cannot serve, before the listener
// starts. It is the "refuse to start if a mode is enabled without its material" rule from ADR 0020
// decision 2, and it names the setting an operator has to supply.
func checkAuthMaterial(modes []protocol.AuthMode, m authMaterial) error {
	for _, mode := range modes {
		switch mode {
		case protocol.AuthModeX509:
			if !m.directTLS && m.forwardedHeader == "" {
				return errors.New("x509 mode is enabled but neither TLS server material (--tls-cert/--tls-key) " +
					"nor a forwarded-certificate header (--tls-client-cert-header) is configured")
			}
			if m.clientCA == nil {
				return fmt.Errorf("x509 mode is enabled but no client CA bundle is configured (--tls-client-ca / %s)", EnvTLSClientCAPEM)
			}
		case protocol.AuthModeDPoP:
			if m.tokenPublicKey == nil {
				return fmt.Errorf("dpop mode is enabled but no token public key is configured (--dpop-token-public-pem / %s)", EnvDPoPTokenPublicPEM)
			}
			if m.issuer == "" || m.audience == "" {
				return fmt.Errorf("dpop mode is enabled but the token issuer and audience must both be configured (--dpop-issuer / %s, --dpop-audience / %s)", EnvDPoPIssuer, EnvDPoPAudience)
			}
		default:
			return fmt.Errorf("unknown auth mode %q", mode)
		}
	}
	return nil
}

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

// boolean resolves a boolean setting the same way. An explicitly passed `--flag=false` wins over an
// environment variable that says true, which is the only reading of "flag wins" that is not
// surprising.
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

// postgresDSN renders the connection string this build would use, for the refusal and for the
// startup log. There is no password in it: a deployed PostgreSQL on this architecture authenticates
// the managed identity, so the credential is a token obtained from the platform, not a secret in
// the environment (infra/main.bicep passes no password, and §5.4 forbids one in a plaintext
// variable). sslmode=require because the database has no public listener and no plaintext path.
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

// validateHTTPAddr rejects an address the server cannot bind before anything else starts, so a
// configuration typo is a startup failure rather than a container that runs and serves nothing. The
// port is checked numerically: ":host" is a host with a non-numeric port, which ListenAndServe would
// reject only after the process reported itself as listening.
// redactDSN removes the password from a DSN before it can reach a log. The service never logs a
// credential: infra/main.bicep passes none, because the deployed database authenticates a managed
// identity — but a local run may, and a log line is a durable place for a mistake to live.
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

func validateHTTPAddr(addr string) error {
	if addr == "" {
		return fmt.Errorf("listen address is empty")
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("listen address %q is not host:port: %w", addr, err)
	}
	_ = host // an empty host means "every interface", which is what a container wants
	n, err := strconv.Atoi(port)
	if err != nil {
		return fmt.Errorf("listen address %q has a non-numeric port", addr)
	}
	if n < 1 || n > 65535 {
		return fmt.Errorf("listen address %q has port %d outside 1-65535", addr, n)
	}
	return nil
}

// checkURL validates a configured endpoint without using it, so a typo in a deployment parameter
// fails at boot rather than the first time a request needs it.
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
