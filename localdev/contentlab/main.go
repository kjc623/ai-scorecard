// contentlab is the local lab's stand-in for ciphertext storage, the one thing the M3 content path
// needs that the lab has no real form of (docs/02-ingest-and-transport.md §10).
//
// In Azure a granted device writes one object to Blob storage with a single-object, write-only,
// short-lived credential, and a finaliser promotes it once its size and digest are verified. Here
// the edge forwards the device's PUT to this service, which verifies the upload URL control-api
// signed, stores the bytes once, and reports the upload to control-api's finaliser. A rejected
// upload is deleted. content-vault reads a stored object back from it for an approved retrieval.
//
// It has no page of its own. An analyst reads content in the dashboard (query/dashboard, the
// Explore page), which reaches the vault through query-api.
//
// It is a lab tool: the stored objects are readable by anything on the lab network, which is not
// how a storage account works. They are ciphertext, and the keys are the vault's.
package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The upload metadata headers and the signature formats below are endpoint/protocol's and
// control-api/internal/content's. They are repeated rather than imported because this program,
// like the edge, is a standard-library-only module with no dependency on the product's.
const (
	headerGrantID       = "X-Sac-Grant-Id"
	headerEventID       = "X-Sac-Event-Id"
	headerWrappedKey    = "X-Sac-Wrapped-Key"
	headerKeyID         = "X-Sac-Key-Id"
	headerKeyVersion    = "X-Sac-Key-Version"
	headerPlaintextSize = "X-Sac-Plaintext-Size"
	headerRawDigest     = "X-Sac-Raw-Digest"

	maxObjectBytes = 64 << 20
)

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

type server struct {
	dataDir    string
	controlURL string
	key        []byte
	log        *slog.Logger
	http       *http.Client
}

func main() {
	addr := flag.String("addr", "0.0.0.0:8080", "listen address")
	dataDir := flag.String("data-dir", "/data", "where stored objects are kept")
	controlURL := flag.String("control-url", "http://control-api:8080", "control-api base URL (the finaliser)")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	key := os.Getenv("SAC_UPLOAD_SIGNING_KEY")
	if len(key) < 16 {
		log.Error("SAC_UPLOAD_SIGNING_KEY is required (at least 16 bytes): it is how an upload URL is verified")
		os.Exit(1)
	}
	if err := os.MkdirAll(*dataDir, 0o700); err != nil {
		log.Error("creating the data directory", "error", err)
		os.Exit(1)
	}
	s := &server{
		dataDir: *dataDir, controlURL: strings.TrimRight(*controlURL, "/"),
		key: []byte(key), log: log, http: &http.Client{Timeout: 30 * time.Second},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /v1/content/upload/{object}", s.handleUpload)
	mux.HandleFunc("GET /blob/{tenant}/{object}", s.handleBlob)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{"status":"ok"}`) })

	log.Info("contentlab listening", "addr", *addr, "data_dir", *dataDir, "control", s.controlURL)
	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("serving", "error", err)
		os.Exit(1)
	}
}

func signUpload(key []byte, tenantID, objectID, grantID, exp string) string {
	mac := hmac.New(sha256.New, key)
	fmt.Fprintf(mac, "sac.upload.v1\n%s\n%s\n%s\n%s", tenantID, objectID, grantID, exp)
	return hex.EncodeToString(mac.Sum(nil))
}

func signBody(key, body []byte) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("sac.finalise.v1\n"))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func (s *server) objectPath(tenantID, objectID string) string {
	return filepath.Join(s.dataDir, tenantID, objectID+".bin")
}

// handleUpload is the one write a grant permits.
func (s *server) handleUpload(w http.ResponseWriter, r *http.Request) {
	objectID := r.PathValue("object")
	q := r.URL.Query()
	tenantID, grantID, exp, sig := q.Get("tenant"), q.Get("grant"), q.Get("exp"), q.Get("sig")
	if !uuidPattern.MatchString(objectID) || !uuidPattern.MatchString(tenantID) || !uuidPattern.MatchString(grantID) {
		http.Error(w, "not an upload URL", http.StatusForbidden)
		return
	}
	// The URL is the credential: one object, one grant, one expiry (§10.3).
	if !hmac.Equal([]byte(signUpload(s.key, tenantID, objectID, grantID, exp)), []byte(sig)) {
		http.Error(w, "the upload URL is not one control-api issued", http.StatusForbidden)
		return
	}
	if unix, err := strconv.ParseInt(exp, 10, 64); err != nil || time.Now().Unix() > unix {
		http.Error(w, "the upload window has closed", http.StatusForbidden)
		return
	}
	if r.Header.Get(headerGrantID) != grantID {
		http.Error(w, "the object metadata names a different grant", http.StatusForbidden)
		return
	}
	objectPath := s.objectPath(tenantID, objectID)
	if err := os.MkdirAll(filepath.Dir(objectPath), 0o700); err != nil {
		http.Error(w, "storage unavailable", http.StatusServiceUnavailable)
		return
	}
	// Single use: O_EXCL makes a second write for the object a conflict, not an overwrite.
	f, err := os.OpenFile(objectPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		http.Error(w, "the object was already written", http.StatusConflict)
		return
	}
	if err != nil {
		http.Error(w, "storage unavailable", http.StatusServiceUnavailable)
		return
	}
	sum := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, sum), http.MaxBytesReader(w, r.Body, maxObjectBytes))
	_ = f.Close()
	if err != nil {
		_ = os.Remove(objectPath)
		http.Error(w, "the upload could not be read", http.StatusBadRequest)
		return
	}
	eventID := r.Header.Get(headerEventID)
	plaintextSize, _ := strconv.ParseInt(r.Header.Get(headerPlaintextSize), 10, 64)
	report, _ := json.Marshal(map[string]any{
		"tenant_id": tenantID, "grant_id": grantID, "object_id": objectID, "event_id": eventID,
		"blob_path": tenantID + "/" + objectID, "raw_digest": "sha256:" + hex.EncodeToString(sum.Sum(nil)),
		"declared_raw_digest": r.Header.Get(headerRawDigest), "size_bytes": n,
		"plaintext_size_bytes": plaintextSize, "wrapped_key_b64": r.Header.Get(headerWrappedKey),
		"key_id": r.Header.Get(headerKeyID), "key_version": r.Header.Get(headerKeyVersion),
	})
	status, body, err := s.post(s.controlURL+"/internal/v1/content/finalise", report, map[string]string{
		"X-Sac-Upload-Signature": signBody(s.key, report),
	})
	if err != nil || status != http.StatusOK {
		// The finaliser did not promote it, so the staged bytes go: a rejected upload must not
		// leave an object nobody can identify (§10.4), and a failed one can be written again.
		_ = os.Remove(objectPath)
		s.log.Warn("upload not finalised; staged object deleted", "object", objectID, "status", status, "error", err, "body", strings.TrimSpace(string(body)))
		if status == http.StatusUnprocessableEntity {
			http.Error(w, "the upload does not match a live grant", http.StatusUnprocessableEntity)
			return
		}
		http.Error(w, "the upload could not be finalised", http.StatusServiceUnavailable)
		return
	}
	s.log.Info("object stored", "tenant", tenantID, "object", objectID, "event", eventID, "bytes", n)
	w.WriteHeader(http.StatusCreated)
}

// handleBlob serves the stored ciphertext to the vault. The edge does not route it.
func (s *server) handleBlob(w http.ResponseWriter, r *http.Request) {
	tenantID, objectID := r.PathValue("tenant"), r.PathValue("object")
	if !uuidPattern.MatchString(tenantID) || !uuidPattern.MatchString(objectID) {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, s.objectPath(tenantID, objectID))
}

func (s *server) post(url string, body []byte, headers map[string]string) (int, []byte, error) {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, b, err
}
