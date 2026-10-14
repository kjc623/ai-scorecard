package diagnostics

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/protocol"

	"github.com/shadow-ai-capture/control-api/internal/deploy"
	"github.com/shadow-ai-capture/control-api/internal/store"
	"github.com/shadow-ai-capture/control-api/internal/store/storetest"
)

const (
	tenant = "0b7f3c1e-6c2d-4a51-9b1e-7f3c1e6c2d4a"
	device = "5a3c0de0-7e57-4a11-9000-0000000d3a01"
	other  = "5a3c0de0-7e57-4a11-9000-0000000d3a02"
)

type fixture struct {
	st    *storetest.Memory
	h     *Handler
	mux   *http.ServeMux
	now   time.Time
	roles []string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{st: storetest.New(), now: time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC), roles: []string{deploy.RoleAdmin}}
	f.st.AddTenant(store.Tenant{TenantID: tenant, Status: "active", IngestEnabled: true})
	f.st.AddDevice(store.Device{TenantID: tenant, DeviceID: device, OS: "windows", Hostname: "LAPTOP 7", AgentVersion: "1.0.42"})
	f.st.AddDevice(store.Device{TenantID: tenant, DeviceID: other, OS: "windows"})
	h, err := New(f.st, func(r *http.Request) (deploy.Principal, error) {
		if r.Header.Get("Authorization") == "" {
			return deploy.Principal{}, errors.New("no token")
		}
		return deploy.Principal{Tenant: tenant, Actor: "admin@example.com", Subject: "sub-1", Roles: f.roles}, nil
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	h.now = func() time.Time { return f.now }
	f.h = h
	f.mux = http.NewServeMux()
	h.Register(f.mux, func(r *http.Request) (string, string, error) {
		return tenant, r.Header.Get("X-Test-Device"), nil
	})
	return f
}

func (f *fixture) do(method, path string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, bytes.NewReader(body))
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, r)
	return w
}

func (f *fixture) admin(method, path string) *httptest.ResponseRecorder {
	return f.do(method, path, nil, map[string]string{"Authorization": "Bearer t"})
}

func (f *fixture) upload(dev, request string, body []byte) *httptest.ResponseRecorder {
	return f.do(http.MethodPost, protocol.DiagnosticsUploadPath, body, map[string]string{
		"X-Test-Device": dev, protocol.HeaderDiagnosticsRequest: request, "Content-Type": protocol.DiagnosticsContentType})
}

func zipOf(t *testing.T, name, body string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create(name)
	_, _ = w.Write([]byte(body))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func decode[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("%d %s: %v", w.Code, w.Body.String(), err)
	}
	return v
}

func errorCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	return decode[struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}](t, w).Error.Code
}

// TestRequestUploadDownload walks one request: the admin asks, a second ask returns the same
// request, the device uploads once, the admin lists and downloads it, and each step is audited.
func TestRequestUploadDownload(t *testing.T) {
	f := newFixture(t)
	base := "/admin/v1/devices/" + device + "/diagnostics"

	w := f.admin(http.MethodPost, base)
	if w.Code != http.StatusCreated {
		t.Fatalf("request: %d %s", w.Code, w.Body)
	}
	req := decode[diagnosticsJSON](t, w)
	if req.State != "waiting" || req.RequestedBy != "admin@example.com" || !req.ExpiresAt.Equal(f.now.Add(RequestLifetime)) {
		t.Fatalf("request: %+v", req)
	}
	f.now = f.now.Add(time.Minute)
	if w := f.admin(http.MethodPost, base); w.Code != http.StatusOK || decode[diagnosticsJSON](t, w).RequestID != req.RequestID {
		t.Fatalf("a second ask: %d %s", w.Code, w.Body)
	}

	bundle := zipOf(t, "health.json", "{}")
	if w := f.upload(other, req.RequestID, bundle); w.Code != http.StatusConflict || errorCode(t, w) != string(protocol.ReasonDiagnosticsNotPending) {
		t.Fatalf("another device's upload: %d %s", w.Code, w.Body)
	}
	if w := f.upload(device, req.RequestID, []byte("not a zip")); w.Code != http.StatusBadRequest {
		t.Fatalf("a body that is not a zip: %d", w.Code)
	}
	if w := f.upload(device, "nope", bundle); w.Code != http.StatusBadRequest {
		t.Fatalf("no request id: %d", w.Code)
	}
	if w := f.upload(device, req.RequestID, make([]byte, protocol.MaxDiagnosticsBundleBytes+1)); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("an oversize bundle: %d", w.Code)
	}
	f.now = f.now.Add(time.Minute)
	if w := f.upload(device, req.RequestID, bundle); w.Code != http.StatusNoContent {
		t.Fatalf("upload: %d %s", w.Code, w.Body)
	}
	if w := f.upload(device, req.RequestID, bundle); w.Code != http.StatusConflict {
		t.Fatalf("a second upload: %d", w.Code)
	}

	list := decode[struct {
		Diagnostics []diagnosticsJSON `json:"diagnostics"`
	}](t, f.admin(http.MethodGet, base)).Diagnostics
	if len(list) != 1 || list[0].State != "received" || *list[0].SizeBytes != int64(len(bundle)) || *list[0].AgentVersion != "1.0.42" {
		t.Fatalf("list: %+v", list)
	}

	w = f.admin(http.MethodGet, base+"/"+req.RequestID+"/bundle")
	if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), bundle) || w.Header().Get("Content-Type") != "application/zip" {
		t.Fatalf("download: %d %q", w.Code, w.Header())
	}
	if got := w.Header().Get("Content-Disposition"); got != `attachment; filename="diagnostics-LAPTOP-7-20261014-1202.zip"` {
		t.Fatalf("Content-Disposition %q", got)
	}
	if w := f.admin(http.MethodGet, "/admin/v1/devices/"+other+"/diagnostics/"+req.RequestID+"/bundle"); w.Code != http.StatusNotFound {
		t.Fatalf("the bundle under another device: %d", w.Code)
	}
	f.now = f.now.Add(BundleLifetime)
	if w := f.admin(http.MethodGet, base+"/"+req.RequestID+"/bundle"); w.Code != http.StatusNotFound {
		t.Fatalf("an expired bundle: %d", w.Code)
	}

	var actions []string
	for _, a := range f.st.Audits() {
		if strings.HasPrefix(a.Action, "device.diagnostics.") {
			actions = append(actions, a.ActorType+":"+a.Action)
		}
	}
	want := "user:device.diagnostics.request,device:device.diagnostics.receive,user:device.diagnostics.download"
	if strings.Join(actions, ",") != want {
		t.Fatalf("audit %v, want %s", actions, want)
	}
}

// TestAdminRoutesRefuse refuses a caller without a token or the admin role, a malformed or unknown
// device, and a revoked one.
func TestAdminRoutesRefuse(t *testing.T) {
	f := newFixture(t)
	base := "/admin/v1/devices/" + device + "/diagnostics"
	if w := f.do(http.MethodPost, base, nil, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", w.Code)
	}
	f.roles = []string{"analyst"}
	if w := f.admin(http.MethodGet, base); w.Code != http.StatusForbidden {
		t.Fatalf("an analyst: %d", w.Code)
	}
	f.roles = []string{deploy.RoleAdmin}
	if w := f.admin(http.MethodPost, "/admin/v1/devices/not-a-device/diagnostics"); w.Code != http.StatusNotFound {
		t.Fatalf("a malformed device: %d", w.Code)
	}
	if w := f.admin(http.MethodPost, "/admin/v1/devices/5a3c0de0-7e57-4a11-9000-0000000d3a09/diagnostics"); w.Code != http.StatusNotFound {
		t.Fatalf("an unknown device: %d", w.Code)
	}
	revoked := f.now
	f.st.AddDevice(store.Device{TenantID: tenant, DeviceID: other, OS: "windows", RevokedAt: &revoked})
	if w := f.admin(http.MethodPost, "/admin/v1/devices/"+other+"/diagnostics"); w.Code != http.StatusConflict {
		t.Fatalf("a revoked device: %d", w.Code)
	}
}
