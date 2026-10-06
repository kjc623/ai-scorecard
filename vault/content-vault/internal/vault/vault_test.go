package vault_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/protocol"

	"github.com/shadow-ai-capture/content-vault/internal/keyring"
	"github.com/shadow-ai-capture/content-vault/internal/store"
	"github.com/shadow-ai-capture/content-vault/internal/store/storetest"
	"github.com/shadow-ai-capture/content-vault/internal/vault"
)

const (
	tenantID     = "7d3c6a52-0b8e-4f0e-9a51-2a4c1f6b9e01"
	otherTenant  = "c2b1f0e4-5d6a-4b7c-8e9f-0a1b2c3d4e5f"
	eventID      = "0f8e7d6c-5b4a-4392-8170-6f5e4d3c2b1a"
	grantID      = "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d"
	submissionID = "2b3c4d5e-6f7a-4b8c-9d0e-1f2a3b4c5d6e"
	deviceID     = "3c4d5e6f-7a8b-4c9d-8e1f-2a3b4c5d6e7f"
)

var t0 = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

type rig struct {
	t     *testing.T
	store *storetest.Fake
	keys  *keyring.Keyring
	svc   *vault.Service
	now   time.Time
}

func newRig(t *testing.T, tier store.SearchTier) *rig {
	t.Helper()
	r := &rig{t: t, store: storetest.New(), now: t0}
	r.keys = mustKeys(t, "v1:"+b64(1))
	r.store.PutTenant(store.Tenant{TenantID: tenantID, Status: "active", ContentSearch: tier, IngestEnabled: true, ReadEnabled: true})
	r.store.PutSubmission(storetest.Submission{TenantID: tenantID, SubmissionID: submissionID, PromptKind: "user", ReceivedAt: t0.Add(-time.Minute)})
	r.store.PutGrant(tenantID, store.Grant{
		GrantID: grantID, EventID: eventID, DeviceID: deviceID, SubmissionID: submissionID,
		Decision: "granted", ExpiresAt: t0.Add(10 * time.Minute),
	})
	r.build(r.keys)
	return r
}

func (r *rig) build(keys *keyring.Keyring) {
	svc, err := vault.New(vault.Config{Store: r.store, Keys: keys, RetrievalURLBase: "https://analyst.example/", Now: func() time.Time { return r.now }})
	if err != nil {
		r.t.Fatal(err)
	}
	r.svc = svc
}

func b64(fill byte) string {
	return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{fill}, keyring.KeySize))
}

func mustKeys(t *testing.T, spec string) *keyring.Keyring {
	t.Helper()
	k, err := keyring.Parse(spec)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func upload(content []byte) vault.UploadRequest {
	return vault.UploadRequest{TenantID: tenantID, EventID: eventID, GrantID: grantID, RawDigest: protocol.RawDigest(content), Content: content}
}

func wantDenial(t *testing.T, err error, reason vault.Reason) {
	t.Helper()
	d, ok := vault.AsDenial(err)
	if !ok || d.Reason != reason {
		t.Fatalf("error = %v, want denial %s", err, reason)
	}
}

func actions(f *storetest.Fake) []string {
	var out []string
	for _, a := range f.Audit() {
		out = append(out, a.Action)
	}
	return out
}

var ctx = context.Background()

func TestUploadStoresEncryptedContent(t *testing.T) {
	r := newRig(t, store.SearchFullText)
	content := []byte("summarise the Q3 board pack")
	res, err := r.svc.Upload(ctx, upload(content))
	if err != nil {
		t.Fatal(err)
	}
	if res.Replayed || res.EventID != eventID || res.GrantID != grantID || res.SizeBytes != len(content) {
		t.Fatalf("result = %+v", res)
	}
	if want := t0.AddDate(0, 0, 30); !res.ExpiresAt.Equal(want) {
		t.Fatalf("expires_at = %s, want now + the tenant's 30-day retention = %s", res.ExpiresAt, want)
	}
	stored := r.store.Content(tenantID)
	if len(stored) != 1 {
		t.Fatalf("%d objects stored, want 1", len(stored))
	}
	c := stored[0]
	if bytes.Contains(c.Ciphertext, content) || c.KeyVersion != "v1" || c.RawDigest != protocol.RawDigest(content) ||
		c.SubmissionID != submissionID || c.PromptKind != "user" || c.RetentionClass != "content" || c.ObjectID != res.ObjectID {
		t.Fatalf("stored row = %+v", c)
	}
	plaintext, err := r.keys.Open(tenantID, c.ObjectID, c.KeyVersion, c.Ciphertext)
	if err != nil || !bytes.Equal(plaintext, content) {
		t.Fatalf("stored ciphertext does not open to the upload: %q, %v", plaintext, err)
	}
	if g := r.store.Grant(tenantID, grantID); !g.UsedAt.Equal(t0) {
		t.Fatalf("grant used_at = %s, want %s", g.UsedAt, t0)
	}
	if s := r.store.Submission(tenantID, submissionID); s.ContentState != "uploaded" {
		t.Fatalf("submission content_state = %q, want uploaded", s.ContentState)
	}
	audit := r.store.Audit()
	if len(audit) != 1 || audit[0].Action != vault.ActionContentStored || audit[0].ActorType != "device" || audit[0].ActorID != deviceID {
		t.Fatalf("audit = %+v", audit)
	}
	if strings.Contains(strings.ToLower(audit[0].Detail["raw_digest"].(string)), "q3") {
		t.Fatal("the audit row carries content")
	}
}

func TestARetryOfTheSameUploadSucceedsWithoutWriting(t *testing.T) {
	r := newRig(t, store.SearchFullText)
	content := []byte("the same prompt")
	first, err := r.svc.Upload(ctx, upload(content))
	if err != nil {
		t.Fatal(err)
	}
	r.now = t0.Add(time.Hour) // after the grant expired: a retry still succeeds
	again, err := r.svc.Upload(ctx, upload(content))
	if err != nil {
		t.Fatal(err)
	}
	if !again.Replayed || again.ObjectID != first.ObjectID || again.RawDigest != first.RawDigest {
		t.Fatalf("retry = %+v, want the first object replayed", again)
	}
	if n := len(r.store.Content(tenantID)); n != 1 {
		t.Fatalf("%d objects after a retry, want 1", n)
	}
	if got := actions(r.store); len(got) != 1 {
		t.Fatalf("audit after a retry = %v, want the one content_stored row", got)
	}
}

func TestADifferentBodyUnderAUsedGrantIsRefused(t *testing.T) {
	r := newRig(t, store.SearchFullText)
	if _, err := r.svc.Upload(ctx, upload([]byte("first"))); err != nil {
		t.Fatal(err)
	}
	_, err := r.svc.Upload(ctx, upload([]byte("second")))
	wantDenial(t, err, vault.ReasonAlreadyStored)
}

func TestASecondGrantCannotReplaceStoredContent(t *testing.T) {
	r := newRig(t, store.SearchFullText)
	if _, err := r.svc.Upload(ctx, upload([]byte("first"))); err != nil {
		t.Fatal(err)
	}
	const second = "4d5e6f7a-8b9c-4d0e-9f1a-2b3c4d5e6f7a"
	r.store.PutGrant(tenantID, store.Grant{GrantID: second, EventID: eventID, DeviceID: deviceID, Decision: "granted", ExpiresAt: t0.Add(time.Hour)})
	req := upload([]byte("replacement"))
	req.GrantID = second
	_, err := r.svc.Upload(ctx, req)
	wantDenial(t, err, vault.ReasonAlreadyStored)
	if !r.store.Grant(tenantID, second).UsedAt.IsZero() {
		t.Fatal("the refused upload's grant claim was committed")
	}
}

func TestUploadRefusals(t *testing.T) {
	cases := map[string]struct {
		edit   func(*rig, *vault.UploadRequest)
		reason vault.Reason
	}{
		"digest mismatch": {func(_ *rig, q *vault.UploadRequest) { q.RawDigest = protocol.RawDigest([]byte("other")) }, vault.ReasonInvalidRequest},
		"oversize": {func(_ *rig, q *vault.UploadRequest) {
			q.Content = make([]byte, protocol.MaxContentObjectBytes+1)
			q.RawDigest = protocol.RawDigest(q.Content)
		}, vault.ReasonInvalidRequest},
		"unknown grant":         {func(_ *rig, q *vault.UploadRequest) { q.GrantID = "5e6f7a8b-9c0d-4e1f-8a2b-3c4d5e6f7a8b" }, vault.ReasonGrantUnknown},
		"grant for other event": {func(_ *rig, q *vault.UploadRequest) { q.EventID = "6f7a8b9c-0d1e-4f2a-9b3c-4d5e6f7a8b9c" }, vault.ReasonGrantUnknown},
		"grant in other tenant": {func(r *rig, q *vault.UploadRequest) {
			r.store.PutTenant(store.Tenant{TenantID: otherTenant, Status: "active", IngestEnabled: true, ReadEnabled: true})
			q.TenantID = otherTenant
		}, vault.ReasonGrantUnknown},
		"denied grant": {func(r *rig, _ *vault.UploadRequest) {
			g := r.store.Grant(tenantID, grantID)
			g.Decision = "denied"
			r.store.PutGrant(tenantID, g)
		}, vault.ReasonGrantNotGranted},
		"expired grant": {func(r *rig, _ *vault.UploadRequest) { r.now = t0.Add(10 * time.Minute) }, vault.ReasonGrantExpired},
		"used grant, content since deleted": {func(r *rig, _ *vault.UploadRequest) {
			g := r.store.Grant(tenantID, grantID)
			g.UsedAt = t0.Add(-time.Minute)
			r.store.PutGrant(tenantID, g)
		}, vault.ReasonGrantConsumed},
		"unknown tenant": {func(_ *rig, q *vault.UploadRequest) { q.TenantID = otherTenant }, vault.ReasonTenantNotPermitted},
		"ingest disabled": {func(r *rig, _ *vault.UploadRequest) {
			r.store.PutTenant(store.Tenant{TenantID: tenantID, Status: "suspended", ContentSearch: store.SearchFullText, ReadEnabled: true})
		}, vault.ReasonTenantNotPermitted},
		"offboarding": {func(r *rig, _ *vault.UploadRequest) {
			r.store.PutTenant(store.Tenant{TenantID: tenantID, Status: "offboarding", IngestEnabled: true, ReadEnabled: true})
		}, vault.ReasonTenantNotPermitted},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, store.SearchFullText)
			req := upload([]byte("prompt"))
			tc.edit(r, &req)
			_, err := r.svc.Upload(ctx, req)
			wantDenial(t, err, tc.reason)
			if n := len(r.store.Content(tenantID)) + len(r.store.Content(otherTenant)); n != 0 {
				t.Fatalf("%d objects stored by a refused upload", n)
			}
			if len(r.store.Audit()) != 0 {
				t.Fatal("a refused upload wrote an audit row")
			}
		})
	}
}

func TestNothingCommitsWithoutTheAuditRow(t *testing.T) {
	r := newRig(t, store.SearchFullText)
	r.store.FailAudit = true
	if _, err := r.svc.Upload(ctx, upload([]byte("prompt"))); err == nil {
		t.Fatal("the upload succeeded without its audit row")
	}
	if len(r.store.Content(tenantID)) != 0 || !r.store.Grant(tenantID, grantID).UsedAt.IsZero() || len(r.store.SearchRows(tenantID)) != 0 ||
		r.store.Submission(tenantID, submissionID).ContentState != "" {
		t.Fatal("content, the grant claim, the content state or index rows committed without the audit row")
	}
}

func TestAShreddedSubmissionStaysShredded(t *testing.T) {
	r := newRig(t, store.SearchFullText)
	r.store.PutSubmission(storetest.Submission{TenantID: tenantID, SubmissionID: submissionID, PromptKind: "user", ContentState: "shredded"})
	if _, err := r.svc.Upload(ctx, upload([]byte("late upload"))); err != nil {
		t.Fatal(err)
	}
	if s := r.store.Submission(tenantID, submissionID); s.ContentState != "shredded" {
		t.Fatalf("content_state = %q, want shredded left alone", s.ContentState)
	}
}

func TestIndexingFollowsTheTenantsTier(t *testing.T) {
	object := []byte(`{"prompt":"merger plan for Contoso","attachments":[{"name":"Q3-board.pdf","size_bytes":10},{"name":"terms.docx"}]}`)
	cases := map[store.SearchTier][]string{
		store.SearchFullText:        {"attachment_name:0:Q3-board.pdf", "attachment_name:1:terms.docx", "prompt_body:0:merger plan for Contoso"},
		store.SearchAttachmentNames: {"attachment_name:0:Q3-board.pdf", "attachment_name:1:terms.docx"},
		store.SearchDisabled:        nil,
	}
	for tier, want := range cases {
		t.Run(string(tier), func(t *testing.T) {
			r := newRig(t, tier)
			res, err := r.svc.Upload(ctx, upload(object))
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, u := range r.store.SearchRows(tenantID) {
				got = append(got, u.UnitKind+":"+string(rune('0'+u.UnitIndex))+":"+u.Body)
				if !u.ExpiresAt.Equal(res.ExpiresAt) {
					t.Errorf("index row expires %s, the content %s", u.ExpiresAt, res.ExpiresAt)
				}
			}
			if strings.Join(got, "|") != strings.Join(want, "|") {
				t.Fatalf("index rows = %v, want %v", got, want)
			}
			if res.IndexedAttachmentNames+res.IndexedPrompt != len(want) {
				t.Fatalf("result counts %d+%d rows, %d written", res.IndexedPrompt, res.IndexedAttachmentNames, len(want))
			}
		})
	}
}

func TestAClientGeneratedRequestIsNeverIndexed(t *testing.T) {
	r := newRig(t, store.SearchFullText)
	r.store.PutSubmission(storetest.Submission{TenantID: tenantID, SubmissionID: submissionID, PromptKind: "client_generated"})
	res, err := r.svc.Upload(ctx, upload([]byte(`{"prompt":"generate a title","attachments":[{"name":"a.txt"}]}`)))
	if err != nil {
		t.Fatal(err)
	}
	if rows := r.store.SearchRows(tenantID); len(rows) != 0 || res.IndexedPrompt+res.IndexedAttachmentNames != 0 {
		t.Fatalf("client-generated content was indexed: %+v", rows)
	}
	if c := r.store.Content(tenantID); len(c) != 1 || c[0].PromptKind != "client_generated" {
		t.Fatal("client-generated content is still stored, with its kind")
	}
}

func TestContentWithoutAKnownSubmissionIsStoredButNotIndexed(t *testing.T) {
	r := newRig(t, store.SearchFullText)
	g := r.store.Grant(tenantID, grantID)
	g.SubmissionID = ""
	r.store.PutGrant(tenantID, g)
	if _, err := r.svc.Upload(ctx, upload([]byte("typed text"))); err != nil {
		t.Fatal(err)
	}
	if len(r.store.Content(tenantID)) != 1 || len(r.store.SearchRows(tenantID)) != 0 {
		t.Fatal("want the object stored and no index rows")
	}
}

func TestTheIndexHoldsWhatThePersonTyped(t *testing.T) {
	r := newRig(t, store.SearchFullText)
	body := `{"model":"x","system":"You are helpful","messages":[
		{"role":"user","content":"first question"},
		{"role":"assistant","content":"answer"},
		{"role":"user","content":[{"type":"text","text":"<system-reminder>ctx</system-reminder>follow-up about invoices"},{"type":"image"}]}]}`
	if _, err := r.svc.Upload(ctx, upload([]byte(body))); err != nil {
		t.Fatal(err)
	}
	rows := r.store.SearchRows(tenantID)
	if len(rows) != 1 || rows[0].Body != "follow-up about invoices" {
		t.Fatalf("index rows = %+v, want only this turn's typed text", rows)
	}
}

// --- retrieval -----------------------------------------------------------------------------

func stored(t *testing.T, r *rig, content []byte) vault.UploadResult {
	t.Helper()
	res, err := r.svc.Upload(ctx, upload(content))
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func retrieve() vault.RetrieveRequest {
	return vault.RetrieveRequest{TenantID: tenantID, EventID: eventID, Principal: "alice@example.com", CaseReference: "CASE-7", SecondApprover: "bob@example.com", SessionID: "abcdef0123"}
}

func TestRetrievalIsMintedAuditedAndRedeemedOnce(t *testing.T) {
	r := newRig(t, store.SearchFullText)
	content := []byte("the prompt")
	up := stored(t, r, content)

	res, err := r.svc.Retrieve(ctx, retrieve())
	if err != nil {
		t.Fatal(err)
	}
	if res.RawDigest != up.RawDigest || !res.ExpiresAt.Equal(t0.Add(5*time.Minute)) {
		t.Fatalf("retrieval = %+v", res)
	}
	if want := "https://analyst.example/v1/content/retrieval/" + tenantID + "/" + res.GrantID; res.RetrievalURL != want {
		t.Fatalf("retrieval URL = %q, want %q", res.RetrievalURL, want)
	}
	audit := r.store.Audit()
	granted := audit[len(audit)-1]
	if granted.Action != vault.ActionRetrievalGranted || granted.ActorID != "alice@example.com" || granted.CaseReference != "CASE-7" ||
		granted.Detail["second_approver"] != "bob@example.com" || granted.Detail["sid"] != "abcdef0123" {
		t.Fatalf("grant audit = %+v", granted)
	}

	got, err := r.svc.Redeem(ctx, tenantID, res.GrantID)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Plaintext, content) || got.RawDigest != up.RawDigest || got.EventID != eventID {
		t.Fatalf("redeemed = %+v", got)
	}
	if last := r.store.Audit()[len(r.store.Audit())-1]; last.Action != vault.ActionRetrievalRedeemed || last.Detail["outcome"] != "served" {
		t.Fatalf("redemption audit = %+v", last)
	}

	_, err = r.svc.Redeem(ctx, tenantID, res.GrantID)
	wantDenial(t, err, vault.ReasonGrantAlreadyUsed)
}

func TestRetrievalRefusalsAreAudited(t *testing.T) {
	r := newRig(t, store.SearchFullText)
	stored(t, r, []byte("x"))

	self := retrieve()
	self.SecondApprover = self.Principal
	_, err := r.svc.Retrieve(ctx, self)
	wantDenial(t, err, vault.ReasonSecondApproverNotDistinct)

	missing := retrieve()
	missing.EventID = "7a8b9c0d-1e2f-4a3b-8c4d-5e6f7a8b9c0d"
	_, err = r.svc.Retrieve(ctx, missing)
	wantDenial(t, err, vault.ReasonNoContentObject)

	got := actions(r.store)
	if strings.Join(got[1:], ",") != vault.ActionRetrievalRefused+","+vault.ActionRetrievalRefused {
		t.Fatalf("audit = %v, want both refusals recorded", got)
	}
}

func TestRetrievalWithoutAnApproverIsAllowed(t *testing.T) {
	r := newRig(t, store.SearchFullText)
	stored(t, r, []byte("x"))
	req := retrieve()
	req.SecondApprover, req.CaseReference = "", ""
	if _, err := r.svc.Retrieve(ctx, req); err != nil {
		t.Fatal(err)
	}
}

func TestExpiredContentIsUnavailableWithItsReceipt(t *testing.T) {
	r := newRig(t, store.SearchFullText)
	up := stored(t, r, []byte("x"))
	r.store.PutReceipt(tenantID, "8b9c0d1e-2f3a-4b4c-9d5e-6f7a8b9c0d1e")
	r.now = up.ExpiresAt
	_, err := r.svc.Retrieve(ctx, retrieve())
	u, ok := vault.AsUnavailable(err)
	if !ok || u.Reason != vault.UnavailableRetentionExpired || u.ReceiptRef != "8b9c0d1e-2f3a-4b4c-9d5e-6f7a8b9c0d1e" {
		t.Fatalf("error = %v, want retention_expired with the receipt", err)
	}
	if last := actions(r.store); last[len(last)-1] != vault.ActionRetrievalRefused {
		t.Fatal("the unavailability was not audited")
	}
}

func TestContentUnderARetiredKeyIsUnavailable(t *testing.T) {
	r := newRig(t, store.SearchFullText)
	stored(t, r, []byte("x"))
	r.build(mustKeys(t, "v2:"+b64(2)))
	_, err := r.svc.Retrieve(ctx, retrieve())
	if u, ok := vault.AsUnavailable(err); !ok || u.Reason != vault.UnavailableKeyUnavailable {
		t.Fatalf("error = %v, want key_unavailable", err)
	}
}

func TestContentSurvivesKeyRotation(t *testing.T) {
	r := newRig(t, store.SearchFullText)
	stored(t, r, []byte("before rotation"))
	r.build(mustKeys(t, "v2:"+b64(2)+",v1:"+b64(1)))
	res, err := r.svc.Retrieve(ctx, retrieve())
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.svc.Redeem(ctx, tenantID, res.GrantID)
	if err != nil || string(got.Plaintext) != "before rotation" {
		t.Fatalf("redeem after rotation = %q, %v", got.Plaintext, err)
	}
}

func TestRedemptionRefusals(t *testing.T) {
	r := newRig(t, store.SearchFullText)
	stored(t, r, []byte("x"))
	res, err := r.svc.Retrieve(ctx, retrieve())
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.svc.Redeem(ctx, tenantID, "9c0d1e2f-3a4b-4c5d-8e6f-7a8b9c0d1e2f")
	wantDenial(t, err, vault.ReasonGrantRequired)
	_, err = r.svc.Redeem(ctx, otherTenant, res.GrantID)
	wantDenial(t, err, vault.ReasonGrantRequired)

	r.now = res.ExpiresAt
	_, err = r.svc.Redeem(ctx, tenantID, res.GrantID)
	wantDenial(t, err, vault.ReasonGrantExpired)
}

func TestContentDeletedAfterTheGrantIsUnavailable(t *testing.T) {
	r := newRig(t, store.SearchFullText)
	up := stored(t, r, []byte("x"))
	res, _ := r.svc.Retrieve(ctx, retrieve())
	r.store.EditContent(tenantID, up.ObjectID, func(*store.Content) bool { return false })
	_, err := r.svc.Redeem(ctx, tenantID, res.GrantID)
	if u, ok := vault.AsUnavailable(err); !ok || u.Reason != vault.UnavailableErasure {
		t.Fatalf("error = %v, want erasure", err)
	}
	_, err = r.svc.Redeem(ctx, tenantID, res.GrantID)
	wantDenial(t, err, vault.ReasonGrantAlreadyUsed)
}

func TestTamperedCiphertextIsAFaultAndConsumesNothing(t *testing.T) {
	r := newRig(t, store.SearchFullText)
	up := stored(t, r, []byte("x"))
	res, _ := r.svc.Retrieve(ctx, retrieve())
	r.store.EditContent(tenantID, up.ObjectID, func(c *store.Content) bool {
		c.Ciphertext = bytes.Clone(c.Ciphertext)
		c.Ciphertext[len(c.Ciphertext)-1] ^= 1
		return true
	})
	_, err := r.svc.Redeem(ctx, tenantID, res.GrantID)
	if err == nil {
		t.Fatal("tampered ciphertext was served")
	}
	if _, ok := vault.AsDenial(err); ok {
		t.Fatal("tampering was reported as a refusal rather than a fault")
	}
	if _, ok := vault.AsUnavailable(err); ok {
		t.Fatal("tampering was reported as content that is gone")
	}
	r.store.EditContent(tenantID, up.ObjectID, func(c *store.Content) bool {
		c.Ciphertext[len(c.Ciphertext)-1] ^= 1
		return true
	})
	if _, err := r.svc.Redeem(ctx, tenantID, res.GrantID); err != nil {
		t.Fatalf("the failed redemption consumed the grant: %v", err)
	}
}

func TestNothingIsServedWithoutTheAuditRow(t *testing.T) {
	r := newRig(t, store.SearchFullText)
	stored(t, r, []byte("x"))
	res, _ := r.svc.Retrieve(ctx, retrieve())
	r.store.FailAudit = true
	if got, err := r.svc.Redeem(ctx, tenantID, res.GrantID); err == nil || got.Plaintext != nil {
		t.Fatal("content was served without its audit row")
	}
	if _, err := r.svc.Retrieve(ctx, retrieve()); err == nil {
		t.Fatal("a retrieval grant was minted without its audit row")
	}
	r.store.FailAudit = false
	if _, err := r.svc.Redeem(ctx, tenantID, res.GrantID); err != nil {
		t.Fatalf("the grant was consumed by the failed redemption: %v", err)
	}
}

func TestReadsCanBeSwitchedOff(t *testing.T) {
	r := newRig(t, store.SearchFullText)
	stored(t, r, []byte("x"))
	res, _ := r.svc.Retrieve(ctx, retrieve())
	r.store.PutTenant(store.Tenant{TenantID: tenantID, Status: "suspended", ContentSearch: store.SearchFullText, IngestEnabled: true})
	_, err := r.svc.Retrieve(ctx, retrieve())
	wantDenial(t, err, vault.ReasonRetrievalDisabled)
	_, err = r.svc.Redeem(ctx, tenantID, res.GrantID)
	wantDenial(t, err, vault.ReasonRetrievalDisabled)
	_, err = r.svc.Search(ctx, vault.SearchRequest{TenantID: tenantID, Principal: "alice", Query: "x"})
	wantDenial(t, err, vault.ReasonRetrievalDisabled)
}

// --- search --------------------------------------------------------------------------------

func searchRig(t *testing.T, tier store.SearchTier) *rig {
	t.Helper()
	r := newRig(t, tier)
	expires := t0.Add(24 * time.Hour)
	r.store.PutSearchRow(tenantID, store.SearchUnit{SubmissionID: submissionID, UnitKind: store.UnitPromptBody, Body: "contract renewal for contoso", ExpiresAt: expires})
	r.store.PutSearchRow(tenantID, store.SearchUnit{SubmissionID: submissionID, UnitKind: store.UnitAttachmentName, Body: "contoso-contract.pdf", ExpiresAt: expires})
	return r
}

func search(r *rig, form store.SearchForm, query string) (vault.SearchResult, error) {
	return r.svc.Search(ctx, vault.SearchRequest{TenantID: tenantID, Principal: "alice@example.com", SessionID: "abcdef0123", Form: form, Query: query, CaseReference: "CASE-1"})
}

func kinds(res vault.SearchResult) string {
	var out []string
	for _, h := range res.Hits {
		out = append(out, h.UnitKind)
	}
	return strings.Join(out, ",")
}

func TestSearchFollowsTheTenantsTier(t *testing.T) {
	cases := []struct {
		tier store.SearchTier
		form store.SearchForm
		want string
	}{
		{store.SearchFullText, store.FormTerms, "prompt_body,attachment_name"},
		{store.SearchFullText, "", "prompt_body,attachment_name"},
		{store.SearchFullText, store.FormSubstring, "attachment_name"},
		{store.SearchFullText, store.FormFuzzy, "attachment_name"},
		{store.SearchAttachmentNames, store.FormTerms, "attachment_name"},
		{store.SearchAttachmentNames, store.FormSubstring, "attachment_name"},
	}
	for _, tc := range cases {
		r := searchRig(t, tc.tier)
		res, err := search(r, tc.form, "contoso")
		if err != nil {
			t.Fatalf("%s/%s: %v", tc.tier, tc.form, err)
		}
		if got := kinds(res); got != tc.want || res.Tier != tc.tier {
			t.Errorf("%s/%s: hits %q (tier %s), want %q", tc.tier, tc.form, got, res.Tier, tc.want)
		}
	}
	r := searchRig(t, store.SearchDisabled)
	_, err := search(r, store.FormTerms, "contoso")
	wantDenial(t, err, vault.ReasonSearchDisabled)
}

func TestSearchIsAuditedInTheSameTransaction(t *testing.T) {
	r := searchRig(t, store.SearchFullText)
	if _, err := search(r, store.FormTerms, `contoso "contract renewal"`); err != nil {
		t.Fatal(err)
	}
	a := r.store.Audit()
	last := a[len(a)-1]
	if last.Action != vault.ActionSearch || last.ActorID != "alice@example.com" || last.CaseReference != "CASE-1" ||
		last.Detail["terms"] != "contoso & contract <-> renewal" || last.Detail["sid"] != "abcdef0123" {
		t.Fatalf("search audit = %+v", last)
	}
	r.store.FailAudit = true
	if res, err := search(r, store.FormTerms, "contoso"); err == nil || len(res.Hits) != 0 {
		t.Fatal("search results were served without an audit row")
	}
}

func TestSearchPagesNewestFirst(t *testing.T) {
	r := newRig(t, store.SearchFullText)
	for i, sub := range []string{"a0000000-0000-4000-8000-000000000001", "a0000000-0000-4000-8000-000000000002", "a0000000-0000-4000-8000-000000000003"} {
		r.store.PutSubmission(storetest.Submission{TenantID: tenantID, SubmissionID: sub, ReceivedAt: t0.Add(-time.Duration(i) * time.Hour)})
		r.store.PutSearchRow(tenantID, store.SearchUnit{SubmissionID: sub, UnitKind: store.UnitPromptBody, Body: "invoice", ExpiresAt: t0.Add(time.Hour)})
	}
	var seen []string
	cursor := ""
	for page := 0; page < 3; page++ {
		res, err := r.svc.Search(ctx, vault.SearchRequest{TenantID: tenantID, Principal: "alice", Query: "invoice", Limit: 2, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		for _, h := range res.Hits {
			seen = append(seen, h.SubmissionID[len(h.SubmissionID)-1:])
		}
		if cursor = res.NextCursor; cursor == "" {
			break
		}
	}
	if strings.Join(seen, "") != "123" {
		t.Fatalf("pages returned %v, want submissions 1,2,3 newest first exactly once", seen)
	}
	_, err := r.svc.Search(ctx, vault.SearchRequest{TenantID: tenantID, Principal: "alice", Query: "invoice", Cursor: "not-a-cursor"})
	wantDenial(t, err, vault.ReasonSearchCursorInvalid)
}

func TestExpiredIndexRowsAreNotReturned(t *testing.T) {
	r := searchRig(t, store.SearchFullText)
	r.now = t0.Add(25 * time.Hour)
	res, err := search(r, store.FormTerms, "contoso")
	if err != nil || len(res.Hits) != 0 {
		t.Fatalf("hits = %v, %v; want none past the rows' expiry", res.Hits, err)
	}
}

func TestSearchInputIsValidated(t *testing.T) {
	r := searchRig(t, store.SearchFullText)
	for _, q := range []string{"", "   ", "&|!()", strings.Repeat("a", 257)} {
		_, err := search(r, store.FormTerms, q)
		wantDenial(t, err, vault.ReasonInvalidRequest)
	}
	_, err := search(r, "regex", "contoso")
	wantDenial(t, err, vault.ReasonInvalidRequest)
}

func TestNewNeedsAStoreAndAKeyring(t *testing.T) {
	if _, err := vault.New(vault.Config{Keys: mustKeys(t, "v1:"+b64(1))}); err == nil {
		t.Fatal("built without a store")
	}
	if _, err := vault.New(vault.Config{Store: storetest.New()}); err == nil {
		t.Fatal("built without a keyring")
	}
}

func TestErrorsAreDistinguishable(t *testing.T) {
	var d *vault.Denial
	if errors.As(&vault.Unavailable{}, &d) {
		t.Fatal("an unavailability reads as a denial")
	}
}
