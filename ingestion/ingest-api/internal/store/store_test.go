package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/shadow-ai-capture/contracts/envelope"
	"github.com/shadow-ai-capture/device/protocol"
)

func TestCheck(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	revoked := now.Add(-time.Minute)
	ok := PrincipalStatus{TenantKnown: true, TenantStatus: "active", IngestEnabled: true, TenantRegion: "eastus",
		DeviceKnown: true, CredentialKnown: true, CredentialExpiry: now.Add(time.Hour)}
	cases := map[string]struct {
		mutate func(*PrincipalStatus)
		region string
		want   error
	}{
		"active":                  {func(*PrincipalStatus) {}, "eastus", nil},
		"region not compared":     {func(s *PrincipalStatus) { s.TenantRegion = "westeurope" }, "", nil},
		"another region":          {func(s *PrincipalStatus) { s.TenantRegion = "westeurope" }, "eastus", ErrRegionMismatch},
		"closed tenant":           {func(s *PrincipalStatus) { s.TenantStatus = "closed" }, "eastus", ErrTenantSuspended},
		"expired at this instant": {func(s *PrincipalStatus) { s.CredentialExpiry = now }, "eastus", ErrCredentialExpired},
		"revoked credential":      {func(s *PrincipalStatus) { s.CredentialRevoked = &revoked }, "eastus", ErrCredentialRevoked},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			s := ok
			c.mutate(&s)
			if err := s.Check(now, c.region); !errors.Is(err, c.want) {
				t.Fatalf("Check = %v, want %v", err, c.want)
			}
		})
	}
}

// The live tests run only against a database named by SAC_TEST_PG_DSN (a postgres:// URL with
// database/schema.sql applied). Each test creates its own tenant with a random id and deletes it
// afterwards. The store under test connects as sac_ingest, so grants and row-level security are
// exercised; seeding and cleanup use the DSN's own role.

type live struct {
	admin  *sql.DB
	store  *Postgres
	tenant string
	device string
	cred   string
}

func newUUID(t *testing.T) string {
	t.Helper()
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func openLive(t *testing.T) *live {
	t.Helper()
	dsn := os.Getenv("SAC_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("SAC_TEST_PG_DSN is not set; set it to a postgres:// URL of a database with database/schema.sql applied to run the live store tests")
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("SAC_TEST_PG_DSN: %v", err)
	}
	admin := stdlib.OpenDB(*cfg)
	ingest := stdlib.OpenDB(*cfg, stdlib.OptionAfterConnect(func(ctx context.Context, c *pgx.Conn) error {
		_, err := c.Exec(ctx, "SET ROLE sac_ingest")
		return err
	}))
	t.Cleanup(func() { _ = ingest.Close(); _ = admin.Close() })

	l := &live{admin: admin, store: New(ingest), tenant: newUUID(t), device: newUUID(t), cred: newUUID(t)}
	l.exec(t, `INSERT INTO ops.tenant (tenant_id, name, status, residency_region, ceiling_mode) VALUES ($1, 'ingest store test', 'active', 'eastus', 'm3')`, l.tenant)
	l.exec(t, `INSERT INTO ops.device (tenant_id, device_id, os) VALUES ($1, $2, 'windows')`, l.tenant, l.device)
	l.exec(t, `INSERT INTO ops.device_credential (tenant_id, credential_id, device_id, public_key_thumbprint, expires_at)
	           VALUES ($1, $2, $3, 'test-thumbprint', now() + interval '90 days')`, l.tenant, l.cred, l.device)
	t.Cleanup(func() { l.cleanup(t) })
	return l
}

func (l *live) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := l.admin.Exec(query, args...); err != nil {
		t.Fatalf("%s: %v", strings.Fields(query)[0:3], err)
	}
}

func (l *live) count(t *testing.T, table string) int {
	t.Helper()
	var n int
	if err := l.admin.QueryRow(`SELECT count(*) FROM `+table+` WHERE tenant_id = $1`, l.tenant).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// cleanup removes every row the test's tenant owns. ingest.observation refuses deletes outside
// the retention path, which declares itself with sac.retention_delete.
func (l *live) cleanup(t *testing.T) {
	tx, err := l.admin.Begin()
	if err != nil {
		t.Errorf("cleanup: %v", err)
		return
	}
	defer func() { _ = tx.Rollback() }()
	statements := []string{`SET LOCAL sac.retention_delete = on`}
	for _, table := range []string{"ingest.search_text", "ingest.rejected", "ingest.observation", "ingest.submission",
		"ops.usage_daily", "ops.device_credential", "ops.device", "ops.tenant"} {
		statements = append(statements, `DELETE FROM `+table+` WHERE tenant_id = $1`)
	}
	for i, s := range statements {
		var err error
		if i == 0 {
			_, err = tx.Exec(s)
		} else {
			_, err = tx.Exec(s, l.tenant)
		}
		if err != nil {
			t.Errorf("cleanup (%s): %v", s, err)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		t.Errorf("cleanup: %v", err)
	}
}

func (l *live) envelope(t *testing.T, eventID, source string) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"schema_version": "1.0", "event_id": eventID, "tenant_id": l.tenant, "device_id": l.device,
		"user_ref": "user-1", "tool_fingerprint": "tool-1", "direction": "egress", "kind": "prompt",
		"occurred_at": "2026-10-05T12:00:00Z", "monotonic_offset_ms": 1, "source": source,
		"collection_mode": "m1", "confidence": "high", "size_bytes": 10,
		"content_digest": "sha256:" + strings.Repeat("a", 64), "labels": []any{}, "classifier_version": "2026.01.0",
		"policy_decision": map[string]any{"rule_id": "RULE_1", "action": "logged", "decided_locally": true},
		"dedup_key":       "sha256:" + strings.Repeat("b", 64),
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func (l *live) write(accepted []AcceptedEvent, rejected []Rejection, at time.Time) ([]EventOutcome, error) {
	return l.store.WriteBatch(context.Background(), BatchWrite{
		TenantID: l.tenant, DeviceID: l.device, CredentialID: l.cred, ReceivedAt: at, Accepted: accepted, Rejected: rejected,
	})
}

func TestLiveRoutesCoverTheContractVocabulary(t *testing.T) {
	l := openLive(t)
	routes, err := l.store.Routes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range envelope.AllRoutes() {
		if !slices.Contains(routes, string(r)) {
			t.Errorf("ref.route_fidelity has no row for the contract route %q", r)
		}
	}
	if err := l.store.Ping(context.Background()); err != nil {
		t.Errorf("Ping: %v", err)
	}
}

func TestLivePrincipalStatus(t *testing.T) {
	l := openLive(t)
	ctx := context.Background()
	st, err := l.store.PrincipalStatus(ctx, l.tenant, l.device, l.cred)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Check(time.Now(), "eastus"); err != nil {
		t.Errorf("a seeded active credential: %v (status %+v)", err, st)
	}
	if st, _ := l.store.PrincipalStatus(ctx, l.tenant, l.device, newUUID(t)); !errors.Is(st.Check(time.Now(), ""), ErrCredentialUnknown) {
		t.Errorf("an unknown credential reads as %+v", st)
	}
	if st, _ := l.store.PrincipalStatus(ctx, newUUID(t), l.device, l.cred); !errors.Is(st.Check(time.Now(), ""), ErrUnknownTenant) {
		t.Errorf("an unknown tenant reads as %+v", st)
	}
	l.exec(t, `UPDATE ops.device_credential SET revoked_at = now() WHERE tenant_id = $1`, l.tenant)
	if st, _ := l.store.PrincipalStatus(ctx, l.tenant, l.device, l.cred); !errors.Is(st.Check(time.Now(), ""), ErrCredentialRevoked) {
		t.Errorf("a revoked credential reads as %+v", st)
	}
}

func TestLiveWriteBatch(t *testing.T) {
	l := openLive(t)
	at := time.Now().UTC().Truncate(time.Microsecond)
	first, second := newUUID(t), newUUID(t)
	rejection := Rejection{
		Reason:   protocol.ReasonModeViolation,
		Detail:   &protocol.BatchRejectionDetail{Pointer: "/events/2/content_digest", Expected: "absent when kind is prompt and collection_mode is m0"},
		Present:  []string{"collection_mode", "content_digest", "event_id", "kind"},
		Redacted: map[string]any{"event_id": newUUID(t), "kind": "prompt", "collection_mode": "m0"},
	}

	out, err := l.write([]AcceptedEvent{
		{Index: 0, EventID: first, Route: "ext.web_request", Envelope: l.envelope(t, first, "ext.web_request")},
		{Index: 1, EventID: second, Route: "ext.dom", Envelope: l.envelope(t, second, "ext.dom")},
	}, []Rejection{rejection}, at)
	if err != nil {
		t.Fatalf("WriteBatch: %v", err)
	}
	if len(out) != 2 || out[0].Outcome != OutcomeInserted || !out[0].WonFields || out[0].SubmissionID == "" {
		t.Fatalf("outcomes = %+v", out)
	}
	// The second event is the same submission seen by another route (same dedup key).
	if out[1].Outcome != OutcomeMerged || out[1].SubmissionID != out[0].SubmissionID {
		t.Errorf("second route's outcome = %+v, want a merge into %s", out[1], out[0].SubmissionID)
	}
	if n := l.count(t, "ingest.rejected"); n != 1 {
		t.Errorf("ingest.rejected rows = %d, want 1", n)
	}
	var lastSeen time.Time
	if err := l.admin.QueryRow(`SELECT last_seen_at FROM ops.device WHERE tenant_id = $1`, l.tenant).Scan(&lastSeen); err != nil || !lastSeen.Equal(at) {
		t.Errorf("last_seen_at = %v (%v), want %v", lastSeen, err, at)
	}

	// A retry is not a second receipt.
	out, err = l.write([]AcceptedEvent{{Index: 0, EventID: first, Route: "ext.web_request", Envelope: l.envelope(t, first, "ext.web_request")}},
		nil, at.Add(time.Minute))
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if out[0].Outcome != OutcomeDuplicate || out[0].FirstReceivedAt == nil || !out[0].FirstReceivedAt.Equal(at) {
		t.Errorf("replay outcome = %+v, want duplicate first received at %v", out[0], at)
	}
	if n := l.count(t, "ingest.observation"); n != 2 {
		t.Errorf("observations = %d, want 2", n)
	}
}

func TestLiveQuarantineAcceptsEveryPerEventCode(t *testing.T) {
	l := openLive(t)
	for _, code := range []protocol.ReasonCode{
		protocol.ReasonSchemaViolation, protocol.ReasonUnsupportedSchemaVersion,
		protocol.ReasonUnknownKind, protocol.ReasonModeViolation,
	} {
		if _, err := l.write(nil, []Rejection{{Reason: code, Detail: &protocol.BatchRejectionDetail{}, Redacted: map[string]any{}}}, time.Now()); err != nil {
			t.Errorf("quarantine %s: %v", code, err)
		}
	}
}

func TestLiveRevocationInsideTheTransactionWritesNothing(t *testing.T) {
	l := openLive(t)
	l.exec(t, `UPDATE ops.device_credential SET revoked_at = now() WHERE tenant_id = $1`, l.tenant)
	id := newUUID(t)
	_, err := l.write([]AcceptedEvent{{Index: 0, EventID: id, Route: "ext.web_request", Envelope: l.envelope(t, id, "ext.web_request")}},
		[]Rejection{{Reason: protocol.ReasonSchemaViolation, Detail: &protocol.BatchRejectionDetail{}, Redacted: map[string]any{}}}, time.Now())
	if !errors.Is(err, ErrCredentialRevoked) {
		t.Fatalf("err = %v, want ErrCredentialRevoked", err)
	}
	if n := l.count(t, "ingest.observation") + l.count(t, "ingest.rejected"); n != 0 {
		t.Errorf("%d rows written for a revoked credential", n)
	}
}
