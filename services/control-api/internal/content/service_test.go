package content

import (
	"bytes"
	"context"
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
	tenant     = "5a3c0de0-7e57-4a11-9000-0000000d3a01"
	device     = "22222222-2222-4222-8222-222222222222"
	event      = "33333333-3333-4333-8333-333333333333"
	submission = "44444444-4444-4444-8444-444444444444"
)

type fakeStore struct {
	ec       EventContext
	grants   map[string]Grant
	uploaded int64
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
	gg := g
	f.ec.Grant = &gg
	return nil
}

func (f *fakeStore) Grant(_ context.Context, _, id string) (*Grant, error) {
	g, ok := f.grants[id]
	if !ok {
		return nil, ErrGrantUnknown
	}
	return &g, nil
}

func (f *fakeStore) AddContentUsage(_ context.Context, _ string, n int64) error {
	f.uploaded += n
	return nil
}

// fakeVault stores each grant's content once and answers a repeat as a replay.
type fakeVault struct {
	puts   []string
	stored map[string]bool
	err    error
}

func (v *fakeVault) Put(_ context.Context, tenantID, eventID, grantID, digest string, body []byte) (bool, error) {
	v.puts = append(v.puts, strings.Join([]string{tenantID, eventID, grantID, digest, string(body)}, "|"))
	if v.err != nil {
		return false, v.err
	}
	if v.stored == nil {
		v.stored = map[string]bool{}
	}
	first := !v.stored[grantID]
	v.stored[grantID] = true
	return first, nil
}

func m3Event() EventContext {
	return EventContext{
		Found: true, Kind: "prompt", CollectionMode: "m3", ExpiresAt: time.Now().Add(time.Hour),
		SubmissionID: submission, ContentState: "not_captured",
		CeilingMode: "m3", ContentSearch: "full_text",
	}
}

func grantRequest() protocol.ContentGrantRequest {
	return protocol.ContentGrantRequest{
		SchemaVersion: protocol.ContentGrantSchemaVersion, EventID: event, CollectionMode: protocol.ModeM3,
		ContentDigest: "sha256:" + strings.Repeat("a", 64), SizeBytes: 100, RawSizeBytes: 120,
	}
}

func newService(t *testing.T, ec EventContext) (*Service, *fakeStore, *fakeVault) {
	t.Helper()
	st, v := &fakeStore{ec: ec}, &fakeVault{}
	s, err := New(st, v)
	if err != nil {
		t.Fatal(err)
	}
	return s, st, v
}

func code(err error) string {
	var e *apierr.Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

func TestGrantDecisions(t *testing.T) {
	for _, c := range []struct {
		name   string
		ec     func(*EventContext)
		req    func(*protocol.ContentGrantRequest)
		code   string
		reason string
	}{
		{name: "another device's event", ec: func(e *EventContext) { e.Found = false }, code: string(protocol.ReasonUnknownEvent)},
		{name: "not an M3 event", ec: func(e *EventContext) { e.CollectionMode = "m2" }, code: string(protocol.ReasonModeViolation)},
		{name: "already uploaded", ec: func(e *EventContext) { e.ContentState = "uploaded" }, code: string(protocol.ReasonGrantConsumed)},
		{name: "oversize", req: func(r *protocol.ContentGrantRequest) { r.RawSizeBytes = protocol.MaxContentObjectBytes + 1 }, code: string(protocol.ReasonOversize)},
		{name: "bad event id", req: func(r *protocol.ContentGrantRequest) { r.EventID = "x" }, code: apierr.CodeSchemaViolation},
		{name: "ceiling below m3", ec: func(e *EventContext) { e.CeilingMode = "m2" }, reason: DenyModeNotPermitted},
		{name: "retention expired", ec: func(e *EventContext) { e.ExpiresAt = time.Now().Add(-time.Minute) }, reason: DenyRetentionExpired},
		{name: "prompt storage off", ec: func(e *EventContext) { e.ContentSearch = "disabled" }, reason: DenyStorageOff},
	} {
		t.Run(c.name, func(t *testing.T) {
			ec := m3Event()
			if c.ec != nil {
				c.ec(&ec)
			}
			req := grantRequest()
			if c.req != nil {
				c.req(&req)
			}
			s, st, _ := newService(t, ec)
			resp, err := s.Decide(context.Background(), tenant, device, req)
			if c.code != "" {
				if code(err) != c.code {
					t.Fatalf("err = %v, want %s", err, c.code)
				}
				return
			}
			if err != nil || resp.State != protocol.ContentGrantDenied || resp.Reason != c.reason {
				t.Fatalf("resp = %+v, %v; want denied %s", resp, err, c.reason)
			}
			again, err := s.Decide(context.Background(), tenant, device, req)
			if err != nil || again.GrantID != resp.GrantID || again.State != protocol.ContentGrantDenied || len(st.grants) != 1 {
				t.Fatalf("a denial was decided again: %+v, %v", again, err)
			}
		})
	}
}

func TestGrantIsReturnedAgainWhileOpen(t *testing.T) {
	s, st, _ := newService(t, m3Event())
	resp, err := s.Decide(context.Background(), tenant, device, grantRequest())
	if err != nil {
		t.Fatal(err)
	}
	if resp.State != protocol.ContentGrantGranted || resp.MaxBytes != protocol.MaxContentObjectBytes ||
		time.Until(resp.ExpiresAt) > GrantTTL || resp.ExpiresAt.Before(time.Now()) {
		t.Fatalf("resp = %+v", resp)
	}
	again, err := s.Decide(context.Background(), tenant, device, grantRequest())
	if err != nil || again.GrantID != resp.GrantID || len(st.grants) != 1 {
		t.Fatalf("an open grant was decided again: %+v, %v", again, err)
	}
	g := st.grants[resp.GrantID]
	used := time.Now()
	g.UsedAt = &used
	st.ec.Grant = &g
	if _, err := s.Decide(context.Background(), tenant, device, grantRequest()); code(err) != string(protocol.ReasonGrantConsumed) {
		t.Fatalf("a used grant: err = %v", err)
	}
}

func grantFor(t *testing.T) (*Service, *fakeStore, *fakeVault, string) {
	t.Helper()
	s, st, v := newService(t, m3Event())
	resp, err := s.Decide(context.Background(), tenant, device, grantRequest())
	if err != nil {
		t.Fatal(err)
	}
	return s, st, v, resp.GrantID
}

func TestUploadForwardsTheGrantedContentOnce(t *testing.T) {
	s, st, v, grantID := grantFor(t)
	body := []byte(`{"prompt":"summarise the contract"}`)
	u := Upload{GrantID: grantID, EventID: event, RawDigest: protocol.RawDigest(body), Body: body}
	if err := s.Upload(context.Background(), tenant, device, u); err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{tenant, event, grantID, protocol.RawDigest(body), string(body)}, "|")
	if len(v.puts) != 1 || v.puts[0] != want {
		t.Fatalf("vault puts = %q", v.puts)
	}
	if st.uploaded != int64(len(body)) {
		t.Fatalf("usage = %d, want %d", st.uploaded, len(body))
	}
	// A retry after the grant was used is forwarded (the vault answers it as a replay) and is not
	// counted again.
	g := st.grants[grantID]
	used := time.Now()
	g.UsedAt = &used
	st.grants[grantID] = g
	if err := s.Upload(context.Background(), tenant, device, u); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if len(v.puts) != 2 || st.uploaded != int64(len(body)) {
		t.Fatalf("retry: puts %d, usage %d", len(v.puts), st.uploaded)
	}
}

func TestUploadRefusals(t *testing.T) {
	body := []byte("content")
	good := func(grantID string) Upload {
		return Upload{GrantID: grantID, EventID: event, RawDigest: protocol.RawDigest(body), Body: body}
	}
	for _, c := range []struct {
		name   string
		device string
		mutate func(*Upload, *fakeStore, *fakeVault)
		code   string
	}{
		{name: "digest mismatch", mutate: func(u *Upload, _ *fakeStore, _ *fakeVault) { u.RawDigest = protocol.RawDigest([]byte("other")) }, code: apierr.CodeSchemaViolation},
		{name: "empty body", mutate: func(u *Upload, _ *fakeStore, _ *fakeVault) { u.Body = nil; u.RawDigest = protocol.RawDigest(nil) }, code: string(protocol.ReasonOversize)},
		{name: "bad grant id", mutate: func(u *Upload, _ *fakeStore, _ *fakeVault) { u.GrantID = "nope" }, code: apierr.CodeSchemaViolation},
		{name: "unknown grant", mutate: func(u *Upload, _ *fakeStore, _ *fakeVault) { u.GrantID = submission }, code: apierr.CodeNotFound},
		{name: "another device", device: "99999999-9999-4999-8999-999999999999", code: apierr.CodeNotFound},
		{name: "another event", mutate: func(u *Upload, _ *fakeStore, _ *fakeVault) { u.EventID = submission }, code: apierr.CodeNotFound},
		{name: "expired", mutate: func(u *Upload, st *fakeStore, _ *fakeVault) {
			g := st.grants[u.GrantID]
			g.ExpiresAt = time.Now().Add(-time.Second)
			st.grants[u.GrantID] = g
		}, code: string(protocol.ReasonGrantExpired)},
		{name: "denied", mutate: func(u *Upload, st *fakeStore, _ *fakeVault) {
			g := st.grants[u.GrantID]
			g.Decision, g.DenialReason = DecisionDenied, DenyStorageOff
			st.grants[u.GrantID] = g
		}, code: apierr.CodeForbidden},
		{name: "already stored", mutate: func(_ *Upload, _ *fakeStore, v *fakeVault) { v.err = &VaultError{Status: 409, Code: "already_stored"} }, code: string(protocol.ReasonGrantConsumed)},
		{name: "vault says expired", mutate: func(_ *Upload, _ *fakeStore, v *fakeVault) { v.err = &VaultError{Status: 403, Code: "grant_expired"} }, code: string(protocol.ReasonGrantExpired)},
		{name: "vault refuses the grant", mutate: func(_ *Upload, _ *fakeStore, v *fakeVault) {
			v.err = &VaultError{Status: 403, Code: "grant_not_granted"}
		}, code: apierr.CodeForbidden},
		{name: "vault 500", mutate: func(_ *Upload, _ *fakeStore, v *fakeVault) { v.err = &VaultError{Status: 500} }, code: apierr.CodeUnavailable},
		{name: "vault down", mutate: func(_ *Upload, _ *fakeStore, v *fakeVault) { v.err = errors.New("connection refused") }, code: apierr.CodeUnavailable},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, st, v, grantID := grantFor(t)
			u := good(grantID)
			if c.mutate != nil {
				c.mutate(&u, st, v)
			}
			dev := device
			if c.device != "" {
				dev = c.device
			}
			if err := s.Upload(context.Background(), tenant, dev, u); code(err) != c.code {
				t.Fatalf("err = %v, want %s", err, c.code)
			}
			if st.uploaded != 0 {
				t.Fatal("a refused upload was counted")
			}
		})
	}
}

type staticTokens struct{}

func (staticTokens) ServiceToken(audience, service string) (string, error) {
	return "svc." + audience + "." + service, nil
}

func TestHTTPVaultPutsWithAServiceToken(t *testing.T) {
	var got *http.Request
	var gotBody []byte
	status := http.StatusCreated
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(status)
		if status == http.StatusConflict {
			_, _ = w.Write([]byte(`{"error":{"code":"grant_consumed"}}`))
		}
	}))
	defer srv.Close()
	v := NewHTTPVault(srv.URL+"/", staticTokens{})
	body := []byte("content")
	if stored, err := v.Put(context.Background(), tenant, event, submission, protocol.RawDigest(body), body); err != nil || !stored {
		t.Fatalf("201: stored %v, %v", stored, err)
	}
	if got.Method != http.MethodPut || got.URL.Path != "/internal/v1/tenants/"+tenant+"/content/"+event {
		t.Fatalf("request = %s %s", got.Method, got.URL.Path)
	}
	if got.Header.Get("Authorization") != "Bearer svc.sac-vault.control-api" ||
		got.Header.Get(protocol.HeaderContentGrantID) != submission ||
		got.Header.Get(protocol.HeaderContentRawDigest) != protocol.RawDigest(body) || !bytes.Equal(gotBody, body) {
		t.Fatalf("headers = %v, body %q", got.Header, gotBody)
	}
	status = http.StatusOK
	if stored, err := v.Put(context.Background(), tenant, event, submission, protocol.RawDigest(body), body); err != nil || stored {
		t.Fatalf("200 replay: stored %v, %v", stored, err)
	}
	status = http.StatusConflict
	var ve *VaultError
	if _, err := v.Put(context.Background(), tenant, event, submission, protocol.RawDigest(body), body); !errors.As(err, &ve) ||
		ve.Status != http.StatusConflict || ve.Code != "grant_consumed" {
		t.Fatalf("409: err = %v", err)
	}
}
