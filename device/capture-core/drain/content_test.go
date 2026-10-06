package drain

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/contentstore"
	"github.com/shadow-ai-capture/device/protocol"
)

const contentEvent = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"

// fakeContent is a ContentSource holding one ready object.
type fakeContent struct {
	mu      sync.Mutex
	body    []byte
	ready   bool
	settled contentstore.State
	detail  string
	retries int
}

func (f *fakeContent) MarkDelivered(string, contentstore.Request) (bool, error) { return true, nil }
func (f *fakeContent) Get(string) ([]byte, error)                               { return f.body, nil }
func (f *fakeContent) Expire(time.Time) (int, error)                            { return 0, nil }

func (f *fakeContent) Ready() []contentstore.Item {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.ready {
		return nil
	}
	return []contentstore.Item{{EventID: contentEvent, SizeBytes: int64(len(f.body)), State: contentstore.StateReady,
		Request: contentstore.Request{CollectionMode: "m3", ContentDigest: "sha256:abc", PolicyRuleID: "r1", AttachmentCount: 1}}}
}

func (f *fakeContent) Settle(_ string, state contentstore.State, detail string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ready, f.settled, f.detail = false, state, detail
	return nil
}

func (f *fakeContent) Retry(string, string, time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.retries++
	return nil
}

func contentDrainer(t *testing.T, e *fakeEdge, src *fakeContent) *Drainer {
	t.Helper()
	return newTestDrainer(t, e, nil, e.issued(24*time.Hour), func(c *Config) { c.Content = src })
}

// A granted event's content is uploaded once to POST /v1/content as the plaintext body, with the
// grant id, the event id and the digest of the exact body bytes, over the device certificate.
func TestGrantedContentIsUploadedWithItsHeaders(t *testing.T) {
	e := newFakeEdge(t)
	src := &fakeContent{body: []byte("the prompt as typed"), ready: true}
	var grantReq protocol.ContentGrantRequest
	var upload struct {
		body                     []byte
		grant, event, digest, cn string
	}
	e.mux.HandleFunc("/v1/content/grant", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&grantReq)
		raw, _ := json.Marshal(protocol.ContentGrantResponse{GrantID: "g-1", State: protocol.ContentGrantGranted,
			ExpiresAt: time.Now().Add(time.Minute), MaxBytes: protocol.MaxContentObjectBytes})
		writeJSON(t, w, http.StatusOK, raw)
	})
	e.mux.HandleFunc(protocol.ContentUploadPath, func(w http.ResponseWriter, r *http.Request) {
		upload.body, _ = io.ReadAll(r.Body)
		upload.grant = r.Header.Get(protocol.HeaderContentGrantID)
		upload.event = r.Header.Get(protocol.HeaderContentEventID)
		upload.digest = r.Header.Get(protocol.HeaderContentRawDigest)
		if len(r.TLS.PeerCertificates) > 0 {
			upload.cn = r.TLS.PeerCertificates[0].Subject.CommonName
		}
		w.WriteHeader(http.StatusCreated)
	})
	d := contentDrainer(t, e, src)
	d.contentPass(context.Background(), time.Now().Add(5*time.Second))

	if grantReq.EventID != contentEvent || grantReq.SizeBytes != int64(len(src.body)) || grantReq.RawSizeBytes != int64(len(src.body)) ||
		grantReq.AttachmentCount != 1 || grantReq.CollectionMode != protocol.ModeM3 || grantReq.SchemaVersion != protocol.ContentGrantSchemaVersion {
		t.Fatalf("grant request = %+v", grantReq)
	}
	if string(upload.body) != string(src.body) || upload.grant != "g-1" || upload.event != contentEvent ||
		upload.digest != protocol.RawDigest(src.body) || upload.cn != testDevice {
		t.Fatalf("upload = %+v", upload)
	}
	if src.settled != contentstore.StateUploaded || src.detail != "g-1" {
		t.Fatalf("settled %s %q, want uploaded", src.settled, src.detail)
	}
}

// A grant already consumed means an earlier upload landed and its acknowledgement was lost: the
// retry is settled as uploaded, at the grant and at the upload.
func TestConsumedGrantIsIdempotent(t *testing.T) {
	for _, at := range []string{"grant", "upload"} {
		t.Run(at, func(t *testing.T) {
			e := newFakeEdge(t)
			src := &fakeContent{body: []byte("x"), ready: true}
			consumed := []byte(`{"error":{"code":"grant_consumed"}}`)
			e.mux.HandleFunc("/v1/content/grant", func(w http.ResponseWriter, r *http.Request) {
				if at == "grant" {
					writeJSON(t, w, http.StatusConflict, consumed)
					return
				}
				raw, _ := json.Marshal(protocol.ContentGrantResponse{GrantID: "g-2", State: protocol.ContentGrantGranted})
				writeJSON(t, w, http.StatusOK, raw)
			})
			e.mux.HandleFunc(protocol.ContentUploadPath, func(w http.ResponseWriter, r *http.Request) {
				writeJSON(t, w, http.StatusConflict, consumed)
			})
			d := contentDrainer(t, e, src)
			d.contentPass(context.Background(), time.Now().Add(5*time.Second))
			if src.settled != contentstore.StateUploaded {
				t.Fatalf("settled %q, want uploaded", src.settled)
			}
		})
	}
}

func TestDeniedGrantIsTerminalAndNothingIsUploaded(t *testing.T) {
	e := newFakeEdge(t)
	src := &fakeContent{body: []byte("x"), ready: true}
	e.mux.HandleFunc("/v1/content/grant", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := json.Marshal(protocol.ContentGrantResponse{State: protocol.ContentGrantDenied, Reason: "tenant_disabled"})
		writeJSON(t, w, http.StatusOK, raw)
	})
	e.mux.HandleFunc(protocol.ContentUploadPath, func(w http.ResponseWriter, r *http.Request) {
		t.Error("content was uploaded after a denial")
	})
	d := contentDrainer(t, e, src)
	d.contentPass(context.Background(), time.Now().Add(5*time.Second))
	if src.settled != contentstore.StateDenied || src.detail != "tenant_disabled" {
		t.Fatalf("settled %s %q, want denied/tenant_disabled", src.settled, src.detail)
	}
}

func TestContentOverTheGrantsMaximumIsDenied(t *testing.T) {
	e := newFakeEdge(t)
	src := &fakeContent{body: []byte("twelve bytes"), ready: true}
	e.mux.HandleFunc("/v1/content/grant", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := json.Marshal(protocol.ContentGrantResponse{GrantID: "g-3", State: protocol.ContentGrantGranted, MaxBytes: 4})
		writeJSON(t, w, http.StatusOK, raw)
	})
	e.mux.HandleFunc(protocol.ContentUploadPath, func(w http.ResponseWriter, r *http.Request) {
		t.Error("content over the grant's maximum was uploaded")
	})
	d := contentDrainer(t, e, src)
	d.contentPass(context.Background(), time.Now().Add(5*time.Second))
	if src.settled != contentstore.StateDenied {
		t.Fatalf("settled %q, want denied", src.settled)
	}
}

// An outage at the upload is retried with a fresh grant later, never settled.
func TestFailedUploadIsRetried(t *testing.T) {
	e := newFakeEdge(t)
	src := &fakeContent{body: []byte("x"), ready: true}
	e.mux.HandleFunc("/v1/content/grant", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := json.Marshal(protocol.ContentGrantResponse{GrantID: "g-4", State: protocol.ContentGrantGranted})
		writeJSON(t, w, http.StatusOK, raw)
	})
	e.mux.HandleFunc(protocol.ContentUploadPath, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusServiceUnavailable, []byte(`{"error":{"code":"unavailable"}}`))
	})
	d := contentDrainer(t, e, src)
	d.contentPass(context.Background(), time.Now().Add(5*time.Second))
	if src.settled != "" || src.retries != 1 {
		t.Fatalf("settled %q after %d retries, want unsettled with one retry", src.settled, src.retries)
	}
}
