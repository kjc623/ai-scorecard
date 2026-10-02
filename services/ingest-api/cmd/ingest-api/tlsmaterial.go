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

// pemFromEnv returns the three environment values, so the caller can decide whether a deployment is
// using this form at all. A partial set is an error rather than a silent mix with the file form: a
// server certificate without its key cannot serve, and would otherwise surface as a TLS handshake
// failure on the first device request.
func pemFromEnv() (cert, key, ca string, err error) {
	cert, key, ca = os.Getenv(EnvTLSCertPEM), os.Getenv(EnvTLSKeyPEM), os.Getenv(EnvTLSClientCAPEM)
	set := 0
	for _, v := range []string{cert, key, ca} {
		if strings.TrimSpace(v) != "" {
			set++
		}
	}
	switch set {
	case 0, 3:
		return cert, key, ca, nil
	default:
		return "", "", "", fmt.Errorf(
			"TLS material is incomplete in the environment: set all of %s, %s and %s, or none of them "+
				"(a deployment injects all three from Key Vault; a laptop supplies files with "+
				"--tls-cert/--tls-key/--tls-client-ca)",
			EnvTLSCertPEM, EnvTLSKeyPEM, EnvTLSClientCAPEM)
	}
}

// loadTLSMaterial resolves the material from the file flags first and the environment second.
// It returns (nil, nil) when no material is configured at all, which is a valid state: the binary
// then refuses to serve unless the dev principal is enabled, and the refusal is the caller's.
func loadTLSMaterial(o options) (*tlsMaterial, error) {
	if o.tlsCert != "" || o.tlsKey != "" || o.tlsClientCA != "" {
		if o.tlsCert == "" || o.tlsKey == "" || o.tlsClientCA == "" {
			return nil, errors.New("TLS material is incomplete: --tls-cert, --tls-key and --tls-client-ca are all required")
		}
		cert, err := tls.LoadX509KeyPair(o.tlsCert, o.tlsKey)
		if err != nil {
			return nil, fmt.Errorf("load the server key pair: %w", err)
		}
		pool, err := loadCAPool(o.tlsClientCA)
		if err != nil {
			return nil, err
		}
		return &tlsMaterial{cert: cert, clientCAs: pool, source: "files"}, nil
	}

	certPEM, keyPEM, caPEM, err := pemFromEnv()
	if err != nil {
		return nil, err
	}
	if certPEM == "" {
		return nil, nil
	}
	cert, err := tls.X509KeyPair([]byte(certPEM), []byte(keyPEM))
	if err != nil {
		return nil, fmt.Errorf("parse %s/%s: %w", EnvTLSCertPEM, EnvTLSKeyPEM, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(caPEM)) {
		return nil, fmt.Errorf("%s contains no certificates", EnvTLSClientCAPEM)
	}
	return &tlsMaterial{cert: cert, clientCAs: pool, source: "environment (Key Vault secret reference)"}, nil
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
