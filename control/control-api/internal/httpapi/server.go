// Package httpapi is the transport for control-api's two device-facing endpoints,
// POST /v1/enrol and POST /v1/token (docs/02-ingest-and-transport.md §5.1, §5.2).
//
// It owns exactly three things: reading the bytes (a size cap), turning an authenticated request into
// a call to a service, and rendering errors as §5's common envelope:
//
//	{ "error": { "code": "...", "detail": { ... }, "server_time": "..." } }
//
// All decisions and codes come from the services, so the transport cannot invent an outcome. The one
// thing it does own is resolving the *current credential* a rotation re-enrolment presents (a
// forwarded certificate, or an access token plus DPoP proof), because that is a property of the HTTP
// request rather than of the enrolment domain.
package httpapi

import (
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/shadow-ai-capture/device/protocol"

	"github.com/shadow-ai-capture/control-api/internal/apierr"
	"github.com/shadow-ai-capture/control-api/internal/content"
	"github.com/shadow-ai-capture/control-api/internal/dpop"
	"github.com/shadow-ai-capture/control-api/internal/enrol"
	"github.com/shadow-ai-capture/control-api/internal/store"
	"github.com/shadow-ai-capture/control-api/internal/token"
)

// MaxBodyBytes caps a device request body. Both endpoints carry a small JSON document -- a CSR or a
// JWK is the largest member -- so 256 KiB is generous and bounds a hostile body before decoding.
const MaxBodyBytes = 256 << 10

// Server is the HTTP surface.
type Server struct {
	Enrol    *enrol.Service
	Token    *token.Service
	Verifier *token.Verifier
	Store    store.Store
	Logger   *slog.Logger
	Now      func() time.Time

	// Content is the grant path (§5.5, §10). Nil when the deployment has no content vault to ask:
	// the routes then answer 503 rather than deciding a grant nothing could honour.
	Content *content.Service
}

// New builds a server.
func New(enrolSvc *enrol.Service, tokenSvc *token.Service, verifier *token.Verifier, st store.Store, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{Enrol: enrolSvc, Token: tokenSvc, Verifier: verifier, Store: st, Logger: logger, Now: time.Now}
}

// Handler returns the routes. /healthz is deployment infrastructure, not a device API (§5).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/enrol", s.handleEnrol)
	mux.HandleFunc("/v1/token", s.handleToken)
	mux.HandleFunc("/v1/content/grant", s.handleContentGrant)
	// Not a device route: the edge does not forward it. The storage layer calls it when an upload
	// lands, and authenticates with the upload signing key.
	mux.HandleFunc("/internal/v1/content/finalise", s.handleContentFinalise)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	})
	return mux
}

func (s *Server) handleEnrol(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		s.writeError(w, apierr.New(http.StatusMethodNotAllowed, apierr.CodeSchemaViolation, "POST is required"))
		return
	}
	body, ok := s.readBody(w, r)
	if !ok {
		return
	}
	var req protocol.EnrolmentRequest
	if err := json.Unmarshal(body, &req); err != nil {
		s.writeError(w, apierr.New(400, apierr.CodeSchemaViolation, "the enrolment body is not valid JSON"))
		return
	}

	in := enrol.Input{
		Request: req,
		Proof:   r.Header.Get(protocol.HeaderDPoP),
		HTM:     r.Method,
		HTU:     dpop.HTU(r),
	}
	if req.EnrolmentToken == "" {
		// The resolver runs only after the service has validated the body, so a malformed body is a
		// 400 even when no usable credential is presented.
		in.ResolveCurrent = func() (*enrol.Current, error) { return s.resolveCurrent(r) }
	}

	resp, err := s.Enrol.Enrol(r.Context(), in)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		s.writeError(w, apierr.New(http.StatusMethodNotAllowed, apierr.CodeInvalidRequest, "POST is required"))
		return
	}
	body, ok := s.readBody(w, r)
	if !ok {
		return
	}
	var req protocol.TokenRequest
	if err := json.Unmarshal(body, &req); err != nil {
		s.writeError(w, apierr.New(400, apierr.CodeInvalidRequest, "the token body is not valid JSON"))
		return
	}
	proof := r.Header.Get(protocol.HeaderDPoP)
	if proof == "" {
		s.writeError(w, apierr.New(401, apierr.CodeInvalidProof, "a DPoP proof is required"))
		return
	}
	resp, err := s.Token.Issue(r.Context(), req, proof, r.Method, dpop.HTU(r))
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, resp)
}

// handleContentGrant is POST /v1/content/grant (§5.5). The device authenticates with its current
// credential; the tenant and device come from that credential, never from the body.
func (s *Server) handleContentGrant(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		s.writeError(w, apierr.New(http.StatusMethodNotAllowed, apierr.CodeInvalidRequest, "POST is required"))
		return
	}
	if s.Content == nil {
		s.writeError(w, apierr.New(http.StatusServiceUnavailable, apierr.CodeUnavailable, "content grants are not configured on this deployment"))
		return
	}
	body, ok := s.readBody(w, r)
	if !ok {
		return
	}
	var req protocol.ContentGrantRequest
	if err := json.Unmarshal(body, &req); err != nil {
		s.writeError(w, apierr.New(400, apierr.CodeSchemaViolation, "the grant body is not valid JSON"))
		return
	}
	cur, err := s.resolveCurrent(r)
	if err != nil {
		s.writeError(w, err)
		return
	}
	resp, err := s.Content.Decide(r.Context(), cur.TenantID, cur.DeviceID, req, publicBase(r))
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.Logger.Info("control: content grant decided", "tenant", cur.TenantID, "device", cur.DeviceID,
		"event", req.EventID, "state", resp.State, "reason", resp.Reason)
	s.writeJSON(w, http.StatusOK, resp)
}

// handleContentFinalise is the finaliser's entry point (§10.4): the storage layer reports an
// upload, and the object is promoted only if it matches a live grant and its declared digest. A
// rejection is a 422, which tells the storage layer to delete the staged bytes.
func (s *Server) handleContentFinalise(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		s.writeError(w, apierr.New(http.StatusMethodNotAllowed, apierr.CodeInvalidRequest, "POST is required"))
		return
	}
	if s.Content == nil {
		s.writeError(w, apierr.New(http.StatusServiceUnavailable, apierr.CodeUnavailable, "content grants are not configured on this deployment"))
		return
	}
	body, ok := s.readBody(w, r)
	if !ok {
		return
	}
	if !s.Content.VerifyBody(body, r.Header.Get("X-Sac-Upload-Signature")) {
		s.writeError(w, apierr.New(http.StatusUnauthorized, apierr.CodeInvalidRequest, "the finalise call is not signed by the storage layer"))
		return
	}
	var report content.UploadReport
	if err := json.Unmarshal(body, &report); err != nil {
		s.writeError(w, apierr.New(400, apierr.CodeSchemaViolation, "the finalise body is not valid JSON"))
		return
	}
	submissionID, err := s.Content.Finalise(r.Context(), report)
	if err != nil {
		if errors.Is(err, content.ErrUploadRejected) {
			s.Logger.Warn("control: upload rejected", "grant", report.GrantID, "object", report.ObjectID, "error", err)
			s.writeError(w, apierr.New(http.StatusUnprocessableEntity, apierr.CodeInvalidRequest, "the upload does not match a live grant"))
			return
		}
		s.writeError(w, apierr.Internal(err))
		return
	}
	s.Logger.Info("control: content object stored", "tenant", report.TenantID, "event", report.EventID,
		"object", report.ObjectID, "bytes", report.SizeBytes)
	s.writeJSON(w, http.StatusOK, map[string]string{"state": "uploaded", "object_id": report.ObjectID, "submission_id": submissionID})
}

// publicBase is the scheme and host the device reached this service on, as the edge reports it.
func publicBase(r *http.Request) string {
	scheme := r.Header.Get("X-Forwarded-Proto")
	if scheme == "" {
		scheme = "https"
		if r.TLS == nil {
			scheme = "http"
		}
	}
	host := r.Header.Get("X-Forwarded-Host")
	if host == "" {
		host = r.Host
	}
	return scheme + "://" + host
}

// readBody enforces the size cap and returns the bytes. It answers the caller itself on failure.
func (s *Server) readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, MaxBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			s.writeError(w, apierr.New(http.StatusRequestEntityTooLarge, apierr.CodeInvalidRequest, "the request body is over the cap"))
			return nil, false
		}
		s.Logger.Warn("control: reading the request body failed", "error", err, "remote", r.RemoteAddr)
		s.writeError(w, apierr.New(400, apierr.CodeInvalidRequest, "the request body could not be read"))
		return nil, false
	}
	return body, true
}

// resolveCurrent authenticates a rotation re-enrolment that presents its existing credential instead
// of a bootstrap token. Two forms, matching ADR 0020 decision 2:
//
//   - x509: the leaf in the TLS connection (a direct-TLS listener) or forwarded as X-Client-Cert by
//     the edge. Its SPKI thumbprint must equal the credential's registered value.
//   - dpop: a short-lived access token this service issued, presented as `Authorization: DPoP ...`
//     with a fresh DPoP proof whose `ath` binds it to the token and whose key matches `cnf.jkt`.
func (s *Server) resolveCurrent(r *http.Request) (*enrol.Current, error) {
	if cert := s.clientCertificate(r); cert != nil {
		return s.resolveCertCurrent(r, cert)
	}
	auth := r.Header.Get(protocol.HeaderAuthorization)
	if strings.HasPrefix(strings.ToLower(auth), "dpop ") {
		return s.resolveDPoPCurrent(r, strings.TrimSpace(auth[len("DPoP "):]))
	}
	return nil, apierr.New(401, apierr.CodeRevokedDevice,
		"a re-enrolment must present an enrolment token or the current device credential")
}

func (s *Server) clientCertificate(r *http.Request) *x509.Certificate {
	if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
		return r.TLS.PeerCertificates[0]
	}
	header := r.Header.Get(protocol.HeaderClientCert)
	if header == "" {
		return nil
	}
	// The edge percent-encodes the PEM so it survives as one header value; a gateway that forwards
	// it raw is accepted as it is.
	if unescaped, err := url.QueryUnescape(header); err == nil && strings.Contains(unescaped, "-----BEGIN") {
		header = unescaped
	}
	// The edge forwards the certificate as PEM; the first CERTIFICATE block is the leaf.
	for {
		block, rest := pem.Decode([]byte(header))
		if block == nil {
			return nil
		}
		if block.Type == "CERTIFICATE" {
			cert, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				return nil
			}
			return cert
		}
		header = string(rest)
	}
}

func (s *Server) resolveCertCurrent(r *http.Request, leaf *x509.Certificate) (*enrol.Current, error) {
	deviceID := leaf.Subject.CommonName
	if !store.IsUUID(deviceID) {
		return nil, apierr.New(401, apierr.CodeRevokedDevice, "the certificate subject CN is not a device uuid")
	}
	var tenantID string
	for _, ou := range leaf.Subject.OrganizationalUnit {
		if store.IsUUID(ou) {
			tenantID = ou
			break
		}
	}
	if tenantID == "" {
		return nil, apierr.New(401, apierr.CodeRevokedDevice, "the certificate carries no tenant organisational unit")
	}
	cred, err := s.Store.DeviceCredentialByDevice(r.Context(), tenantID, deviceID)
	if errors.Is(err, store.ErrCredentialUnknown) {
		return nil, apierr.New(401, apierr.CodeRevokedDevice, "the device has no live credential")
	}
	if err != nil {
		return nil, apierr.Internal(err)
	}
	if cred.Type != protocol.AuthModeX509 {
		return nil, apierr.New(401, apierr.CodeRevokedDevice, "the live credential is not an x509 credential")
	}
	sum := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
	presented := base64.RawURLEncoding.EncodeToString(sum[:])
	if subtle.ConstantTimeCompare([]byte(presented), []byte(cred.PublicKeyThumbprint)) != 1 {
		return nil, apierr.New(401, apierr.CodeRevokedDevice, "the certificate does not match the registered device credential")
	}
	if err := cred.Active(s.now()); err != nil {
		return nil, apierr.New(401, apierr.CodeRevokedDevice, "the current device credential is revoked or expired")
	}
	return &enrol.Current{TenantID: tenantID, DeviceID: deviceID, CredentialID: cred.CredentialID, KeyThumbprint: presented}, nil
}

func (s *Server) resolveDPoPCurrent(r *http.Request, accessToken string) (*enrol.Current, error) {
	if s.Verifier == nil {
		return nil, apierr.New(401, apierr.CodeRevokedDevice, "access-token authentication is not configured")
	}
	verified, err := s.Verifier.Verify(accessToken)
	if err != nil {
		return nil, apierr.New(401, apierr.CodeRevokedDevice, "the presented access token did not verify")
	}
	proofCompact := r.Header.Get(protocol.HeaderDPoP)
	if proofCompact == "" {
		return nil, apierr.New(401, apierr.CodeInvalidProof, "a DPoP proof is required with the access token")
	}
	proof, err := dpop.Verify(proofCompact, r.Method, dpop.HTU(r), accessToken, s.now(), dpop.DefaultSkew)
	if err != nil {
		return nil, apierr.New(401, apierr.CodeInvalidProof, "the DPoP proof did not verify")
	}
	if subtle.ConstantTimeCompare([]byte(proof.Thumbprint), []byte(verified.JKT)) != 1 {
		return nil, apierr.New(401, apierr.CodeInvalidProof, "the DPoP proof key is not the token's bound key")
	}
	cred, err := s.Store.DeviceCredential(r.Context(), verified.TenantID, verified.CredentialID)
	if errors.Is(err, store.ErrCredentialUnknown) {
		return nil, apierr.New(401, apierr.CodeRevokedDevice, "the credential the token names is gone")
	}
	if err != nil {
		return nil, apierr.Internal(err)
	}
	if err := cred.Active(s.now()); err != nil {
		return nil, apierr.New(401, apierr.CodeRevokedDevice, "the current device credential is revoked or expired")
	}
	return &enrol.Current{
		TenantID: verified.TenantID, DeviceID: verified.DeviceID,
		CredentialID: verified.CredentialID, KeyThumbprint: verified.JKT,
	}, nil
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// errorEnvelope is §5's common error body.
type errorEnvelope struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code        string    `json:"code"`
	Detail      any       `json:"detail,omitempty"`
	ServerTime  time.Time `json:"server_time"`
	RetryAfterS int       `json:"retry_after_s,omitempty"`
	Message     string    `json:"message,omitempty"`
}

func (s *Server) writeError(w http.ResponseWriter, err error) {
	var apiErr *apierr.Error
	if !errors.As(err, &apiErr) {
		s.Logger.Error("control: unclassified failure", "error", err)
		apiErr = apierr.Internal(err)
	}
	if apiErr.Cause != nil {
		s.Logger.Error("control: request failed with an internal cause", "error", apiErr.Cause, "code", apiErr.Code)
	}
	body := errorEnvelope{Error: errorBody{
		Code: apiErr.Code, Detail: apiErr.Detail, ServerTime: s.now(), Message: apiErr.Message,
	}}
	if apiErr.Status == http.StatusTooManyRequests || apiErr.Status == http.StatusServiceUnavailable {
		body.Error.RetryAfterS = 5
		w.Header().Set("Retry-After", "5")
	}
	s.writeJSON(w, apiErr.Status, body)
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		s.Logger.Error("control: write response", "error", err)
	}
}
