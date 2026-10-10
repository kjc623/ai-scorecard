// Package httpapi is control-api's HTTP surface: the device routes, the browser extension routes,
// the admin, identity, onboarding and SCIM handlers mounted beside them, and the platform probes.
//
// Devices authenticate with the certificate control-api issued them. Application Gateway
// terminates the device's TLS connection and forwards the presented certificate in X-Client-Cert
// without validating its chain, so every request that relies on it is verified here against the
// device CA (chain, validity window, clientAuth) and matched to the device's live credential before
// the tenant and device it names are used. The tenant and device always come from the certificate,
// never from a request body.
package httpapi

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/shadow-ai-capture/device/protocol"

	"github.com/shadow-ai-capture/control-api/internal/apierr"
	"github.com/shadow-ai-capture/control-api/internal/content"
	"github.com/shadow-ai-capture/control-api/internal/deploy"
	"github.com/shadow-ai-capture/control-api/internal/deviceca"
	"github.com/shadow-ai-capture/control-api/internal/directoryadmin"
	"github.com/shadow-ai-capture/control-api/internal/enrol"
	"github.com/shadow-ai-capture/control-api/internal/health"
	"github.com/shadow-ai-capture/control-api/internal/policyserve"
	"github.com/shadow-ai-capture/control-api/internal/settings"
	"github.com/shadow-ai-capture/control-api/internal/store"
)

// MaxBodyBytes caps a device JSON request body.
const MaxBodyBytes = 256 << 10

// Server is the HTTP surface. Every field is set in a deployment; a test sets the ones it exercises,
// and a route whose handler is nil is not registered.
type Server struct {
	Store   store.Store
	CA      *deviceca.CA
	Enrol   *enrol.Service
	Health  *health.Service
	Policy  *policyserve.Service
	Content *content.Service

	// Admin is the deployment admin API (/admin/v1/*), authenticated by product access tokens.
	Admin *deploy.Handler
	// Settings is the Settings admin API (/admin/v1/settings*), authenticated the same way.
	Settings *settings.Handler
	// Directory is the directory pull and teams admin API (/admin/v1/directory*, /admin/v1/teams*).
	Directory *directoryadmin.Handler
	// Extensions serves the browser extension's update manifest and CRX.
	Extensions *deploy.Extensions
	// Agents serves enrolled devices the signed agent release and its package.
	Agents *deploy.Agents
	// SCIM is the provisioning endpoint a customer's identity provider calls (/scim/v2).
	SCIM http.Handler
	// Mounts are further handlers by path prefix: the internal sign-in API, the onboarding pages and
	// the token issuer's well-known documents.
	Mounts map[string]http.Handler

	Logger *slog.Logger
	Now    func() time.Time
}

// Handler returns the routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	if s.Enrol != nil {
		mux.HandleFunc("POST /v1/enrol", s.handleEnrol)
	}
	if s.Health != nil {
		mux.HandleFunc("POST /v1/health", s.handleHealth)
	}
	if s.Policy != nil {
		mux.Handle("/v1/policy", s.Policy.Handler(func(r *http.Request) (string, string, error) {
			cur, err := s.authenticateDevice(r)
			return cur.TenantID, cur.DeviceID, err
		}))
	}
	if s.Content != nil {
		mux.HandleFunc("POST /v1/content/grant", s.handleContentGrant)
		mux.HandleFunc("POST "+protocol.ContentUploadPath, s.handleContentUpload)
	}
	if s.Extensions != nil {
		s.Extensions.Register(mux)
	}
	if s.Agents != nil {
		s.Agents.Register(mux, func(r *http.Request) (string, string, error) {
			cur, err := s.authenticateDevice(r)
			return cur.TenantID, cur.DeviceID, err
		})
	}
	if s.Admin != nil {
		s.Admin.Register(mux)
	}
	if s.Settings != nil {
		s.Settings.Register(mux)
	}
	if s.Directory != nil {
		s.Directory.Register(mux)
	}
	if s.SCIM != nil {
		mux.Handle("/scim/v2", s.SCIM)
		mux.Handle("/scim/v2/", s.SCIM)
	}
	for prefix, h := range s.Mounts {
		mux.Handle(prefix, h)
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		s.writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", s.handleReady)
	return mux
}

// handleReady answers readiness with a database round trip. The reason for a failure is logged,
// not returned: it can carry a host name.
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if s.Store == nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not-ready"})
		return
	}
	if err := s.Store.Ping(ctx); err != nil {
		s.Logger.Warn("readiness probe failed", "error", err)
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not-ready"})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (s *Server) handleEnrol(w http.ResponseWriter, r *http.Request) {
	var req protocol.EnrolmentRequest
	if !s.decode(w, r, &req) {
		return
	}
	resp, err := s.Enrol.Enrol(r.Context(), enrol.Input{
		Request: req,
		Current: func() (enrol.Current, error) { return s.authenticateDevice(r) },
	})
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.Logger.Info("control: device enrolled", "tenant", resp.TenantID, "device", resp.DeviceID, "reenrolled", resp.Reenrolled)
	s.writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	var req protocol.HealthRequest
	if !s.decode(w, r, &req) {
		return
	}
	cur, err := s.authenticateDevice(r)
	if err != nil {
		s.writeError(w, err)
		return
	}
	resp, err := s.Health.Report(r.Context(), cur.TenantID, cur.DeviceID, req)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleContentGrant(w http.ResponseWriter, r *http.Request) {
	var req protocol.ContentGrantRequest
	if !s.decode(w, r, &req) {
		return
	}
	cur, err := s.authenticateDevice(r)
	if err != nil {
		s.writeError(w, err)
		return
	}
	resp, err := s.Content.Decide(r.Context(), cur.TenantID, cur.DeviceID, req)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.Logger.Info("control: content grant decided", "tenant", cur.TenantID, "device", cur.DeviceID,
		"event", req.EventID, "state", resp.State, "reason", resp.Reason)
	s.writeJSON(w, http.StatusOK, resp)
}

// handleContentUpload is POST /v1/content: the body is the content object itself.
func (s *Server) handleContentUpload(w http.ResponseWriter, r *http.Request) {
	cur, err := s.authenticateDevice(r)
	if err != nil {
		s.writeError(w, err)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, protocol.MaxContentObjectBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			s.writeError(w, apierr.New(http.StatusRequestEntityTooLarge, string(protocol.ReasonOversize),
				"the content object is over the per-object cap"))
			return
		}
		s.writeError(w, apierr.New(http.StatusBadRequest, apierr.CodeInvalidRequest, "the request body could not be read"))
		return
	}
	u := content.Upload{
		GrantID:   r.Header.Get(protocol.HeaderContentGrantID),
		EventID:   r.Header.Get(protocol.HeaderContentEventID),
		RawDigest: r.Header.Get(protocol.HeaderContentRawDigest),
		Body:      body,
	}
	if err := s.Content.Upload(r.Context(), cur.TenantID, cur.DeviceID, u); err != nil {
		s.writeError(w, err)
		return
	}
	s.Logger.Info("control: content uploaded", "tenant", cur.TenantID, "device", cur.DeviceID,
		"event", u.EventID, "grant", u.GrantID, "bytes", len(body))
	s.writeJSON(w, http.StatusOK, map[string]string{"state": "uploaded", "grant_id": u.GrantID, "event_id": u.EventID})
}

// authenticateDevice verifies the forwarded device certificate and resolves the device's live
// credential. The certificate must chain to the device CA, be inside its validity window and carry
// clientAuth; it must name a device and a tenant; and it must be exactly the certificate the
// device's live, unexpired credential was issued for.
func (s *Server) authenticateDevice(r *http.Request) (enrol.Current, error) {
	header := r.Header.Get(protocol.HeaderClientCert)
	if header == "" {
		return enrol.Current{}, apierr.New(http.StatusUnauthorized, apierr.CodeUnauthenticated, "a device certificate is required")
	}
	chain, err := parseChain(header)
	if err != nil {
		return enrol.Current{}, apierr.New(http.StatusUnauthorized, apierr.CodeInvalidClientCert, "the forwarded device certificate cannot be read")
	}
	now := s.now()
	if err := s.CA.Verify(chain, now); err != nil {
		s.Logger.Info("control: device certificate refused", "error", err)
		return enrol.Current{}, apierr.New(http.StatusUnauthorized, apierr.CodeInvalidClientCert,
			"the device certificate does not verify against the device CA")
	}
	leaf := chain[0]
	cur := enrol.Current{DeviceID: leaf.Subject.CommonName, CredentialID: protocol.CredentialID(leaf.Raw)}
	for _, ou := range leaf.Subject.OrganizationalUnit {
		if store.IsUUID(ou) {
			cur.TenantID = ou
			break
		}
	}
	if !store.IsUUID(cur.DeviceID) || cur.TenantID == "" {
		return enrol.Current{}, apierr.New(http.StatusUnauthorized, apierr.CodeInvalidClientCert,
			"the device certificate does not name a device and a tenant")
	}
	cred, err := s.Store.DeviceCredential(r.Context(), cur.TenantID, cur.CredentialID)
	if errors.Is(err, store.ErrCredentialUnknown) {
		return enrol.Current{}, apierr.New(http.StatusUnauthorized, apierr.CodeRevokedDevice, "the device certificate is not a registered credential")
	}
	if err != nil {
		return enrol.Current{}, apierr.Internal(err)
	}
	if cred.DeviceID != cur.DeviceID || cred.PublicKeyThumbprint != enrol.SPKIThumbprint(leaf.RawSubjectPublicKeyInfo) {
		return enrol.Current{}, apierr.New(http.StatusUnauthorized, apierr.CodeRevokedDevice, "the device certificate does not match its credential")
	}
	if err := cred.Active(now); err != nil {
		return enrol.Current{}, apierr.New(http.StatusUnauthorized, apierr.CodeRevokedDevice, "the device credential is revoked or expired")
	}
	return cur, nil
}

// parseChain decodes the forwarded PEM chain, leaf first. The gateway URL-encodes the PEM because a
// header value cannot carry its newlines; both percent encodings are tried (a space as %20 keeps
// base64's '+' intact, a space as '+' needs form decoding), and raw PEM is accepted too.
func parseChain(header string) ([]*x509.Certificate, error) {
	candidates := []string{header}
	if s, err := url.PathUnescape(header); err == nil && s != header {
		candidates = append(candidates, s)
	}
	if s, err := url.QueryUnescape(header); err == nil && s != header {
		candidates = append(candidates, s)
	}
	err := errors.New("the header carries no certificate")
	for _, text := range candidates {
		var chain []*x509.Certificate
		if chain, err = decodePEM(text); err == nil {
			return chain, nil
		}
	}
	return nil, err
}

func decodePEM(text string) ([]*x509.Certificate, error) {
	var chain []*x509.Certificate
	rest := []byte(text)
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, err
		}
		chain = append(chain, cert)
	}
	if len(chain) == 0 {
		return nil, errors.New("the header carries no certificate")
	}
	return chain, nil
}

// decode reads a size-capped JSON body. It answers the caller itself on failure.
func (s *Server) decode(w http.ResponseWriter, r *http.Request, v any) bool {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			s.writeError(w, apierr.New(http.StatusRequestEntityTooLarge, apierr.CodeInvalidRequest, "the request body is over the cap"))
			return false
		}
		s.writeError(w, apierr.New(http.StatusBadRequest, apierr.CodeInvalidRequest, "the request body could not be read"))
		return false
	}
	if err := json.Unmarshal(body, v); err != nil {
		s.writeError(w, apierr.New(http.StatusBadRequest, apierr.CodeSchemaViolation, "the request body is not valid JSON"))
		return false
	}
	return true
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Server) writeError(w http.ResponseWriter, err error) {
	apierr.Write(w, err, s.now(), s.Logger)
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, v any) {
	apierr.WriteJSON(w, status, v, s.Logger)
}
