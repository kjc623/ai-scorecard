// Command edge is the local simulation of the Azure Application Gateway that fronts the device
// transport (ADR 0020 decision 1, decision 5). It is deliberately a faithful stand-in, not a
// second authentication path:
//
//   - It terminates TLS 1.3 and requests a client certificate without requiring one
//     (tls.RequestClientCert), exactly as the gateway's passthrough listener does, so a device
//     with a certificate presents it and a DPoP device need not.
//   - For every request with a peer certificate it sets X-Client-Cert to the presented chain,
//     PEM-concatenated (leaf first), which is the form ingestion/ingest-api/internal/auth's
//     parseCertificateChain accepts. It never verifies the chain: the gateway's passthrough mode is
//     a filter, and the origin is the authority (ADR 0019's intent, kept by ADR 0020 decision 2).
//   - It sets X-Forwarded-Proto: https and X-Forwarded-Host so the DPoP `htu` the origin
//     reconstructs is the URL the device signed.
//   - It reverse-proxies by path with net/http/httputil, mapping each device-facing prefix to the
//     owning service. It has no knowledge of tenants, devices or credentials.
//
// The service code behind it is the production code path: nothing here is imported by a service,
// and the services have no "if lab" branch. See localdev/README.md.
package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"
)

// clientCertHeader is the edge-forwarded certificate header. It is the same name
// endpoint/protocol.HeaderClientCert declares ("X-Client-Cert"); it is repeated as a constant here
// so the edge is a standalone program with no module dependency, which is what lets it be a tiny
// image. A drift between the two spellings is caught by the cross-check in
// localdev/tools/check-config-agreement.mjs reading this file.
const clientCertHeader = "X-Client-Cert"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "edge:", err)
		os.Exit(1)
	}
}

type options struct {
	addr          string
	healthAddr    string
	tlsCert       string
	tlsKey        string
	clientCA      string
	controlURL    string
	ingestURL     string
	contentURL    string
	shutdownGrace time.Duration
}

// route maps a public path prefix to the service that owns it. The device prefixes mirror
// docs/02-ingest-and-transport.md §5: enrolment and tokens are control-api's, events are
// ingest-api's, and the policy/health/grant surface is control-api's; the identity prefixes (SCIM,
// onboarding, the issuer's well-known documents) are control-api's too. It is a table rather than a
// hand-written mux so a reader can see the whole mapping at once.
type route struct {
	prefix string
	target *url.URL
}

func run() error {
	var o options
	flag.StringVar(&o.addr, "addr", "0.0.0.0:8443", "TLS listen address")
	flag.StringVar(&o.healthAddr, "health-addr", "0.0.0.0:8081",
		"plain-HTTP health address, so a container healthcheck needs no client certificate")
	flag.StringVar(&o.tlsCert, "tls-cert", "", "server certificate chain (PEM)")
	flag.StringVar(&o.tlsKey, "tls-key", "", "server private key (PEM)")
	flag.StringVar(&o.clientCA, "client-ca", "", "CA chain sent in the TLS certificate request and forwarded as the trust hint (PEM)")
	flag.StringVar(&o.controlURL, "control-url", "http://control-api:8080", "control-api base URL")
	flag.StringVar(&o.ingestURL, "ingest-url", "http://ingest-api:8080", "ingest-api base URL")
	flag.StringVar(&o.contentURL, "content-url", "",
		"ciphertext storage base URL; a granted content upload is forwarded to it. Empty means the lab has no content path and an upload is a 404")
	flag.DurationVar(&o.shutdownGrace, "shutdown-grace", 10*time.Second, "graceful shutdown grace period")
	flag.Parse()

	if o.tlsCert == "" || o.tlsKey == "" {
		return errors.New("TLS server material is required: --tls-cert and --tls-key")
	}
	if o.clientCA == "" {
		return errors.New("--client-ca is required: the gateway must name the CA chain it presents to devices")
	}

	cert, err := tls.LoadX509KeyPair(o.tlsCert, o.tlsKey)
	if err != nil {
		return fmt.Errorf("load server key pair: %w", err)
	}
	caPEM, err := os.ReadFile(o.clientCA)
	if err != nil {
		return fmt.Errorf("read client CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return fmt.Errorf("client CA %s carries no certificates", o.clientCA)
	}

	control, err := url.Parse(o.controlURL)
	if err != nil {
		return fmt.Errorf("parse --control-url: %w", err)
	}
	ingest, err := url.Parse(o.ingestURL)
	if err != nil {
		return fmt.Errorf("parse --ingest-url: %w", err)
	}

	// Longest prefix wins, so a future /v1/token/introspect would not be swallowed by /v1/token.
	routes := []route{
		{prefix: "/v1/enrol", target: control},
		{prefix: "/v1/token", target: control},
		// The signed policy bundle a device fetches after enrolment (docs/02 §5.2), authenticated by
		// its device credential like /v1/health.
		{prefix: "/v1/policy", target: control},
		{prefix: "/v1/health", target: control},
		{prefix: "/v1/content/grant", target: control},
		{prefix: "/v1/events", target: ingest},
		// The lab has one public edge, so it also stands in for the public routes Azure puts on the
		// analyst edge (azure/main.bicep): a customer IdP's SCIM client, the one-time onboarding pages
		// and the product token issuer's discovery and JWKS. Each authenticates on its own -- a SCIM
		// bearer, the invite token, nothing secret -- so the edge only routes them.
		{prefix: "/scim/v2", target: control},
		{prefix: "/onboard", target: control},
		{prefix: "/.well-known", target: control},
	}

	proxies := map[string]*httputil.ReverseProxy{
		control.String(): newProxy(control),
		ingest.String():  newProxy(ingest),
	}
	if o.contentURL != "" {
		// In Azure a granted device writes straight to Blob storage (docs/02 §10.3); the lab has no
		// storage account, so the upload URL control-api issues points back here and the edge
		// forwards it to the storage stand-in. The URL's signature is the credential, checked there.
		contentStore, err := url.Parse(o.contentURL)
		if err != nil {
			return fmt.Errorf("parse --content-url: %w", err)
		}
		routes = append(routes, route{prefix: "/v1/content/upload", target: contentStore})
		proxies[contentStore.String()] = newProxy(contentStore)
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", okHandler)
	mux.HandleFunc("/readyz", okHandler)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		var match *url.URL
		matchLen := -1
		for _, rt := range routes {
			if r.URL.Path != rt.prefix && !strings.HasPrefix(r.URL.Path, rt.prefix+"/") {
				continue
			}
			if len(rt.prefix) > matchLen {
				match, matchLen = rt.target, len(rt.prefix)
			}
		}
		if match == nil {
			http.NotFound(w, r)
			return
		}
		proxies[match.String()].ServeHTTP(w, r)
	})

	srv := &http.Server{
		Addr:    o.addr,
		Handler: mux,
		TLSConfig: &tls.Config{
			MinVersion:   tls.VersionTLS13,
			Certificates: []tls.Certificate{cert},
			// Passthrough: ask for a client certificate, accept a request that omits one, and never
			// verify it here. A gateway that verified would be a second authority; ADR 0020 makes the
			// origin the authority and the gateway a filter.
			ClientAuth: tls.RequestClientCert,
			ClientCAs:  pool,
		},
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       90 * time.Second,
	}

	// The health listener is plain HTTP and serves only liveness/readiness, so a container
	// healthcheck does not need to present a device credential or trust the edge's server key pair.
	healthMux := http.NewServeMux()
	healthMux.HandleFunc("/healthz", okHandler)
	healthMux.HandleFunc("/readyz", okHandler)
	health := &http.Server{Addr: o.healthAddr, Handler: healthMux, ReadHeaderTimeout: 5 * time.Second}

	errCh := make(chan error, 2)
	go func() {
		logger.Info("edge listening", "addr", o.addr, "tls", "1.3", "client_ca", o.clientCA)
		if err := srv.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("tls listener: %w", err)
		}
	}()
	go func() {
		logger.Info("edge health listening", "addr", o.healthAddr)
		if err := health.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("health listener: %w", err)
		}
	}()

	err = <-errCh
	_ = srv.Close()
	_ = health.Close()
	return err
}

func okHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// newProxy builds the reverse proxy for one upstream. The Rewrite function is the whole edge
// contract:
//
//   - the inbound X-Forwarded-* and X-Client-Cert are discarded before anything is set, so a device
//     cannot smuggle a header past the gateway;
//   - X-Forwarded-Proto is forced to https and X-Forwarded-Host to the host the device called, so
//     the origin's DPoP htu reconstruction (requestHTU in the ingest authenticator) matches the URL
//     the device signed;
//   - a presented peer certificate is forwarded as PEM in X-Client-Cert, leaf first.
func newProxy(target *url.URL) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.Header.Del("X-Forwarded-For")
			pr.Out.Header.Del("X-Forwarded-Host")
			pr.Out.Header.Del("X-Forwarded-Proto")
			pr.Out.Header.Del(clientCertHeader)

			pr.SetURL(target)
			pr.Out.Host = pr.In.Host
			pr.SetXForwarded()
			pr.Out.Header.Set("X-Forwarded-Proto", "https")
			pr.Out.Header.Set("X-Forwarded-Host", pr.In.Host)

			if pemChain := peerCertPEM(pr.In); pemChain != "" {
				// A raw PEM contains newlines, which Go's HTTP stack refuses to write in a header.
				// Percent-encode it, exactly as an L7 gateway does (Application Gateway's client
				// certificate variable, AWS ALB's X-Amzn-Mtls-Clientcert); the origin unescapes it.
				pr.Out.Header.Set(clientCertHeader, url.QueryEscape(pemChain))
			}
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			slog.Error("edge: upstream request failed", "path", r.URL.Path, "error", err)
			http.Error(w, "bad gateway", http.StatusBadGateway)
		},
	}
}

// peerCertPEM renders every certificate the client presented as one PEM string, leaf first. It is
// exactly what auth.parseCertificateChain decodes: the first CERTIFICATE block is the leaf and any
// later block is an intermediate.
func peerCertPEM(r *http.Request) string {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return ""
	}
	var b strings.Builder
	for _, cert := range r.TLS.PeerCertificates {
		b.Write(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}))
	}
	return b.String()
}
