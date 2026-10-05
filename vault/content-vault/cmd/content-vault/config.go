package main

import (
	"flag"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// Configuration: one vocabulary, two sources.
//
// Every setting is settable by a flag and by an environment variable, and a flag wins. That is what
// a container platform needs (it configures by environment, and its module has no command/args
// parameter) and what a person on a laptop expects. Precedence is decided by asking the flag package
// which flags were *passed*, not by comparing against a default, so `--addr ""` is a request rather
// than an absence.
//
// The environment names are the deployment's names: the SAC_* variables infra/main.bicep passes to
// this container app. That agreement is asserted by lab/tools/check-config-agreement.mjs, which reads
// this file, the Dockerfile and the Bicep and fails if they disagree in either direction.
//
// CONTENT_VAULT_* stay as they are: they are not deployment parameters but inputs from the policy
// bundle and the key store (scope tiers, KMS mode), and renaming them would move a control-plane
// seam for no reason. The checker lists them as service-specific rather than unread.
//
// This file is deliberately duplicated in services/ingest-api/cmd/ingest-api/config.go: the two
// services are separate Go modules with no shared dependency, and the checker is what keeps the two
// copies of the vocabulary equal.
const (
	// EnvHTTPAddr is the listen address. Set by the image: a container's loopback is unreachable.
	EnvHTTPAddr = "SAC_HTTP_ADDR"
	// EnvStore selects the persistence mode: memory | sql.
	EnvStore = "SAC_STORE"
	// EnvPGHost, EnvPGDatabase and EnvRole are the database coordinates the deployment passes. Read
	// even though this build cannot open a connection, so the refusal names the values.
	EnvPGHost     = "SAC_PG_HOST"
	EnvPGDatabase = "SAC_PG_DATABASE"
	EnvRole       = "SAC_ROLE"
	// EnvKeyBackend selects the key wrapping backend: local | kms.
	EnvKeyBackend = "SAC_KEY_BACKEND"
	// EnvKeyVaultURI is the Key Vault the kms backend would call.
	EnvKeyVaultURI = "SAC_KEYVAULT_URI"
	// EnvBlobCiphertextEndpoint is the private blob endpoint for ciphertext.
	EnvBlobCiphertextEndpoint = "SAC_BLOB_CIPHERTEXT_ENDPOINT"
	// EnvBlobIdentity selects how the vault authenticates to blob storage: static | managed. A
	// deployment uses managed, so the read carries the container app's identity and no secret is
	// stored; the lab uses static, a bearer its storage stand-in checks.
	EnvBlobIdentity = "SAC_BLOB_IDENTITY"
	// EnvBlobReadCredential is the lab's static storage bearer. It is read only when
	// EnvBlobIdentity is static, and a deployment must leave it unset.
	EnvBlobReadCredential = "SAC_BLOB_READ_CREDENTIAL"
	// EnvRetrievalURLBase is the origin a browser reaches a minted retrieval URL on. Empty mints a
	// path the caller resolves against the page's own origin.
	EnvRetrievalURLBase = "SAC_RETRIEVAL_URL_BASE"
	// EnvInternalOnly is the deployment's declaration that this app has internal ingress only. It is
	// the honest acknowledgement for a non-loopback bind in a container: the deployment states the
	// fact the flag asks the operator to assert.
	EnvInternalOnly = "SAC_INTERNAL_ONLY"
	// EnvAllowNonLoopback acknowledges a non-loopback bind from the environment, for a container
	// where the flag cannot be passed.
	EnvAllowNonLoopback = "SAC_ALLOW_NON_LOOPBACK"
	// EnvAppInsights is the Application Insights connection string. A credential: read only to
	// report whether it is configured, never logged.
	EnvAppInsights = "SAC_APPINSIGHTS"
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

// boolean resolves a boolean setting the same way.
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

// envTrue reports whether an environment variable is set to an affirmative value. It is used for
// SAC_INTERNAL_ONLY, which must be exactly the deployment's assertion rather than merely present.
func envTrue(name string) bool {
	v, ok := os.LookupEnv(name)
	if !ok {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// postgresDSN renders the connection string this build would use, for the refusal and the startup
// log. There is no password in it: a deployed PostgreSQL on this architecture authenticates the
// managed identity, and §5.4 forbids a credential in a plaintext environment variable.
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
// port is checked numerically: ":host" is a non-numeric port, which ListenAndServe would reject only
// after the process had reported itself as listening.
func validateHTTPAddr(addr string) error {
	if addr == "" {
		return fmt.Errorf("listen address is empty")
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("listen address %q is not host:port: %w", addr, err)
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		return fmt.Errorf("listen address %q has a non-numeric port", addr)
	}
	if n < 1 || n > 65535 {
		return fmt.Errorf("listen address %q has port %d outside 1-65535", addr, n)
	}
	return nil
}

// checkURL validates a configured endpoint without using it, so a deployment typo fails at boot.
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
