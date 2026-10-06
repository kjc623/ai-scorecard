package main

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// The environment control-api reads. The database settings (SAC_PG_HOST, SAC_PG_PORT,
// SAC_PG_DATABASE, SAC_PG_USER, SAC_PG_PASSWORD, SAC_PG_SSLMODE) are read by services/platform/postgres.
const (
	EnvHTTPAddr             = "SAC_HTTP_ADDR"
	EnvRegion               = "SAC_REGION"
	EnvCACertPEM            = "SAC_CA_CERT_PEM"
	EnvCAKeyPEM             = "SAC_CA_KEY_PEM"
	EnvAuthIssuer           = "SAC_AUTH_ISSUER"
	EnvSessionSigningKey    = "SAC_SESSION_SIGNING_KEY_FILE"
	EnvInternalToken        = "SAC_INTERNAL_TOKEN"
	EnvPublicURL            = "SAC_PUBLIC_URL"
	EnvAuthRedirectURIs     = "SAC_AUTH_REDIRECT_URIS"
	EnvDirectoryKey         = "SAC_DIRECTORY_KEY"
	EnvPublicDeviceEndpoint = "SAC_PUBLIC_DEVICE_ENDPOINT"
	EnvPolicySigningKey     = "SAC_POLICY_SIGNING_KEY_FILE"
	EnvPolicySigningKeyID   = "SAC_POLICY_SIGNING_KEY_ID"
	EnvAgentReleaseDir      = "SAC_AGENT_RELEASE_DIR"
	EnvContentVaultURL      = "SAC_CONTENT_VAULT_URL"
	EnvEntraClientID        = "SAC_ENTRA_CLIENT_ID"
	EnvEntraClientSecret    = "SAC_ENTRA_CLIENT_SECRET"
	EnvEntraFIC             = "SAC_ENTRA_FIC"
	EnvAuthAllowInsecureIdP = "SAC_AUTH_ALLOW_INSECURE_IDP"
)

// Defaults for the settings that have one.
const (
	defaultHTTPAddr        = "0.0.0.0:8080"
	defaultAgentReleaseDir = "/opt/sac/agent-release"
	// callbackPath is the dashboard's sign-in return path under SAC_PUBLIC_URL.
	callbackPath = "/callback"
)

// config is the service's resolved environment.
type config struct {
	HTTPAddr             string
	Region               string
	CACertPEM            []byte
	CAKeyPEM             []byte
	AuthIssuer           string
	SessionSigningKey    string
	InternalToken        string
	PublicURL            string
	RedirectURIs         []string
	DirectoryKey         string
	PublicDeviceEndpoint string
	PolicySigningKey     string
	PolicySigningKeyID   string
	AgentReleaseDir      string
	ContentVaultURL      string
	EntraClientID        string
	EntraClientSecret    string
	EntraFIC             string
	AllowInsecureIdP     bool
}

// loadConfig reads and checks the environment, reporting every missing or malformed setting at
// once.
func loadConfig(getenv func(string) string) (config, error) {
	get := func(name string) string { return strings.TrimSpace(getenv(name)) }
	c := config{
		HTTPAddr:             get(EnvHTTPAddr),
		Region:               get(EnvRegion),
		CACertPEM:            []byte(getenv(EnvCACertPEM)),
		CAKeyPEM:             []byte(getenv(EnvCAKeyPEM)),
		AuthIssuer:           strings.TrimRight(get(EnvAuthIssuer), "/"),
		SessionSigningKey:    get(EnvSessionSigningKey),
		InternalToken:        get(EnvInternalToken),
		PublicURL:            strings.TrimRight(get(EnvPublicURL), "/"),
		DirectoryKey:         get(EnvDirectoryKey),
		PublicDeviceEndpoint: strings.TrimRight(get(EnvPublicDeviceEndpoint), "/"),
		PolicySigningKey:     get(EnvPolicySigningKey),
		PolicySigningKeyID:   get(EnvPolicySigningKeyID),
		AgentReleaseDir:      get(EnvAgentReleaseDir),
		ContentVaultURL:      strings.TrimRight(get(EnvContentVaultURL), "/"),
		EntraClientID:        get(EnvEntraClientID),
		EntraClientSecret:    getenv(EnvEntraClientSecret),
		EntraFIC:             get(EnvEntraFIC),
	}
	if c.HTTPAddr == "" {
		c.HTTPAddr = defaultHTTPAddr
	}
	if c.AgentReleaseDir == "" {
		c.AgentReleaseDir = defaultAgentReleaseDir
	}
	var errs []error
	if v := get(EnvAuthAllowInsecureIdP); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s=%q is not a boolean", EnvAuthAllowInsecureIdP, v))
		}
		c.AllowInsecureIdP = b
	}
	for _, s := range []struct{ name, value string }{
		{EnvCACertPEM, string(c.CACertPEM)}, {EnvCAKeyPEM, string(c.CAKeyPEM)},
		{EnvSessionSigningKey, c.SessionSigningKey}, {EnvInternalToken, c.InternalToken},
		{EnvDirectoryKey, c.DirectoryKey}, {EnvPolicySigningKey, c.PolicySigningKey},
	} {
		if strings.TrimSpace(s.value) == "" {
			errs = append(errs, fmt.Errorf("%s is required", s.name))
		}
	}
	for _, s := range []struct{ name, value string }{
		{EnvAuthIssuer, c.AuthIssuer}, {EnvPublicURL, c.PublicURL},
		{EnvPublicDeviceEndpoint, c.PublicDeviceEndpoint}, {EnvContentVaultURL, c.ContentVaultURL},
	} {
		if err := checkURL(s.name, s.value); err != nil {
			errs = append(errs, err)
		}
	}
	if err := checkHTTPAddr(c.HTTPAddr); err != nil {
		errs = append(errs, err)
	}
	// The sign-in redirect URIs are exactly the dashboard's callback under the public URL, plus any
	// further dashboard origins named explicitly.
	if c.PublicURL != "" {
		c.RedirectURIs = []string{c.PublicURL + callbackPath}
	}
	for _, u := range strings.Split(get(EnvAuthRedirectURIs), ",") {
		if u = strings.TrimSpace(u); u == "" {
			continue
		}
		if err := checkURL(EnvAuthRedirectURIs, u); err != nil {
			errs = append(errs, err)
			continue
		}
		c.RedirectURIs = append(c.RedirectURIs, u)
	}
	return c, errors.Join(errs...)
}

// checkURL requires an absolute http(s) URL.
func checkURL(name, value string) error {
	if value == "" {
		return fmt.Errorf("%s is required", name)
	}
	u, err := url.Parse(value)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return fmt.Errorf("%s=%q is not an absolute http(s) URL", name, value)
	}
	return nil
}

func checkHTTPAddr(addr string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%s=%q is not host:port", EnvHTTPAddr, addr)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("%s=%q has no valid port", EnvHTTPAddr, addr)
	}
	return nil
}
