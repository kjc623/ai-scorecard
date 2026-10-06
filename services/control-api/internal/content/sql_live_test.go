package content_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/protocol"

	"github.com/shadow-ai-capture/control-api/internal/content"
	"github.com/shadow-ai-capture/control-api/internal/pgtest"
)

// The live test: the grant path's statements prepared as written, then a grant decided and an
// upload counted as sac_control under forced row-level security, against the database
// SAC_TEST_PG_DSN names.

func TestStatementsPrepare(t *testing.T) {
	db := pgtest.Open(t)
	for name, q := range content.Statements {
		if _, err := db.Exec("PREPARE sac_content_" + name + " AS " + q); err != nil {
			t.Errorf("statement %q does not prepare: %v", name, err)
			continue
		}
		_, _ = db.Exec("DEALLOCATE sac_content_" + name)
	}
}

type storedVault struct{}

func (storedVault) Put(context.Context, string, string, string, string, []byte) (bool, error) {
	return true, nil
}

func TestGrantAndUsageAgainstPostgres(t *testing.T) {
	owner := pgtest.Open(t)
	ctx := context.Background()
	tenant := pgtest.Tenant(t, owner, "eastus")
	device, event, submission := pgtest.UUID(t), pgtest.UUID(t), pgtest.UUID(t)
	digest := "sha256:" + strings.Repeat("ab", 32)
	dedup := fmt.Sprintf("sha256:%064x", time.Now().UnixNano())
	for _, s := range []struct {
		q    string
		args []any
	}{
		{`UPDATE ops.tenant SET ceiling_mode = 'm3', content_budget_bytes_per_day = 1000 WHERE tenant_id = $1::uuid`, []any{tenant}},
		{`INSERT INTO ops.device (tenant_id, device_id, os) VALUES ($1::uuid, $2::uuid, 'windows')`, []any{tenant, device}},
		{`INSERT INTO ingest.submission (tenant_id, submission_id, dedup_key, dedup_weak_key, kind, prompt_kind, device_id,
		     user_ref, tool_fingerprint, first_occurred_at, last_occurred_at, received_at, collection_mode, size_bytes,
		     content_digest, labels, classifier_version, confidence, winning_source, winning_fidelity, observed_routes,
		     content_state, expires_at)
		  VALUES ($1::uuid, $2::uuid, $3, $3, 'prompt', 'user', $4::uuid, 'u_live', 'genai.web.chat.v1:chatgpt',
		     now(), now(), now(), 'm3', 10, $5, '[]'::jsonb, 'rel-1', 'high', 'ext.page_context', 10,
		     ARRAY['ext.page_context'], 'local_only', now() + interval '30 days')`, []any{tenant, submission, dedup, device, digest}},
		{`INSERT INTO ingest.observation (tenant_id, event_id, device_id, user_ref, tool_fingerprint, direction, kind,
		     prompt_kind, occurred_at, received_at, monotonic_offset_ms, source, confidence, collection_mode, size_bytes,
		     content_digest, labels, classifier_version, policy_decision, dedup_key, schema_version, expires_at)
		  VALUES ($1::uuid, $2::uuid, $3::uuid, 'u_live', 'genai.web.chat.v1:chatgpt', 'egress', 'prompt',
		     'user', now(), now(), 0, 'ext.page_context', 'high', 'm3', 10,
		     $4, '[]'::jsonb, 'rel-1', '{}'::jsonb, $5, '1.0', now() + interval '30 days')`, []any{tenant, event, device, digest, dedup}},
	} {
		if _, err := owner.ExecContext(ctx, s.q, s.args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, s.q)
		}
	}

	st := content.NewSQL(pgtest.OpenAs(t, "sac_control"))
	svc, err := content.New(st, storedVault{})
	if err != nil {
		t.Fatal(err)
	}
	req := protocol.ContentGrantRequest{
		SchemaVersion: protocol.ContentGrantSchemaVersion, EventID: event, CollectionMode: protocol.ModeM3,
		ContentDigest: digest, SizeBytes: 10, RawSizeBytes: 600,
	}
	if _, err := svc.Decide(ctx, tenant, pgtest.UUID(t), req); err == nil || !strings.Contains(err.Error(), string(protocol.ReasonUnknownEvent)) {
		t.Fatalf("another device's event: err = %v, want %s", err, protocol.ReasonUnknownEvent)
	}
	resp, err := svc.Decide(ctx, tenant, device, req)
	if err != nil || resp.State != protocol.ContentGrantGranted {
		t.Fatalf("Decide = %+v, %v", resp, err)
	}
	again, err := svc.Decide(ctx, tenant, device, req)
	if err != nil || again.GrantID != resp.GrantID {
		t.Fatalf("an open grant was decided again: %+v, %v", again, err)
	}

	body := []byte(strings.Repeat("x", 600))
	if err := svc.Upload(ctx, tenant, device, content.Upload{GrantID: resp.GrantID, EventID: event,
		RawDigest: protocol.RawDigest(body), Body: body}); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	var used int64
	if err := owner.QueryRowContext(ctx, `SELECT content_bytes_added FROM ops.usage_daily WHERE tenant_id = $1::uuid`, tenant).Scan(&used); err != nil || used != 600 {
		t.Fatalf("content usage = %d, %v; want 600", used, err)
	}

	// The budget now refuses a second object of the same size on another event.
	second := pgtest.UUID(t)
	if _, err := owner.ExecContext(ctx, `INSERT INTO ingest.observation (tenant_id, event_id, device_id, user_ref,
	     tool_fingerprint, direction, kind, prompt_kind, occurred_at, received_at, monotonic_offset_ms, source, confidence,
	     collection_mode, size_bytes, content_digest, labels, classifier_version, policy_decision, dedup_key, schema_version, expires_at)
	   VALUES ($1::uuid, $2::uuid, $3::uuid, 'u_live', 'genai.web.chat.v1:chatgpt', 'egress', 'prompt', 'user', now(), now(),
	     0, 'ext.page_context', 'high', 'm3', 10, $4, '[]'::jsonb, 'rel-1', '{}'::jsonb, $5, '1.0', now() + interval '30 days')`,
		tenant, second, device, digest, dedup); err != nil {
		t.Fatalf("seed second observation: %v", err)
	}
	req.EventID = second
	denied, err := svc.Decide(ctx, tenant, device, req)
	if err != nil || denied.State != protocol.ContentGrantDenied || denied.Reason != content.DenyOverBudget {
		t.Fatalf("over budget = %+v, %v", denied, err)
	}
}
