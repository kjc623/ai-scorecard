package content

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/protocol"

	"github.com/shadow-ai-capture/control-api/internal/apierr"
)

const (
	tenant = "11111111-1111-1111-1111-111111111111"
	device = "22222222-2222-4222-8222-222222222222"
	event  = "33333333-3333-4333-8333-333333333333"
)

type fakeStore struct {
	ec       EventContext
	grants   map[string]Grant
	uploaded int64
	voided   []string
}

func (f *fakeStore) EventContext(_ context.Context, _, deviceID, _ string) (*EventContext, error) {
	ec := f.ec
	if deviceID != device {
		ec.Found = false
	}
	return &ec, nil
}
func (f *fakeStore) InsertGrant(_ context.Context, _ string, g Grant) error {
	if f.grants == nil {
		f.grants = map[string]Grant{}
	}
	f.grants[g.GrantID] = g
	return nil
}
func (f *fakeStore) Grant(_ context.Context, _, id string) (*Grant, error) {
	g, ok := f.grants[id]
	if !ok {
		return nil, ErrGrantUnknown
	}
	return &g, nil
}
func (f *fakeStore) VoidGrant(_ context.Context, _, id string) error {
	f.voided = append(f.voided, id)
	return nil
}
func (f *fakeStore) RecordUpload(_ context.Context, _, _ string, n int64) error {
	f.uploaded += n
	f.ec.ContentState = "uploaded"
	return nil
}

type fakeVault struct {
	prepared  int
	finalised []StoredObject
}

func (v *fakeVault) Prepare(context.Context, string, string, string, string, string, time.Time, int64) (*PreparedKey, error) {
	v.prepared++
	return &PreparedKey{ObjectKeyB64: "a2V5", WrappedDEK: "d3JhcHBlZA==", KEKID: "kek", KEKVersion: "1"}, nil
}
func (v *fakeVault) Finalise(_ context.Context, _, _ string, obj StoredObject) error {
	v.finalised = append(v.finalised, obj)
	return nil
}

func m3Event() EventContext {
	return EventContext{
		Found: true, Kind: "prompt", CollectionMode: "m3", ExpiresAt: time.Now().Add(time.Hour),
		SubmissionID: "44444444-4444-4444-8444-444444444444", ContentState: "not_captured",
		CeilingMode: "m3", BudgetBytesPerDay: 1 << 20,
	}
}

func request() protocol.ContentGrantRequest {
	return protocol.ContentGrantRequest{
		SchemaVersion: protocol.ContentGrantSchemaVersion, EventID: event, CollectionMode: protocol.ModeM3,
		ContentDigest: "sha256:" + strings.Repeat("0", 64), SizeBytes: 100, RawSizeBytes: 128,
	}
}

func newService(t *testing.T, st *fakeStore, v *fakeVault) *Service {
	t.Helper()
	s, err := New(st, v, Config{UploadSigningKey: []byte("0123456789abcdef0123")})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func statusOf(err error) int {
	var e *apierr.Error
	if errors.As(err, &e) {
		return e.Status
	}
	return 0
}

// §5.5: a granted decision carries one upload URL, the object key and the metadata the device
// repeats; it is recorded as a live grant naming one object.
func TestDecide_GrantsAnM3EventOfThisDevice(t *testing.T) {
	st, v := &fakeStore{ec: m3Event()}, &fakeVault{}
	resp, err := newService(t, st, v).Decide(context.Background(), tenant, device, request(), "https://edge.example")
	if err != nil {
		t.Fatal(err)
	}
	if resp.State != protocol.ContentGrantGranted || resp.Upload == nil || resp.Key == nil {
		t.Fatalf("response = %+v, want a granted decision with an upload and a key", resp)
	}
	g := st.grants[resp.GrantID]
	if g.Decision != DecisionGranted || g.ObjectID == "" {
		t.Fatalf("recorded grant = %+v", g)
	}
	if !strings.HasPrefix(resp.Upload.URL, "https://edge.example/v1/content/upload/"+g.ObjectID+"?") {
		t.Fatalf("upload URL %q does not name the granted object", resp.Upload.URL)
	}
	if resp.Upload.Headers[protocol.HeaderContentGrantID] != resp.GrantID {
		t.Fatal("the upload metadata does not carry the grant id")
	}
}

// §5.5: another device's event is unknown, a mode that holds no content is refused rather than
// denied, and a consumed grant is a conflict.
func TestDecide_Refusals(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*EventContext)
		device string
		want   int
	}{
		{"another device's event", func(*EventContext) {}, "99999999-9999-4999-8999-999999999999", http.StatusNotFound},
		{"an M2 event", func(e *EventContext) { e.CollectionMode = "m2" }, device, http.StatusUnprocessableEntity},
		{"already uploaded", func(e *EventContext) { e.ContentState = "uploaded" }, device, http.StatusConflict},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ec := m3Event()
			tc.mutate(&ec)
			v := &fakeVault{}
			_, err := newService(t, &fakeStore{ec: ec}, v).Decide(context.Background(), tenant, tc.device, request(), "https://e")
			if got := statusOf(err); got != tc.want {
				t.Fatalf("status = %d (%v), want %d", got, err, tc.want)
			}
			if v.prepared != 0 {
				t.Fatal("a refused request reached the vault")
			}
		})
	}
}

// §10.2: a denial is a 200 with a reason from the closed set, it mints no key, and it is terminal.
func TestDecide_Denials(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*EventContext)
		want   string
	}{
		{"ceiling below M3", func(e *EventContext) { e.CeilingMode = "m1" }, DenyModeNotPermitted},
		{"past retention", func(e *EventContext) { e.ExpiresAt = time.Now().Add(-time.Minute) }, DenyRetentionExpired},
		{"over budget", func(e *EventContext) { e.BudgetBytesPerDay = 100 }, DenyOverBudget},
		{"no budget set", func(e *EventContext) { e.BudgetBytesPerDay = 0 }, DenyOverBudget},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ec := m3Event()
			tc.mutate(&ec)
			st, v := &fakeStore{ec: ec}, &fakeVault{}
			s := newService(t, st, v)
			resp, err := s.Decide(context.Background(), tenant, device, request(), "https://e")
			if err != nil {
				t.Fatal(err)
			}
			if resp.State != protocol.ContentGrantDenied || resp.Reason != tc.want || resp.Key != nil || resp.Upload != nil {
				t.Fatalf("response = %+v, want denied/%s with no key and no upload", resp, tc.want)
			}
			if v.prepared != 0 {
				t.Fatal("a denial minted a key")
			}
			g := st.grants[resp.GrantID]
			st.ec.Grant = &g
			again, err := s.Decide(context.Background(), tenant, device, request(), "https://e")
			if err != nil || again.GrantID != resp.GrantID {
				t.Fatalf("a repeat request re-decided a denied event: %+v %v", again, err)
			}
		})
	}
}

// §10.4: the finaliser promotes only the bytes the device declared, against a live grant, once.
func TestFinalise(t *testing.T) {
	st, v := &fakeStore{ec: m3Event()}, &fakeVault{}
	s := newService(t, st, v)
	resp, err := s.Decide(context.Background(), tenant, device, request(), "https://e")
	if err != nil {
		t.Fatal(err)
	}
	g := st.grants[resp.GrantID]
	digest := "sha256:" + strings.Repeat("a", 64)
	report := UploadReport{
		TenantID: tenant, GrantID: g.GrantID, ObjectID: g.ObjectID, EventID: event, BlobPath: "p",
		RawDigest: digest, DeclaredRawDigest: digest, SizeBytes: 128, PlaintextSizeBytes: 100,
		WrappedKeyB64: "d3JhcHBlZA==", KeyID: "kek", KeyVersion: "1",
	}

	bad := report
	bad.DeclaredRawDigest = "sha256:" + strings.Repeat("b", 64)
	if _, err := s.Finalise(context.Background(), bad); !errors.Is(err, ErrUploadRejected) {
		t.Fatalf("a digest mismatch was not rejected: %v", err)
	}
	if len(st.voided) != 1 || len(v.finalised) != 0 {
		t.Fatalf("a mismatch must void the grant and store nothing (voided %d, stored %d)", len(st.voided), len(v.finalised))
	}

	wrong := report
	wrong.ObjectID = "55555555-5555-4555-8555-555555555555"
	if _, err := s.Finalise(context.Background(), wrong); !errors.Is(err, ErrUploadRejected) {
		t.Fatalf("an object the grant does not name was not rejected: %v", err)
	}

	if _, err := s.Finalise(context.Background(), report); err != nil {
		t.Fatal(err)
	}
	if len(v.finalised) != 1 || st.uploaded != 128 || st.ec.ContentState != "uploaded" {
		t.Fatalf("a verified upload was not recorded (stored %d, bytes %d, state %s)", len(v.finalised), st.uploaded, st.ec.ContentState)
	}
	if _, err := s.Finalise(context.Background(), report); !errors.Is(err, ErrUploadRejected) {
		t.Fatalf("a second write for the event was not rejected: %v", err)
	}
}

// §10.4 (task 08): the finaliser relays the device's request kind to the vault, so the vault can
// refuse to index a client-generated request.
func TestFinaliseCarriesPromptKind(t *testing.T) {
	ec := m3Event()
	ec.PromptKind = "client_generated"
	st, v := &fakeStore{ec: ec}, &fakeVault{}
	s := newService(t, st, v)
	resp, err := s.Decide(context.Background(), tenant, device, request(), "https://e")
	if err != nil {
		t.Fatal(err)
	}
	g := st.grants[resp.GrantID]
	digest := "sha256:" + strings.Repeat("a", 64)
	report := UploadReport{
		TenantID: tenant, GrantID: g.GrantID, ObjectID: g.ObjectID, EventID: event, BlobPath: "p",
		RawDigest: digest, DeclaredRawDigest: digest, SizeBytes: 128, PlaintextSizeBytes: 100,
		WrappedKeyB64: "d3JhcHBlZA==", KeyID: "kek", KeyVersion: "1",
	}
	if _, err := s.Finalise(context.Background(), report); err != nil {
		t.Fatal(err)
	}
	if len(v.finalised) != 1 {
		t.Fatalf("finalise stored %d objects, want 1", len(v.finalised))
	}
	if v.finalised[0].PromptKind != "client_generated" {
		t.Fatalf("the vault was told prompt_kind %q, want client_generated", v.finalised[0].PromptKind)
	}
}

// HTTPVault.Finalise carries the request kind across the internal HTTP boundary (task 08).
func TestHTTPVaultFinaliseCarriesPromptKind(t *testing.T) {
	var got map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading the request body: %v", err)
			return
		}
		if err := json.Unmarshal(body, &got); err != nil {
			t.Errorf("request body is not JSON: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	v := NewHTTPVault(ts.URL)
	err := v.Finalise(context.Background(), tenant, "device:"+device, StoredObject{
		ObjectID: "aaaaaaaa-0000-4000-8000-000000000001", SubmissionID: "bbbbbbbb-1111-4111-8111-00000000000b",
		EventID: event, PromptKind: "client_generated", BlobPath: "p",
		CiphertextSHA256: "sha256:" + strings.Repeat("a", 64), PlaintextSizeBytes: 100,
		WrappedDEK: "d3JhcHBlZA==", KEKID: "kek", KEKVersion: "1",
	})
	if err != nil {
		t.Fatalf("finalise: %v", err)
	}
	if got["prompt_kind"] != "client_generated" {
		t.Fatalf("the vault was sent prompt_kind %v, want client_generated", got["prompt_kind"])
	}
}
