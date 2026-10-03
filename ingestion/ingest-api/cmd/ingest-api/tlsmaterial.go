package main

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"strings"
)

// TLS material for the device-facing channel, in the two forms a deployment can actually deliver.
//
// # Why both forms exist
//
// §2.1 requires mutual authentication and §2.2 makes the origin re-validate the credential on every
// request, so the origin needs a server key pair and the CA that signed the device certificates. On a
// laptop that is three files and three flags. In a container it is neither: the Container Apps module
// has no command/args parameter to pass flags with, and it has no volume mount for files — but it does
// have keyVaultEnv, which injects a Key Vault secret as an environment variable under its own
// managed identity. So the same material arrives as PEM text:
//
//	SAC_TLS_CERT_PEM        the server certificate chain
//	SAC_TLS_KEY_PEM         its private key
//	SAC_TLS_CLIENT_CA_PEM   the CA bundle that must have signed the device certificates
//
// Precedence follows the one rule the whole configuration uses — a flag wins — so files
// (--tls-cert/--tls-key/--tls-client-ca) take precedence over the environment when both are present.
// A deployment sets one or the other, not both.
//
// # What this does not change
//
// It is deliberately not a new trust model. The material still comes from Key Vault under the
// workload's managed identity, the listener is still TLS 1.3 with RequireAndVerifyClientCert, and the
// per-device credential status is still re-checked in the write transaction. It only removes the
// assumption that the material must arrive as files.

const (
	// EnvTLSCertPEM is the server certificate chain as PEM text.
	EnvTLSCertPEM = "SAC_TLS_CERT_PEM"
	// EnvTLSKeyPEM is the server private key as PEM text.
	EnvTLSKeyPEM = "SAC_TLS_KEY_PEM"
	// EnvTLSClientCAPEM is the CA bundle that must have issued the device certificates, as PEM text.
	EnvTLSClientCAPEM = "SAC_TLS_CLIENT_CA_PEM"
)

// tlsMaterial is a loaded server key pair and client CA pool.
type tlsMaterial struct {
	cert      tls.Certificate
	clientCAs *x509.CertPool
	// source names where the material came from, for the startup log. It never carries material.
	source string
}

// loadClientCAPool resolves the device CA bundle from the file flag first and the environment
// second. It returns (nil, nil) when neither is configured, which is a valid state the caller
// checks: x509 cannot serve without roots, and the forwarded path must not trust an unverified
// chain. The CA is loaded separately from the server key pair so a forwarded-only deployment — the
// edge terminates TLS and no listener-level mTLS is configured — can still supply roots without a
// server certificate.
func loadClientCAPool(o options) (*x509.CertPool, error) {
	if o.tlsClientCA != "" {
		return loadCAPool(o.tlsClientCA)
	}
	pemText := os.Getenv(EnvTLSClientCAPEM)
	if strings.TrimSpace(pemText) == "" {
		return nil, nil
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(pemText)) {
		return nil, fmt.Errorf("%s contains no certificates", EnvTLSClientCAPEM)
	}
	return pool, nil
}

// loadTLSMaterial resolves the direct-listener material: a server key pair, present only when the
// process itself terminates client TLS. It returns (nil, nil) when no server key pair is configured
// at all, which is a valid state (a forwarded-only deployment, or dev), and the caller decides
// whether authentication is possible. A server certificate without its key, or without the client
// CA it verifies devices against, is a startup error rather than a handshake failure on the first
// device request.
func loadTLSMaterial(o options) (*tlsMaterial, error) {
	var certPEM, keyPEM []byte
	source := ""
	switch {
	case o.tlsCert != "" || o.tlsKey != "":
		if o.tlsCert == "" || o.tlsKey == "" {
			return nil, errors.New("TLS server material is incomplete: --tls-cert and --tls-key are both required")
		}
		cert, err := os.ReadFile(o.tlsCert)
		if err != nil {
			return nil, fmt.Errorf("read server certificate %s: %w", o.tlsCert, err)
		}
		key, err := os.ReadFile(o.tlsKey)
		if err != nil {
			return nil, fmt.Errorf("read server private key %s: %w", o.tlsKey, err)
		}
		certPEM, keyPEM, source = cert, key, "files"
	default:
		envCert, envKey := os.Getenv(EnvTLSCertPEM), os.Getenv(EnvTLSKeyPEM)
		if strings.TrimSpace(envCert) == "" && strings.TrimSpace(envKey) == "" {
			return nil, nil
		}
		if strings.TrimSpace(envCert) == "" || strings.TrimSpace(envKey) == "" {
			return nil, fmt.Errorf(
				"TLS server key pair is incomplete in the environment: set both %s and %s, or neither "+
					"(a deployment injects them from Key Vault; a laptop supplies files with --tls-cert/--tls-key)",
				EnvTLSCertPEM, EnvTLSKeyPEM)
		}
		certPEM, keyPEM, source = []byte(envCert), []byte(envKey), "environment (Key Vault secret reference)"
	}

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("parse the server key pair: %w", err)
	}
	pool, err := loadClientCAPool(o)
	if err != nil {
		return nil, err
	}
	if pool == nil {
		return nil, fmt.Errorf("TLS server material requires the device CA bundle: --tls-client-ca or %s", EnvTLSClientCAPEM)
	}
	return &tlsMaterial{cert: cert, clientCAs: pool, source: source}, nil
}

// serverTLSConfig is the §2.1 configuration: TLS 1.3 minimum, mutual authentication required. Go
// negotiates neither 0-RTT nor renegotiation, which is the rest of §2.1's refusal list.
func (m *tlsMaterial) serverTLSConfig() *tls.Config {
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{m.cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    m.clientCAs,
	}
}
