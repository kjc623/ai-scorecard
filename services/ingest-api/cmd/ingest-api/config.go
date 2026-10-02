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
	// EnvAppInsights is the Application Insights connection string. It is a credential: it is read
	// only to report whether it is configured, and its value is never logged.
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
