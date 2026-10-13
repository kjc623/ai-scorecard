package providers

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/protocol"
	"github.com/shadow-ai-capture/device/protocol/canon"

	"github.com/shadow-ai-capture/control-api/internal/store"
)

var conn = Connection{TenantID: "11111111-1111-4111-8111-111111111111", ConnectionID: "c0000000-0000-4000-8000-000000000001", Provider: ClaudeEnterprise}

func prompt() Record {
	return Record{
		ID: "msg_1", OccurredAt: time.Date(2026, 10, 12, 9, 30, 0, 0, time.UTC), App: "claude_web",
		UPN: "Ada@Contoso.example", Text: "Summarise  this for a@b.example",
		Attachments: []canon.Attachment{{Name: "q3-forecast.xlsx", MediaType: "application/vnd.ms-excel", SizeBytes: 2048, ContentDigest: canon.Unreadable}},
	}
}

func record(t *testing.T, p Policy, rec Record) (*fakeStore, *fakeClassifier, *fakeVault, string) {
	t.Helper()
	st, cl, v := newFakeStore(), &fakeClassifier{}, &fakeVault{}
	r := &Recorder{Store: st, Classifier: cl, Vault: v, Now: func() time.Time { return time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC) }}
	outcome, err := r.Record(context.Background(), conn, p, testRefKey, rec)
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	return st, cl, v, outcome
}

func TestARecordAtM1IsClassifiedAndAttributedAsADeviceCaptureIs(t *testing.T) {
	st, cl, v, outcome := record(t, Policy{CeilingMode: "m3", Mode: "m1", ContentSearch: "full_text"}, prompt())
	if outcome != OutcomeInserted || len(st.events) != 1 {
		t.Fatalf("outcome %q, %d events", outcome, len(st.events))
	}
	env := st.events[0]
	wantRef, _ := protocol.DeriveUserRef(testRefKey, protocol.UserRefUPN, "ada@contoso.example")
	for k, want := range map[string]any{
		"schema_version": "1.0", "tenant_id": conn.TenantID, "connection_id": conn.ConnectionID,
		"event_id": EventID(conn.ConnectionID, "msg_1"), "user_ref": wantRef, "subject_name": "Ada@Contoso.example",
		"tool_fingerprint": "app:claude_web", "direction": "egress", "kind": "prompt", "source": "provider.api",
		"collection_mode": "m1", "occurred_at": "2026-10-12T09:30:00Z", "size_bytes": float64(len(prompt().Text)),
		"content_digest": canon.Digest(prompt().Text, prompt().Attachments), "classifier_version": "rules-1.0.0+model-1.0.0",
		"confidence": "high", "prompt_kind": "user", "monotonic_offset_ms": float64(0),
	} {
		if env[k] != want {
			t.Errorf("%s = %v, want %v", k, env[k], want)
		}
	}
	if _, ok := env["device_id"]; ok {
		t.Error("a provider's record names a device")
	}
	if _, ok := env["content_excerpt"]; ok {
		t.Error("an M1 record carries an excerpt")
	}
	d, _ := env["policy_decision"].(map[string]any)
	if d["rule_id"] != DefaultRuleID || d["action"] != "logged" || d["decided_locally"] != false {
		t.Errorf("policy decision %v", d)
	}
	atts, _ := env["attachments"].([]any)
	if len(atts) != 1 || atts[0].(map[string]any)["name"] != "q3-forecast.xlsx" {
		t.Errorf("attachments %v", env["attachments"])
	}
	if len(cl.requests) != 1 || string(cl.requests[0].Content) != prompt().Text || cl.requests[0].Mode != protocol.ModeM1 {
		t.Errorf("classifier asked %+v", cl.requests)
	}
	if len(st.grants) != 0 || len(v.puts) != 0 {
		t.Error("an M1 prompt's content was stored")
	}
}

func TestTheSameRecordIsTheSameEventAndAnotherIsNot(t *testing.T) {
	a, b := EventID(conn.ConnectionID, "msg_1"), EventID(conn.ConnectionID, "msg_1")
	if a != b || !store.IsUUID(a) {
		t.Fatalf("event ids %q %q", a, b)
	}
	if EventID(conn.ConnectionID, "msg_2") == a || EventID("c0000000-0000-4000-8000-000000000002", "msg_1") == a {
		t.Fatal("another record or another connection made the same event id")
	}
	if k := dedupKey(conn.ConnectionID, "msg_1"); !strings.HasPrefix(k, "sha256:") || len(k) != 71 {
		t.Fatalf("dedup key %q", k)
	}
}

func TestAtM0NothingContentDerivedIsRecorded(t *testing.T) {
	st, cl, _, _ := record(t, Policy{CeilingMode: "m1", Mode: "m0"}, prompt())
	env := st.events[0]
	for _, k := range []string{"content_digest", "labels", "classifier_version", "confidence", "prompt_kind", "content_excerpt", "attachments"} {
		if _, ok := env[k]; ok {
			t.Errorf("an M0 record carries %s", k)
		}
	}
	if env["collection_mode"] != "m0" || env["size_bytes"] == nil || env["policy_decision"] == nil {
		t.Errorf("M0 envelope %v", env)
	}
	if len(cl.requests) != 0 {
		t.Error("an M0 prompt was classified")
	}
}

func TestAToolOverrideNarrowsTheMode(t *testing.T) {
	st, _, _, _ := record(t, Policy{CeilingMode: "m3", Mode: "m3", ContentSearch: "full_text", ScopeOverrides: map[string]string{"claude_web": "m0"}}, prompt())
	if st.events[0]["collection_mode"] != "m0" {
		t.Fatalf("mode %v under an m0 override", st.events[0]["collection_mode"])
	}
}

func TestAtM2TheExcerptIsKept(t *testing.T) {
	st, _, _, _ := record(t, Policy{CeilingMode: "m2", Mode: "m2"}, prompt())
	ex, _ := st.events[0]["content_excerpt"].(map[string]any)
	if ex["kind"] != protocol.ExcerptMatchSpan {
		t.Fatalf("excerpt %v", st.events[0]["content_excerpt"])
	}
}

func TestCustomLabelsReachTheClassifierAndTheFiles(t *testing.T) {
	p := Policy{CeilingMode: "m1", Mode: "m1", CustomLabels: []store.CustomLabel{
		{Label: "custom_project_x", Words: []string{"falcon"}, CustomerNames: []string{"Contoso"}},
		{Label: "custom_finance", FileExtensions: []string{"XLSX"}},
		{Label: "custom_never", FileNames: []string{"board-*.pdf"}},
	}}
	st, cl, _, _ := record(t, p, prompt())
	terms := cl.requests[0].CustomLabels
	if len(terms) != 1 || terms[0].Label != "custom_project_x" || len(terms[0].Terms) != 2 {
		t.Fatalf("classifier terms %+v", terms)
	}
	var classes []string
	for _, l := range st.events[0]["labels"].([]any) {
		classes = append(classes, l.(map[string]any)["class"].(string))
	}
	if strings.Join(classes, ",") != "customer_pii,custom_finance" {
		t.Fatalf("labels %v", classes)
	}
}

func TestAPersonKnownOnlyByObjectIDIsAttributedByIt(t *testing.T) {
	rec := prompt()
	rec.UPN, rec.ObjectID = "", "6f9619ff-8b86-d011-b42d-00c04fc964ff"
	st, _, _, _ := record(t, Policy{CeilingMode: "m1", Mode: "m1"}, rec)
	want, _ := protocol.DeriveUserRef(testRefKey, protocol.UserRefOID, rec.ObjectID)
	if st.events[0]["user_ref"] != want {
		t.Fatalf("user_ref %v, want %v", st.events[0]["user_ref"], want)
	}
	if _, ok := st.events[0]["subject_name"]; ok {
		t.Error("a record with no name sent one")
	}
}

func TestARecordWithNoPersonOrNoPromptIsSkipped(t *testing.T) {
	r := &Recorder{Store: newFakeStore(), Classifier: &fakeClassifier{}, Vault: &fakeVault{}}
	for name, mutate := range map[string]func(*Record){
		"no person": func(r *Record) { r.UPN, r.ObjectID = "", "" },
		"no prompt": func(r *Record) { r.Text, r.Attachments = "", nil },
		"no id":     func(r *Record) { r.ID = "" },
		"no time":   func(r *Record) { r.OccurredAt = time.Time{} },
	} {
		rec := prompt()
		mutate(&rec)
		if _, err := r.Record(context.Background(), conn, Policy{Mode: "m1", CeilingMode: "m1"}, testRefKey, rec); !IsSkipped(err) {
			t.Errorf("%s: %v, want skipped", name, err)
		}
	}
}

func TestAtM3ANewPromptIsStoredAsADeviceUploadIs(t *testing.T) {
	st, _, v, _ := record(t, Policy{CeilingMode: "m3", Mode: "m3", ContentSearch: "full_text"}, prompt())
	if len(st.grants) != 1 || len(v.puts) != 1 {
		t.Fatalf("%d grants, %d uploads", len(st.grants), len(v.puts))
	}
	g := st.grants[0]
	event := EventID(conn.ConnectionID, "msg_1")
	if g.EventID != event || g.ConnectionID != conn.ConnectionID || g.SubmissionID == "" ||
		!g.ExpiresAt.Equal(time.Date(2026, 10, 13, 0, 15, 0, 0, time.UTC)) {
		t.Errorf("grant %+v", g)
	}
	body := []byte(prompt().Text)
	if want := event + " " + g.GrantID + " " + protocol.RawDigest(body) + " " + string(body); v.puts[0] != want {
		t.Errorf("upload %q, want %q", v.puts[0], want)
	}
	if st.usage != int64(len(body)) {
		t.Errorf("usage %d", st.usage)
	}
}

func TestContentIsNotStoredWhereTheTenantDoesNotStoreIt(t *testing.T) {
	for name, p := range map[string]Policy{
		"storage off":   {CeilingMode: "m3", Mode: "m3", ContentSearch: "disabled"},
		"ceiling below": {CeilingMode: "m2", Mode: "m2", ContentSearch: "attachment_names"},
	} {
		st, _, v, _ := record(t, p, prompt())
		if len(st.grants)+len(v.puts) != 0 {
			t.Errorf("%s: content stored", name)
		}
	}
}

func TestAMergedRecordLeavesTheContentToTheDeviceAndARereadOneIsStoredIfPending(t *testing.T) {
	p := Policy{CeilingMode: "m3", Mode: "m3", ContentSearch: "full_text"}
	st, cl, v := newFakeStore(), &fakeClassifier{}, &fakeVault{}
	r := &Recorder{Store: st, Classifier: cl, Vault: v}
	st.outcomes[EventID(conn.ConnectionID, "msg_1")] = OutcomeMerged
	if out, err := r.Record(context.Background(), conn, p, testRefKey, prompt()); err != nil || out != OutcomeMerged {
		t.Fatalf("merged: %q %v", out, err)
	}
	if len(v.puts) != 0 {
		t.Fatal("a merged record stored its content")
	}

	st2, v2 := newFakeStore(), &fakeVault{}
	r = &Recorder{Store: st2, Classifier: cl, Vault: v2}
	ctx := context.Background()
	if _, err := r.Record(ctx, conn, p, testRefKey, prompt()); err != nil {
		t.Fatal(err)
	}
	if out, _ := r.Record(ctx, conn, p, testRefKey, prompt()); out != OutcomeDuplicate || len(v2.puts) != 1 {
		t.Fatalf("a duplicate whose content is stored: %q, %d uploads", out, len(v2.puts))
	}
	st2.pending = true
	if _, err := r.Record(ctx, conn, p, testRefKey, prompt()); err != nil || len(v2.puts) != 2 {
		t.Fatalf("a duplicate whose content is pending: %v, %d uploads", err, len(v2.puts))
	}
}

func TestAStoreFailureEndsThePass(t *testing.T) {
	st := newFakeStore()
	st.recordErr = context.DeadlineExceeded
	r := &Recorder{Store: st, Classifier: &fakeClassifier{}, Vault: &fakeVault{}}
	_, err := r.Record(context.Background(), conn, Policy{Mode: "m1", CeilingMode: "m1"}, testRefKey, prompt())
	if err == nil || IsSkipped(err) || failureCode(ClaudeEnterprise, err) != ErrInternal {
		t.Fatalf("err %v", err)
	}
}

func TestDescriptorsKeepToTheEnvelopeLimits(t *testing.T) {
	var atts []canon.Attachment
	for i := 0; i < protocol.MaxAttachments+5; i++ {
		atts = append(atts, canon.Attachment{Name: strings.Repeat("n", 300), SizeBytes: -1})
	}
	atts = append([]canon.Attachment{{Name: "  "}}, atts...)
	d := descriptors(atts)
	if len(d) != protocol.MaxAttachments || len([]rune(d[0].Name)) != protocol.MaxAttachmentNameRunes || d[0].SizeBytes != 0 {
		t.Fatalf("%d descriptors, first %q size %d", len(d), d[0].Name, d[0].SizeBytes)
	}
}
