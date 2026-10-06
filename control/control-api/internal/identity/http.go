package identity

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// The internal sign-in API. Its only caller is the dashboard server, which authenticates every
// request with `Authorization: Bearer <SAC_INTERNAL_TOKEN>`: a shared secret held in Key Vault and
// delivered to both services, compared here in constant time. A request without it is refused
// whichever route or edge it arrived through.
const (
	PathBegin    = "/internal/v1/auth/begin"
	PathComplete = "/internal/v1/auth/complete"
	PathToken    = "/internal/v1/auth/token"
	PathRevoke   = "/internal/v1/auth/revoke"

	maxInternalBody = 64 << 10
	// MinInternalTokenLen refuses a guessable shared secret at startup.
	MinInternalTokenLen = 32
)

// InternalHandler serves the four routes behind the internal-token check.
func (s *Service) InternalHandler(internalToken string) (http.Handler, error) {
	if len(internalToken) < MinInternalTokenLen {
		return nil, fmt.Errorf("identity: SAC_INTERNAL_TOKEN must be at least %d characters", MinInternalTokenLen)
	}
	want := sha256.Sum256([]byte(internalToken))
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+PathBegin, func(w http.ResponseWriter, r *http.Request) {
		var req BeginRequest
		if !decode(w, r, &req) {
			return
		}
		res, err := s.Begin(r.Context(), req)
		s.answer(w, http.StatusOK, res, err)
	})
	mux.HandleFunc("POST "+PathComplete, func(w http.ResponseWriter, r *http.Request) {
		var req CompleteRequest
		if !decode(w, r, &req) {
			return
		}
		res, err := s.Complete(r.Context(), req)
		s.answer(w, http.StatusOK, res, err)
	})
	mux.HandleFunc("POST "+PathToken, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Session string `json:"session"`
		}
		if !decode(w, r, &req) {
			return
		}
		res, err := s.Token(r.Context(), req.Session)
		s.answer(w, http.StatusOK, res, err)
	})
	mux.HandleFunc("POST "+PathRevoke, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Session string `json:"session"`
		}
		if !decode(w, r, &req) {
			return
		}
		s.answer(w, http.StatusNoContent, nil, s.Revoke(r.Context(), req.Session))
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		var got [32]byte
		if len(auth) > 7 && strings.EqualFold(auth[:7], "bearer ") {
			got = sha256.Sum256([]byte(strings.TrimSpace(auth[7:])))
		}
		// Both sides are hashed first, so the comparison is constant-time whatever the lengths.
		if subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		mux.ServeHTTP(w, r)
	}), nil
}

func decode(w http.ResponseWriter, r *http.Request, into any) bool {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxInternalBody))
	if err != nil || json.Unmarshal(body, into) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": CodeBadRequest})
		return false
	}
	return true
}

func (s *Service) answer(w http.ResponseWriter, status int, body any, err error) {
	if err != nil {
		var r *Refusal
		if errors.As(err, &r) {
			// The reason is for the operator; the caller learns the code only.
			s.log.Warn("identity: refused", "code", r.Code, "status", r.Status, "reason", r.Reason)
			writeJSON(w, r.Status, map[string]string{"error": r.Code})
			return
		}
		s.log.Error("identity: internal failure", "error", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": CodeUnavailable})
		return
	}
	if status == http.StatusNoContent {
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(status)
		return
	}
	writeJSON(w, status, body)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
