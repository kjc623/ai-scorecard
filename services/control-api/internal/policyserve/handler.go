package policyserve

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/shadow-ai-capture/device/protocol"

	"github.com/shadow-ai-capture/control-api/internal/apierr"
)

// DeviceAuthenticator resolves the device a request authenticates as, from its verified certificate.
// The tenant and device come from the certificate, never from the request.
type DeviceAuthenticator func(r *http.Request) (tenantID, deviceID string, err error)

// Handler serves GET /v1/policy. HEAD answers the same status and headers without a body.
func (s *Service) Handler(auth DeviceAuthenticator) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		now := s.cfg.Now().UTC()
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			apierr.Write(w, apierr.New(http.StatusMethodNotAllowed, apierr.CodeMethodNotAllowed, "GET is required"), now, s.cfg.Logger)
			return
		}
		tenantID, deviceID, err := auth(r)
		if err != nil {
			apierr.Write(w, err, now, s.cfg.Logger)
			return
		}
		sv, err := s.Current(r.Context(), tenantID)
		if err != nil {
			apierr.Write(w, err, now, s.cfg.Logger)
			return
		}
		w.Header().Set(protocol.HeaderETag, sv.ETag)
		w.Header().Set("Cache-Control", "no-cache")
		if protocol.ETagMatches(r.Header.Get(protocol.HeaderIfNoneMatch), sv.ETag) {
			w.WriteHeader(http.StatusNotModified)
			s.cfg.Logger.Info("control: policy unchanged", "tenant", tenantID, "device", deviceID, "version", sv.Version)
			return
		}
		// Escaping is off so the signed envelope's bytes pass through the encoder unchanged even if
		// they were ever stored unescaped; the stored form is json.Marshal's, so this is belt and
		// braces rather than a repair.
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(protocol.PolicyResponse{
			SchemaVersion: protocol.PolicySchemaVersion,
			BundleVersion: sv.Version,
			SignedBundle:  sv.Envelope,
			ServerTime:    now,
		}); err != nil {
			apierr.Write(w, apierr.Internal(err), now, s.cfg.Logger)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = w.Write(buf.Bytes())
		}
		s.cfg.Logger.Info("control: policy served", "tenant", tenantID, "device", deviceID, "version", sv.Version)
	})
}
