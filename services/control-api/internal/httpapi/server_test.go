package httpapi_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/protocol"

	"github.com/shadow-ai-capture/control-api/internal/apierr"
	"github.com/shadow-ai-capture/control-api/internal/content"
	"github.com/shadow-ai-capture/control-api/internal/deviceca/devicecatest"
	"github.com/shadow-ai-capture/control-api/internal/enrol"
	"github.com/shadow-ai-capture/control-api/internal/health"
	"github.com/shadow-ai-capture/control-api/internal/httpapi"
	"github.com/shadow-ai-capture/control-api/internal/policyserve"
	"github.com/shadow-ai-capture/control-api/internal/store"
	"github.com/shadow-ai-capture/control-api/internal/store/storetest"
)

const (
	tenantID = "5a3c0de0-7e57-4a11-9000-0000000d3a01"
	eventID  = "66666666-6666-4666-8666-666666666666"
	region   = "eu-west"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// contentStore is a content.Store with one M3 event observed by one device.
type contentStore struct {
	device string
	grants map[string]content.Grant
}

func (c *contentStore) EventContext(_ context.Context, _, deviceID, _ string) (*content.EventContext, error) {
	ec := &content.EventContext{CeilingMode: "m3", ContentSearch: "full_text"}
	if deviceID == c.device {
		ec.Found, ec.Kind, ec.CollectionMode, ec.ContentState = true, "prompt", "m3", "local_only"
	}
	return ec, nil
}

func (c *contentStore) InsertGrant(_ context.Context, _ string, g content.Grant) error {
	c.grants[g.GrantID] = g
	return nil
}

func (c *contentStore) Grant(_ context.Context, _, id string) (*content.Grant, error) {
	g, ok := c.grants[id]
	if !ok {
		return nil, content.ErrGrantUnknown
	}
	return &g, nil
}

func (c *contentStore) AddContentUsage(context.Context, string, int64) error { return nil }

type vault struct{ bodies [][]byte }

func (v *vault) Put(_ context.Context, _, _, _, _ string, body []byte) (bool, error) {
	v.bodies = append(v.bodies, body)
	return true, nil
}

type harness struct {
	t       *testing.T
	store   *storetest.Memory
	ca      *devicecatest.Authority
	content *contentStore
	vault   *vault
	handler http.Handler
	key     string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	st := storetest.New()
	st.AddTenant(store.Tenant{TenantID: tenantID, Status: "active", IngestEnabled: true, ResidencyRegion: region})
	st.SetCeiling(tenantID, "m3")
	st.SetCatalogueHosts("api.anthropic.com")
	ca := devicecatest.New(t)
	enrolSvc, err := enrol.New(st, ca.CA, enrol.Config{Region: region})
	if err != nil {
		t.Fatal(err)
	}
	healthSvc, err := health.New(st, health.Config{})
	if err != nil {
		t.Fatal(err)
	}
	_, signingKey, _ := ed25519.GenerateKey(rand.Reader)
	policySvc, err := policyserve.New(st, signingKey, policyserve.Config{Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	cs, v := &contentStore{grants: map[string]content.Grant{}}, &vault{}
	contentSvc, err := content.New(cs, v)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, store: st, ca: ca, content: cs, vault: v}
	keyID, _ := store.NewUUID()
	if h.key, err = enrol.MintDeploymentKey(tenantID); err != nil {
		t.Fatal(err)
	}
	st.AddDeploymentKey(store.DeploymentKey{KeyID: keyID, TenantID: tenantID, KeyHash: enrol.HashDeploymentKey(h.key), CreatedAt: time.Now()})
	srv := &httpapi.Server{
		Store: st, CA: ca.CA, Enrol: enrolSvc, Health: healthSvc, Policy: policySvc, Content: contentSvc, Logger: quiet,
	}
	h.handler = srv.Handler()
	return h
}

func (h *harness) do(method, path, certHeader string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	if certHeader != "" {
		req.Header.Set(protocol.HeaderClientCert, certHeader)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec
}

func enrolment(t *testing.T, key, hwid string) []byte {
	t.Helper()
	b, _ := json.Marshal(protocol.EnrolmentRequest{
		SchemaVersion: protocol.EnrolmentSchemaVersion,
		DeploymentKey: key,
		CSR:           devicecatest.NewDeviceKey(t).CSRPEM,
		Device:        protocol.DeviceInfo{OS: "windows", AgentVersion: "1.0.0", HardwareIdentityHash: hwid},
	})
	return b
}

// enrol enrols a device with the deployment key and returns its response and its certificate as
// the gateway forwards it.
func (h *harness) enrol(hwid string) (protocol.EnrolmentResponse, string) {
	h.t.Helper()
	rec := h.do(http.MethodPost, "/v1/enrol", "", enrolment(h.t, h.key, hwid), nil)
	if rec.Code != http.StatusOK {
		h.t.Fatalf("enrol: %d %s", rec.Code, rec.Body.String())
	}
	var resp protocol.EnrolmentResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		h.t.Fatal(err)
	}
	return resp, url.QueryEscape(resp.Credential.CertPEM)
}

func errorCode(rec *httptest.ResponseRecorder) string {
	var e struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &e)
	return e.Error.Code
}

func healthReport() []byte {
	b, _ := json.Marshal(protocol.HealthRequest{
		SchemaVersion: protocol.HealthSchemaVersion, ReportedAt: time.Now(), AgentVersion: "1.0.0",
		Collectors: []protocol.HealthReport{{Collector: "egress_proxy", State: protocol.StateHealthy}},
	})
	return b
}

func grantRequest() []byte {
	b, _ := json.Marshal(protocol.ContentGrantRequest{
		SchemaVersion: protocol.ContentGrantSchemaVersion, EventID: eventID, CollectionMode: protocol.ModeM3,
		ContentDigest: "sha256:" + strings.Repeat("a", 64), SizeBytes: 10, RawSizeBytes: 10,
	})
	return b
}

func TestEnrolThenRotateWithTheForwardedCertificate(t *testing.T) {
	h := newHarness(t)
	first, cert := h.enrol("hw-1")

	rec := h.do(http.MethodPost, "/v1/enrol", cert, enrolment(t, "", "hw-1"), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("rotation: %d %s", rec.Code, rec.Body.String())
	}
	var rotated protocol.EnrolmentResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &rotated)
	if rotated.DeviceID != first.DeviceID || !rotated.Reenrolled || rotated.Credential.CertPEM == first.Credential.CertPEM {
		t.Fatalf("rotation = %+v", rotated)
	}
	// The rotated-away certificate no longer authenticates; the new one does.
	if rec := h.do(http.MethodPost, "/v1/health", cert, healthReport(), nil); rec.Code != http.StatusUnauthorized || errorCode(rec) != apierr.CodeRevokedDevice {
		t.Fatalf("the previous certificate: %d %s", rec.Code, rec.Body.String())
	}
	if rec := h.do(http.MethodPost, "/v1/health", url.QueryEscape(rotated.Credential.CertPEM), healthReport(), nil); rec.Code != http.StatusOK {
		t.Fatalf("the rotated certificate: %d %s", rec.Code, rec.Body.String())
	}
	// A rotation without any credential is refused.
	if rec := h.do(http.MethodPost, "/v1/enrol", "", enrolment(t, "", "hw-1"), nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no credential: %d", rec.Code)
	}
}

func TestDeviceRoutesWithTheForwardedCertificate(t *testing.T) {
	h := newHarness(t)
	resp, cert := h.enrol("hw-routes")
	h.content.device = resp.DeviceID

	if rec := h.do(http.MethodPost, "/v1/health", cert, healthReport(), nil); rec.Code != http.StatusOK {
		t.Fatalf("health: %d %s", rec.Code, rec.Body.String())
	}
	if _, ok := h.store.CollectorStateAt(tenantID, resp.DeviceID, "egress_proxy"); !ok {
		t.Fatal("the health report was not recorded against the certificate's device")
	}

	rec := h.do(http.MethodGet, "/v1/policy", cert, nil, nil)
	if rec.Code != http.StatusOK || rec.Header().Get(protocol.HeaderETag) == "" {
		t.Fatalf("policy: %d %s", rec.Code, rec.Body.String())
	}
	if again := h.do(http.MethodGet, "/v1/policy", cert, nil, map[string]string{protocol.HeaderIfNoneMatch: rec.Header().Get(protocol.HeaderETag)}); again.Code != http.StatusNotModified {
		t.Fatalf("policy with a matching ETag: %d", again.Code)
	}

	rec = h.do(http.MethodPost, "/v1/content/grant", cert, grantRequest(), nil)
	var grant protocol.ContentGrantResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &grant)
	if rec.Code != http.StatusOK || grant.State != protocol.ContentGrantGranted {
		t.Fatalf("grant: %d %s", rec.Code, rec.Body.String())
	}
	body := []byte(`{"prompt":"hello"}`)
	rec = h.do(http.MethodPost, protocol.ContentUploadPath, cert, body, map[string]string{
		protocol.HeaderContentGrantID:   grant.GrantID,
		protocol.HeaderContentEventID:   eventID,
		protocol.HeaderContentRawDigest: protocol.RawDigest(body),
	})
	if rec.Code != http.StatusOK || len(h.vault.bodies) != 1 || !bytes.Equal(h.vault.bodies[0], body) {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body.String())
	}
}

// Every route that relies on the forwarded certificate refuses one that does not verify against
// the device CA or does not match the device's live credential.
func TestForwardedCertificateIsVerified(t *testing.T) {
	h := newHarness(t)
	resp, good := h.enrol("hw-verify")
	leaf, _ := pem.Decode([]byte(resp.Credential.CertPEM))
	issued, _ := x509.ParseCertificate(leaf.Bytes)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	subject := pkix.Name{CommonName: resp.DeviceID, OrganizationalUnit: []string{tenantID}}
	tmpl := func(mutate func(*x509.Certificate)) *x509.Certificate {
		c := &x509.Certificate{
			Subject: subject, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
			KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		}
		if mutate != nil {
			mutate(c)
		}
		return c
	}
	forward := func(c *x509.Certificate) string {
		return url.QueryEscape(string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})))
	}
	other := devicecatest.New(t)
	cases := map[string]struct{ header, code string }{
		"no certificate": {"", apierr.CodeUnauthenticated},
		"not PEM":        {"garbage", apierr.CodeInvalidClientCert},
		"another CA":     {forward(other.Leaf(t, tmpl(nil), &key.PublicKey)), apierr.CodeInvalidClientCert},
		"expired": {forward(h.ca.Leaf(t, tmpl(func(c *x509.Certificate) {
			c.NotBefore, c.NotAfter = time.Now().Add(-48*time.Hour), time.Now().Add(-24*time.Hour)
		}), &key.PublicKey)), apierr.CodeInvalidClientCert},
		"not client auth": {forward(h.ca.Leaf(t, tmpl(func(c *x509.Certificate) {
			c.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		}), &key.PublicKey)), apierr.CodeInvalidClientCert},
		"no tenant": {forward(h.ca.Leaf(t, tmpl(func(c *x509.Certificate) {
			c.Subject = pkix.Name{CommonName: resp.DeviceID}
		}), &key.PublicKey)), apierr.CodeInvalidClientCert},
		"CA-signed but never issued": {forward(h.ca.Leaf(t, tmpl(nil), &key.PublicKey)), apierr.CodeRevokedDevice},
	}
	routes := []struct {
		method, path string
		body         []byte
	}{
		{http.MethodPost, "/v1/health", healthReport()},
		{http.MethodGet, "/v1/policy", nil},
		{http.MethodPost, "/v1/content/grant", grantRequest()},
		{http.MethodPost, protocol.ContentUploadPath, []byte("x")},
		{http.MethodPost, "/v1/enrol", enrolment(t, "", "hw-verify")},
	}
	for name, c := range cases {
		for _, r := range routes {
			rec := h.do(r.method, r.path, c.header, r.body, nil)
			if rec.Code != http.StatusUnauthorized || errorCode(rec) != c.code {
				t.Errorf("%s %s: %d %s, want 401 %s", name, r.path, rec.Code, errorCode(rec), c.code)
			}
		}
	}
	if len(h.vault.bodies) != 0 {
		t.Fatal("a refused certificate reached content-vault")
	}

	// Both percent encodings of the PEM are accepted, as is the raw PEM.
	pemText := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: issued.Raw}))
	for name, header := range map[string]string{
		"query escaped": good,
		"path escaped":  url.PathEscape(pemText),
	} {
		if rec := h.do(http.MethodPost, "/v1/health", header, healthReport(), nil); rec.Code != http.StatusOK {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}

	// A revoked credential no longer authenticates.
	h.store.RevokeCredential(tenantID, protocol.CredentialID(issued.Raw), time.Now())
	if rec := h.do(http.MethodPost, "/v1/health", good, healthReport(), nil); rec.Code != http.StatusUnauthorized || errorCode(rec) != apierr.CodeRevokedDevice {
		t.Fatalf("revoked credential: %d %s", rec.Code, rec.Body.String())
	}
}

func TestBodiesAreCapped(t *testing.T) {
	h := newHarness(t)
	resp, cert := h.enrol("hw-caps")
	h.content.device = resp.DeviceID
	if rec := h.do(http.MethodPost, "/v1/enrol", "", bytes.Repeat([]byte("a"), httpapi.MaxBodyBytes+1), nil); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized enrolment: %d", rec.Code)
	}
	big := bytes.Repeat([]byte("a"), protocol.MaxContentObjectBytes+1)
	if rec := h.do(http.MethodPost, protocol.ContentUploadPath, cert, big, nil); rec.Code != http.StatusRequestEntityTooLarge || errorCode(rec) != string(protocol.ReasonOversize) {
		t.Fatalf("oversized upload: %d %s", rec.Code, errorCode(rec))
	}
}

func TestProbes(t *testing.T) {
	h := newHarness(t)
	for _, path := range []string{"/healthz", "/readyz"} {
		if rec := h.do(http.MethodGet, path, "", nil, nil); rec.Code != http.StatusOK {
			t.Errorf("%s: %d", path, rec.Code)
		}
	}
}
