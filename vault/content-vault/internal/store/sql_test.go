package store_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/shadow-ai-capture/device/protocol"

	"github.com/shadow-ai-capture/content-vault/internal/keyring"
	"github.com/shadow-ai-capture/content-vault/internal/store"
	"github.com/shadow-ai-capture/content-vault/internal/vault"
)

// These tests run the vault against a real PostgreSQL with database/schema.sql applied, named by
// SAC_TEST_PG_DSN. Seed rows are written through that connection; the vault's own statements run
// on connections that SET ROLE sac_vault, so its grants and row-level security are the deployed
// ones. Each test creates its own random tenant and touches nothing else.

type live struct {
	t     *testing.T
	admin *sql.DB
	vault *sql.DB
	keys  *keyring.Keyring
	svc   *vault.Service
}

func newLive(t *testing.T) *live {
	t.Helper()
	dsn := os.Getenv("SAC_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("SAC_TEST_PG_DSN is not set: set it to a postgres:// URL of a database with database/schema.sql applied to run the live-database tests")
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("SAC_TEST_PG_DSN: %v", err)
	}
	admin := stdlib.OpenDB(*cfg)
	vaultDB := stdlib.OpenDB(*cfg, stdlib.OptionAfterConnect(func(ctx context.Context, c *pgx.Conn) error {
		_, err := c.Exec(ctx, "SET ROLE sac_vault")
		return err
	}))
	t.Cleanup(func() { admin.Close(); vaultDB.Close() })
	keys, err := keyring.Parse("v1:" + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	svc, err := vault.New(vault.Config{Store: store.New(vaultDB), Keys: keys})
	if err != nil {
		t.Fatal(err)
	}
	return &live{t: t, admin: admin, vault: vaultDB, keys: keys, svc: svc}
}

func newID() string { return uuid.New().String() }

func digest() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return "sha256:" + hex.EncodeToString(b)
}

// as runs statements as the seeding connection, inside the tenant's row-level-security scope.
func (l *live) as(tenantID string, fn func(*sql.Tx)) {
	l.t.Helper()
	ctx := context.Background()
	tx, err := l.admin.BeginTx(ctx, nil)
	if err != nil {
		l.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantID); err != nil {
		l.t.Fatal(err)
	}
	fn(tx)
	if err := tx.Commit(); err != nil {
		l.t.Fatal(err)
	}
}

func (l *live) exec(tx *sql.Tx, query string, args ...any) {
	l.t.Helper()
	if _, err := tx.ExecContext(context.Background(), query, args...); err != nil {
		l.t.Fatalf("seed: %v\n%s", err, query)
	}
}

type world struct {
	tenant, device string
}

// seedTenant creates a tenant at M3 with the given search tier, a device, and content retention
// policies of 7 days for payment_card and 60 days for legal_commercial.
func (l *live) seedTenant(tier string) world {
	w := world{tenant: newID(), device: newID()}
	l.as(w.tenant, func(tx *sql.Tx) {
		l.exec(tx, `INSERT INTO ops.tenant (tenant_id, name, status, residency_region, ceiling_mode, content_search)
		            VALUES ($1, 'content-vault live test', 'active', 'eastus', 'm3', $2)`, w.tenant, tier)
		l.exec(tx, `INSERT INTO ops.device (tenant_id, device_id, os) VALUES ($1, $2, 'linux')`, w.tenant, w.device)
		l.exec(tx, `INSERT INTO ops.retention_policy (tenant_id, applies_to, data_class, collection_mode, ttl_days, updated_by)
		            VALUES ($1, 'content', 'payment_card', 'm3', 7, 'test'), ($1, 'content', 'legal_commercial', 'm3', 60, 'test')`, w.tenant)
	})
	return w
}

// seedSubmission creates a prompt submission and returns its id.
func (l *live) seedSubmission(w world, promptKind, labels string, receivedAt time.Time) string {
	id := newID()
	l.as(w.tenant, func(tx *sql.Tx) {
		l.exec(tx, `INSERT INTO ingest.submission (tenant_id, submission_id, dedup_weak_key, kind, prompt_kind, device_id,
		              user_ref, tool_fingerprint, first_occurred_at, last_occurred_at, received_at, collection_mode,
		              labels, winning_source, winning_fidelity, observed_routes, expires_at)
		            VALUES ($1, $2, $3, 'prompt', $4, $5, 'user-ref-1', 'tool-a', $6, $6, $6, 'm3',
		                    $7::jsonb, 'ext.page_context', 10, '{ext.page_context}', $6::timestamptz + interval '90 days')`,
			w.tenant, id, digest(), promptKind, w.device, receivedAt, labels)
	})
	return id
}

// seedGrant creates a granted upload grant for an event.
func (l *live) seedGrant(w world, eventID, submissionID string) string {
	id := newID()
	l.as(w.tenant, func(tx *sql.Tx) {
		l.exec(tx, `INSERT INTO ops.grant (tenant_id, grant_id, event_id, submission_id, device_id, decided_at, decision, expires_at)
		            VALUES ($1, $2, $3, nullif($4, '')::uuid, $5, now(), 'granted', now() + interval '10 minutes')`,
			w.tenant, id, eventID, submissionID, w.device)
	})
	return id
}

func (l *live) upload(w world, eventID, grantID string, content []byte) (vault.UploadResult, error) {
	return l.svc.Upload(context.Background(), vault.UploadRequest{
		TenantID: w.tenant, EventID: eventID, GrantID: grantID, RawDigest: protocol.RawDigest(content), Content: content,
	})
}

func TestLiveUploadRetrieveRedeem(t *testing.T) {
	l := newLive(t)
	ctx := context.Background()
	w := l.seedTenant("full_text")
	sub := l.seedSubmission(w, "user", `[{"class":"payment_card","score":0.9},{"class":"legal_commercial","score":0.4}]`, time.Now())
	event := newID()
	grant := l.seedGrant(w, event, sub)
	content := []byte(`{"prompt":"renewal terms for Contoso","attachments":[{"name":"Q3-board-pack.pdf"},{"name":"terms.docx"}]}`)

	res, err := l.upload(w, event, grant, content)
	if err != nil {
		t.Fatal(err)
	}
	if res.IndexedPrompt != 1 || res.IndexedAttachmentNames != 2 || res.Replayed {
		t.Fatalf("upload = %+v", res)
	}
	if d := time.Until(res.ExpiresAt) - 7*24*time.Hour; d > time.Minute || d < -time.Minute {
		t.Fatalf("expires_at %s is not now + 7 days, the payment_card content policy", res.ExpiresAt)
	}

	l.as(w.tenant, func(tx *sql.Tx) {
		var sealedLen, size int
		var version, kind, class string
		var used sql.NullTime
		if err := tx.QueryRowContext(ctx, `SELECT octet_length(c.ciphertext), c.plaintext_size_bytes, c.key_version,
		         coalesce(c.prompt_kind, ''), c.retention_class, g.used_at
		    FROM ops.content c JOIN ops.grant g ON g.tenant_id = c.tenant_id AND g.grant_id = c.grant_id
		   WHERE c.tenant_id = $1 AND c.event_id = $2`, w.tenant, event).Scan(&sealedLen, &size, &version, &kind, &class, &used); err != nil {
			t.Fatal(err)
		}
		if sealedLen != len(content)+28 || size != len(content) || version != "v1" || kind != "user" || class != "content" || !used.Valid {
			t.Fatalf("stored row: %d sealed bytes for %d, version %q, kind %q, class %q, used %v", sealedLen, size, version, kind, class, used)
		}
		var rows int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM ingest.search_text WHERE tenant_id = $1 AND submission_id = $2`, w.tenant, sub).Scan(&rows); err != nil || rows != 3 {
			t.Fatalf("search rows = %d, %v; want 3", rows, err)
		}
		var state string
		if err := tx.QueryRowContext(ctx, `SELECT content_state FROM ingest.submission WHERE tenant_id = $1 AND submission_id = $2`, w.tenant, sub).Scan(&state); err != nil || state != "uploaded" {
			t.Fatalf("submission content_state = %q, %v; want uploaded", state, err)
		}
		var action string
		if err := tx.QueryRowContext(ctx, `SELECT action FROM ops.audit WHERE tenant_id = $1 ORDER BY audit_seq DESC LIMIT 1`, w.tenant).Scan(&action); err != nil || action != vault.ActionContentStored {
			t.Fatalf("latest audit action = %q, %v", action, err)
		}
	})

	again, err := l.upload(w, event, grant, content)
	if err != nil || !again.Replayed || again.ObjectID != res.ObjectID {
		t.Fatalf("retry = %+v, %v; want the stored object replayed", again, err)
	}
	if _, err := l.upload(w, event, grant, []byte("different")); !isDenial(err, vault.ReasonAlreadyStored) {
		t.Fatalf("a different body under the used grant = %v", err)
	}
	second := l.seedGrant(w, event, sub)
	if _, err := l.upload(w, event, second, []byte("replacement")); !isDenial(err, vault.ReasonAlreadyStored) {
		t.Fatalf("a second grant for a stored event = %v", err)
	}

	minted, err := l.svc.Retrieve(ctx, vault.RetrieveRequest{TenantID: w.tenant, EventID: event, Principal: "alice@example.com", CaseReference: "CASE-1", SecondApprover: "bob@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := l.svc.Redeem(ctx, w.tenant, minted.GrantID)
	if err != nil || !bytes.Equal(got.Plaintext, content) || got.RawDigest != protocol.RawDigest(content) {
		t.Fatalf("redeem = %q, %v", got.Plaintext, err)
	}
	if _, err := l.svc.Redeem(ctx, w.tenant, minted.GrantID); !isDenial(err, vault.ReasonGrantAlreadyUsed) {
		t.Fatalf("second redemption = %v", err)
	}
	if _, err := l.svc.Retrieve(ctx, vault.RetrieveRequest{TenantID: w.tenant, EventID: newID(), Principal: "alice@example.com"}); !isDenial(err, vault.ReasonNoContentObject) {
		t.Fatalf("retrieval with no content = %v", err)
	}
}

func TestLiveSearch(t *testing.T) {
	l := newLive(t)
	ctx := context.Background()
	w := l.seedTenant("full_text")
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	var subs []string
	for i, body := range []string{
		`{"prompt":"contract renewal for Contoso","attachments":[{"name":"Contoso-Q3_report.pdf"}]}`,
		`{"prompt":"Contoso pricing review","attachments":[{"name":"pricing.xlsx"}]}`,
		`{"prompt":"unrelated question"}`,
	} {
		sub := l.seedSubmission(w, "user", `[]`, base.Add(time.Duration(i)*time.Minute))
		subs = append(subs, sub)
		event := newID()
		if _, err := l.upload(w, event, l.seedGrant(w, event, sub), []byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	search := func(form store.SearchForm, q, cursor string, limit int) vault.SearchResult {
		t.Helper()
		res, err := l.svc.Search(ctx, vault.SearchRequest{TenantID: w.tenant, Principal: "alice", Form: form, Query: q, Limit: limit, Cursor: cursor})
		if err != nil {
			t.Fatalf("search %s %q: %v", form, q, err)
		}
		return res
	}

	// The text-search parser reads a filename as one token, so the terms form finds the two prompts
	// and not "Contoso-Q3_report.pdf"; the substring and fuzzy forms are the way to match names.
	terms := search(store.FormTerms, "contoso", "", 10)
	if len(terms.Hits) != 2 || terms.Hits[0].SubmissionID != subs[1] || terms.Hits[1].SubmissionID != subs[0] ||
		!strings.Contains(terms.Hits[0].Snippet, "<em>") {
		t.Fatalf("terms hits = %+v, want the two prompts, newest first, highlighted", terms.Hits)
	}
	if phrase := search(store.FormTerms, `"contract renewal"`, "", 10); len(phrase.Hits) != 1 || phrase.Hits[0].SubmissionID != subs[0] {
		t.Fatalf("phrase hits = %+v", phrase.Hits)
	}
	if sub := search(store.FormSubstring, "Q3_rep", "", 10); len(sub.Hits) != 1 || sub.Hits[0].UnitKind != store.UnitAttachmentName {
		t.Fatalf("substring hits = %+v", sub.Hits)
	}
	if lit := search(store.FormSubstring, "%", "", 10); len(lit.Hits) != 0 {
		t.Fatalf("a literal %% matched %d rows; LIKE metacharacters must be escaped", len(lit.Hits))
	}
	if fuzzy := search(store.FormFuzzy, "contoso-q3-reprot.pdf", "", 10); len(fuzzy.Hits) != 1 || fuzzy.Hits[0].SubmissionID != subs[0] {
		t.Fatalf("fuzzy hits = %+v", fuzzy.Hits)
	}

	var seen []string
	cursor := ""
	for range 4 {
		page := search(store.FormTerms, "contoso", cursor, 1)
		for _, h := range page.Hits {
			seen = append(seen, h.SubmissionID)
		}
		if cursor = page.NextCursor; cursor == "" {
			break
		}
	}
	if strings.Join(seen, ",") != subs[1]+","+subs[0] {
		t.Fatalf("paging returned %v, want each hit once, newest first", seen)
	}

	filtered, err := l.svc.Search(ctx, vault.SearchRequest{TenantID: w.tenant, Principal: "alice", Query: "contoso",
		Filters: store.SearchFilters{Device: w.device, Mode: "m3", ReceivedFrom: base.Add(30 * time.Second)}})
	if err != nil || len(filtered.Hits) != 1 || filtered.Hits[0].SubmissionID != subs[1] {
		t.Fatalf("filtered hits = %+v, %v", filtered.Hits, err)
	}
	if none, err := l.svc.Search(ctx, vault.SearchRequest{TenantID: w.tenant, Principal: "alice", Query: "contoso",
		Filters: store.SearchFilters{Subject: "someone-else"}}); err != nil || len(none.Hits) != 0 {
		t.Fatalf("subject filter = %+v, %v", none.Hits, err)
	}
}

func TestLiveTierAndPromptKind(t *testing.T) {
	l := newLive(t)
	ctx := context.Background()
	names := l.seedTenant("attachment_names")
	sub := l.seedSubmission(names, "user", `[]`, time.Now())
	event := newID()
	res, err := l.upload(names, event, l.seedGrant(names, event, sub), []byte(`{"prompt":"secret plan","attachments":[{"name":"plan.pdf"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.IndexedPrompt != 0 || res.IndexedAttachmentNames != 1 {
		t.Fatalf("attachment_names tier indexed %+v", res)
	}
	if d := time.Until(res.ExpiresAt) - 60*24*time.Hour; d > time.Minute || d < -time.Minute {
		t.Fatalf("unlabelled content expires %s, want the longest content policy (60 days)", res.ExpiresAt)
	}
	if hits, err := l.svc.Search(ctx, vault.SearchRequest{TenantID: names.tenant, Principal: "a", Query: "secret"}); err != nil || len(hits.Hits) != 0 {
		t.Fatalf("prompt text found at the attachment_names tier: %+v, %v", hits.Hits, err)
	}

	full := l.seedTenant("full_text")
	generated := l.seedSubmission(full, "client_generated", `[]`, time.Now())
	event = newID()
	res, err = l.upload(full, event, l.seedGrant(full, event, generated), []byte(`{"prompt":"title this chat"}`))
	if err != nil || res.IndexedPrompt+res.IndexedAttachmentNames != 0 {
		t.Fatalf("client-generated upload = %+v, %v; want stored and not indexed", res, err)
	}

	unknown := newID()
	res, err = l.upload(full, unknown, l.seedGrant(full, unknown, ""), []byte("no submission yet"))
	if err != nil || res.IndexedPrompt != 0 {
		t.Fatalf("upload without a submission = %+v, %v", res, err)
	}
	minted, err := l.svc.Retrieve(ctx, vault.RetrieveRequest{TenantID: full.tenant, EventID: unknown, Principal: "alice"})
	if err != nil {
		t.Fatalf("retrieval of content without a submission: %v", err)
	}
	if got, err := l.svc.Redeem(ctx, full.tenant, minted.GrantID); err != nil || string(got.Plaintext) != "no submission yet" {
		t.Fatalf("redeem = %q, %v", got.Plaintext, err)
	}
}

func TestLiveContentStateIsNotRewoundFromShredded(t *testing.T) {
	l := newLive(t)
	w := l.seedTenant("full_text")
	sub := l.seedSubmission(w, "user", `[]`, time.Now())
	l.as(w.tenant, func(tx *sql.Tx) {
		l.exec(tx, `UPDATE ingest.submission SET content_state = 'shredded', shredded_reason = 'erasure'
		             WHERE tenant_id = $1 AND submission_id = $2`, w.tenant, sub)
	})
	event := newID()
	if _, err := l.upload(w, event, l.seedGrant(w, event, sub), []byte("late upload")); err != nil {
		t.Fatal(err)
	}
	l.as(w.tenant, func(tx *sql.Tx) {
		var state string
		if err := tx.QueryRowContext(context.Background(), `SELECT content_state FROM ingest.submission WHERE tenant_id = $1 AND submission_id = $2`,
			w.tenant, sub).Scan(&state); err != nil || state != "shredded" {
			t.Fatalf("content_state = %q, %v; want shredded left alone", state, err)
		}
	})
}

func TestLiveRowLevelSecurity(t *testing.T) {
	l := newLive(t)
	ctx := context.Background()
	a, b := l.seedTenant("full_text"), l.seedTenant("full_text")
	sub := l.seedSubmission(a, "user", `[]`, time.Now())
	event := newID()
	grant := l.seedGrant(a, event, sub)
	if _, err := l.upload(a, event, grant, []byte("tenant a's prompt")); err != nil {
		t.Fatal(err)
	}
	err := store.New(l.vault).InTenant(ctx, b.tenant, func(tx store.Tx) error {
		if _, err := tx.ContentForEvent(ctx, event); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("tenant b read tenant a's content: %v", err)
		}
		if _, err := tx.Grant(ctx, grant); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("tenant b read tenant a's grant: %v", err)
		}
		hits, err := tx.Search(ctx, store.SearchQuery{Form: store.FormTerms, Text: "prompt", Limit: 10, Now: time.Now()})
		if err != nil || len(hits) != 0 {
			t.Errorf("tenant b searched tenant a's index: %v, %v", hits, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.upload(b, event, grant, []byte("x")); !isDenial(err, vault.ReasonGrantUnknown) {
		t.Fatalf("tenant b used tenant a's grant: %v", err)
	}
}

func TestLiveReadiness(t *testing.T) {
	l := newLive(t)
	if err := store.New(l.vault).Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func isDenial(err error, reason vault.Reason) bool {
	d, ok := vault.AsDenial(err)
	return ok && d.Reason == reason
}
