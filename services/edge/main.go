// Command edge is the device entry point where the deployment has no Application Gateway
// (azure/modules/application-gateway.bicep), and it behaves as the gateway does:
//
//   - TLS with a client certificate requested but neither required nor verified (passthrough): a
//     device's first enrolment presents none, and the origins verify the ones that are presented
//     against the device CA.
//   - Only the device API is reachable. A path outside the allow-list is refused with 403, as the
//     gateway's firewall rule refuses it; an allowed prefix that no route serves gets 502, as the
//     gateway's empty default pool answers. Browser downloads (the extension's update manifest and
//     CRX) are not served here: a browser's extension downloader cannot answer the client
//     certificate request, so the dashboard serves them on the analyst hostname.
//   - The presented leaf certificate is forwarded URL-encoded in X-Client-Cert, with
//     X-Forwarded-Proto and X-Forwarded-Host. A client-supplied copy of any of them is discarded.
//
// Configuration:
//
//	EDGE_ADDR          listen address (default 0.0.0.0:8443)
//	EDGE_TLS_CERT_PEM  server certificate chain, PEM
//	EDGE_TLS_KEY_PEM   server private key, PEM
//	EDGE_INGEST_URL    ingest-api (default http://ingest-api:8080)
//	EDGE_CONTROL_URL   control-api (default http://control-api:8080)
package main

import (
	"crypto/tls"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"
)

const clientCertHeader = "X-Client-Cert"

// Server limits. A request is read, and its answer written, within the gateway's 60 second request
// timeout; a kept-alive connection idles at most idleTimeout; a device's headers are a few hundred
// bytes, far below maxHeaderBytes.
const (
	readHeaderTimeout = 10 * time.Second
	readTimeout       = 60 * time.Second
	writeTimeout      = 60 * time.Second
	idleTimeout       = 90 * time.Second
	maxHeaderBytes    = 32 << 10
)

// allowedPrefixes is the firewall's allow-list: a request URI that begins with none of them
// (compared in lower case) is refused before routing.
var allowedPrefixes = []string{"/v1/events", "/v1/enrol", "/v1/policy", "/v1/health", "/v1/content"}

// Upstreams a path can route to.
const (
	upstreamIngest  = "ingest"
	upstreamControl = "control"
)

// route returns the upstream that serves path, or the status the gateway answers with itself.
func route(path string) (upstream string, status int) {
	lower := strings.ToLower(path)
	allowed := false
	for _, p := range allowedPrefixes {
		if strings.HasPrefix(lower, p) {
			allowed = true
			break
		}
	}
	if !allowed {
		return "", http.StatusForbidden
	}
	switch path {
	case "/v1/events":
		return upstreamIngest, 0
	case "/v1/enrol", "/v1/policy", "/v1/health", "/v1/content/grant", "/v1/content":
		return upstreamControl, 0
	}
	return "", http.StatusBadGateway
}

type config struct {
	addr     string
	certPEM  string
	keyPEM   string
	upstream map[string]*url.URL
}

func configFromEnv(getenv func(string) string) (config, error) {
	get := func(name, fallback string) string {
		if v := strings.TrimSpace(getenv(name)); v != "" {
			return v
		}
		return fallback
	}
	c := config{
		addr:     get("EDGE_ADDR", "0.0.0.0:8443"),
		certPEM:  getenv("EDGE_TLS_CERT_PEM"),
		keyPEM:   getenv("EDGE_TLS_KEY_PEM"),
		upstream: map[string]*url.URL{},
	}
	var errs []error
	if strings.TrimSpace(c.certPEM) == "" || strings.TrimSpace(c.keyPEM) == "" {
		errs = append(errs, errors.New("EDGE_TLS_CERT_PEM and EDGE_TLS_KEY_PEM are required"))
	}
	for name, raw := range map[string]string{
		upstreamIngest:  get("EDGE_INGEST_URL", "http://ingest-api:8080"),
		upstreamControl: get("EDGE_CONTROL_URL", "http://control-api:8080"),
	} {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			errs = append(errs, fmt.Errorf("the %s upstream %q is not an absolute http(s) URL", name, raw))
			continue
		}
		c.upstream[name] = u
	}
	return c, errors.Join(errs...)
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(log)
	if err := run(log); err != nil {
		fmt.Fprintln(os.Stderr, "edge:", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := configFromEnv(os.Getenv)
	if err != nil {
		return err
	}
	cert, err := tls.X509KeyPair([]byte(cfg.certPEM), []byte(cfg.keyPEM))
	if err != nil {
		return fmt.Errorf("server key pair: %w", err)
	}
	srv := &http.Server{
		Addr:    cfg.addr,
		Handler: newHandler(cfg.upstream),
		TLSConfig: &tls.Config{
			MinVersion:   tls.VersionTLS12,
			Certificates: []tls.Certificate{cert},
			ClientAuth:   tls.RequestClientCert,
		},
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
	}
	log.Info("edge listening", "addr", cfg.addr, "ingest", cfg.upstream[upstreamIngest].String(), "control", cfg.upstream[upstreamControl].String())
	return srv.ListenAndServeTLS("", "")
}

// newHandler routes each request by path to its upstream's reverse proxy.
func newHandler(upstreams map[string]*url.URL) http.Handler {
	proxies := map[string]*httputil.ReverseProxy{}
	for name, u := range upstreams {
		proxies[name] = newProxy(u)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name, status := route(r.URL.Path)
		if status != 0 {
			http.Error(w, http.StatusText(status), status)
			return
		}
		proxies[name].ServeHTTP(w, r)
	})
}

// newProxy forwards to one upstream, replacing the forwarding headers with the gateway's own.
func newProxy(target *url.URL) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.Header.Del("X-Forwarded-For")
			pr.Out.Header.Del("X-Forwarded-Host")
			pr.Out.Header.Del("X-Forwarded-Proto")
			pr.Out.Header.Del(clientCertHeader)
			pr.SetURL(target)
			pr.SetXForwarded()
			pr.Out.Header.Set("X-Forwarded-Proto", "https")
			if leaf := leafPEM(pr.In); leaf != "" {
				// A header cannot carry the PEM's newlines, so it travels percent-encoded.
				pr.Out.Header.Set(clientCertHeader, url.PathEscape(leaf))
			}
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			slog.Error("upstream request failed", "path", r.URL.Path, "error", err.Error())
			http.Error(w, http.StatusText(http.StatusBadGateway), http.StatusBadGateway)
		},
	}
}

// leafPEM is the client's leaf certificate as PEM, or "" when it presented none.
func leafPEM(r *http.Request) string {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return ""
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: r.TLS.PeerCertificates[0].Raw}))
}
