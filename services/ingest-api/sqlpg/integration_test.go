//go:build sac_sql_driver

package sqlpg_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/protocol"

	"github.com/shadow-ai-capture/ingest-api/internal/ladder"
	"github.com/shadow-ai-capture/ingest-api/internal/store"
	"github.com/shadow-ai-capture/ingest-api/sqlpg"
)

// The tagged integration test: the database/sql plumbing, against a real PostgreSQL.
//
// internal/store's own live test runs the *statement text* through psql, so the SQL and the stored
// procedure are verified there. What was never verified is the layer this file covers: database/sql
// itself — connection pooling, BeginTx/Commit/Rollback, parameter binding, driver-level error
// mapping, and the transaction boundary the service relies on ("either the batch committed and the
// response describes every event, or nothing committed and a retry is free").
//
// It applies nothing: the lab's compose file applies db/schema.sql. It seeds its own tenant, device
// and credential, and removes them again.
//
// Run it with:
//
//	node lab/run.mjs
//	go test -tags sac_sql_driver ./sqlpg/ -v
//
// It SKIPS, loudly, when no server is reachable, because a skip with a reason is honest and a red
// gate on a machine without Docker is not.

// labDSN is the lab's PostgreSQL: lab/docker-compose.yml publishes 5432 with a throwaway credential
// for a container on the host's loopback. SAC_PG_DSN overrides it.
const labDSN = "postgres://postgres:sac-lab-only@127.0.0.1:5432/shadow?sslmode=disable"

func parDSN() string {
	if v := os.Getenv("SAC_PG_DSN"); v != "" {
		return v
	}
	return labDSN
}

// openLab connects, or skips with a reason that says how to fix it.
func openLab(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sqlpg.OpenDB(parDSN())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		t.Skipf("SKIPPING (not a failure): no PostgreSQL at %s — %v\n"+
			"Start the lab first:  node lab/run.mjs\n"+
			"Or point this test at another server:  SAC_PG_DSN=... go test -tags sac_sql_driver ./sqlpg/ -v",
			redactDSN(parDSN()), err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// redactDSN removes a password before a DSN reaches a test log.
func redactDSN(dsn string) string {
	at := strings.LastIndex(dsn, "@")
	proto := strings.Index(dsn, "://")
	if at < 0 || proto < 0 || at < proto {
		return dsn
	}
	creds := dsn[proto+3 : at]
	if i := strings.Index(creds, ":"); i >= 0 {
		creds = creds[:i] + ":<redacted>"
	}
	return dsn[:proto+3] + creds + dsn[at:]
}

// fixture is one seeded tenant, device and credential.
type fixture struct {
	tenant     string
	device     string
	credential string
}

// seed creates the rows the write path needs and removes them afterwards. The identifiers are
// generated per run so two runs cannot collide, and cleanup runs in reverse dependency order.
func seed(t *testing.T, db *sql.DB) fixture {
	t.Helper()
	f := fixture{
		tenant:     fmt.Sprintf("00000000-0000-4000-8000-%012d", time.Now().UnixNano()%1_000_000_000_000),
		device:     fmt.Sprintf("00000000-0000-4000-9000-%012d", time.Now().UnixNano()%1_000_000_000_000),
		credential: fmt.Sprintf("00000000-0000-4000-a000-%012d", time.Now().UnixNano()%1_000_000_000_000),
	}
	ctx := context.Background()
	stmts := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO ops.tenant (tenant_id, name, status, residency_region, key_custody, ceiling_mode, content_search)
		  VALUES ($1::uuid, 'sqlpg-integration', 'active', 'eu-west', 'vendor', 'm1', 'disabled')`, []any{f.tenant}},
		{`INSERT INTO ops.device (tenant_id, device_id, os) VALUES ($1::uuid, $2::uuid, 'windows')`, []any{f.tenant, f.device}},
		{`INSERT INTO ops.device_credential (tenant_id, credential_id, device_id, public_key_thumbprint, issued_at, expires_at)
		  VALUES ($1::uuid, $2::uuid, $3::uuid, 'sha256-sqlpg-integration', now(), now() + interval '90 days')`,
			[]any{f.tenant, f.credential, f.device}},
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s.sql, s.args...); err != nil {
			t.Fatalf("seed (%s): %v", firstLine(s.sql), err)
		}
	}
	t.Cleanup(func() {
		// ingest.observation is append-only: its trigger refuses DELETE unless the session says it is
		// the retention path, which is exactly the discipline db/schema.sql documents ("UPDATE is
		// blocked outright; DELETE is possible only through the retention path, which sets a session
		// flag"). A test that wants its rows gone has to go through that door rather than around it —
		// and doing so is also the cheapest proof that the door exists and is the only one.
		ctx := context.Background()
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Logf("cleanup: begin: %v", err)
			return
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.ExecContext(ctx, `SET LOCAL sac.retention_delete = on`); err != nil {
			t.Logf("cleanup: set the retention flag: %v", err)
			return
		}
		// Dependency order: rows that reference the device and the submissions go first.
		for _, s := range []string{
			`DELETE FROM ingest.search_text WHERE tenant_id = $1::uuid`,
			`DELETE FROM ingest.rejected WHERE tenant_id = $1::uuid`,
			`DELETE FROM ingest.observation WHERE tenant_id = $1::uuid`,
			`DELETE FROM ingest.submission WHERE tenant_id = $1::uuid`,
			`DELETE FROM ops.device_credential WHERE tenant_id = $1::uuid`,
			`DELETE FROM ops.device WHERE tenant_id = $1::uuid`,
			`DELETE FROM ops.tenant WHERE tenant_id = $1::uuid`,
		} {
			if _, err := tx.ExecContext(ctx, s, f.tenant); err != nil {
				t.Logf("cleanup (%s): %v", firstLine(s), err)
				return
			}
		}
		if err := tx.Commit(); err != nil {
			t.Logf("cleanup: commit: %v", err)
		}
	})
	return f
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func count(t *testing.T, db *sql.DB, table string, tenant string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(),
		fmt.Sprintf(`SELECT count(*) FROM %s WHERE tenant_id = $1::uuid`, table), tenant).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// envelope builds a valid M1 prompt for one of this test's own events.
//
// It is built here rather than borrowed from internal/ladder because that fixture is keyed to fixed
// tenant and device identifiers, and this test needs rows it can create and delete. The shape is the
// same one the ladder fixture uses, and the service's contract tests are what pin it.
func envelope(f fixture, eventID, source, key, digest string) json.RawMessage {
	blob, err := json.Marshal(map[string]any{
		"schema_version":      "1.0",
		"event_id":            eventID,
		"tenant_id":           f.tenant,
		"device_id":           f.device,
		"user_ref":            "sqlpg-integration",
		"tool_fingerprint":    "sqlpg-tool",
		"direction":           "egress",
		"kind":                "prompt",
		"occurred_at":         "2026-10-02T14:00:00Z",
		"monotonic_offset_ms": 5,
		"source":              source,
		"collection_mode":     "m1",
		"confidence":          "high",
		"size_bytes":          120,
		"content_digest":      digest,
		"labels":              []any{},
		"classifier_version":  "2026.01.0-shadow",
		"policy_decision":     map[string]any{"rule_id": "RULE_1", "action": "logged", "decided_locally": true},
		"dedup_key":           key,
	})
	if err != nil {
		panic("sqlpg: marshal envelope: " + err.Error())
	}
	return blob
}

// TestStatementsThroughDatabaseSQL is the layer the psql harness cannot reach.
func TestStatementsThroughDatabaseSQL(t *testing.T) {
	db := openLab(t)
	f := seed(t, db)
	ctx := context.Background()

	// The store under test, opened the way the service opens it.
	st, err := sqlpg.Open(parDSN())
	if err != nil {
		t.Fatalf("sqlpg.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	t.Run("route fidelity is read through the driver", func(t *testing.T) {
		routes, err := st.RouteFidelity(ctx)
		if err != nil {
			t.Fatalf("RouteFidelity: %v", err)
		}
		if len(routes) != 7 {
			t.Fatalf("routes = %d, want the seven seeded rows", len(routes))
		}
		if routes["ext.page_context"].Rank != 10 {
			t.Errorf("ext.page_context rank = %d, want 10 (lower wins)", routes["ext.page_context"].Rank)
		}
	})

	t.Run("the session tenant is set before any tenant-scoped read", func(t *testing.T) {
		// If SET LOCAL app.tenant_id did not run, row-level security would return no row and this
		// would report an unknown principal rather than the seeded active one.
		status, err := st.PrincipalStatus(ctx, f.tenant, f.device, f.credential)
		if err != nil {
			t.Fatalf("PrincipalStatus: %v", err)
		}
		if !status.TenantKnown || !status.DeviceKnown || !status.CredentialKnown {
			t.Fatalf("principal not resolved: %+v", status)
		}
		if err := status.CheckWritable(time.Now()); err != nil {
			t.Fatalf("the seeded principal is not writable: %v", err)
		}
	})

	// The two-route case: two observations, one submission, one transaction.
	key := ladder.Hash('a')
	digest := ladder.Hash('b')
	eventA := ladder.DeterministicUUID(9001)
	eventB := ladder.DeterministicUUID(9002)

	res, err := st.WriteBatch(ctx, store.BatchWrite{
		TenantID: f.tenant, DeviceID: f.device, CredentialID: f.credential,
		ReceivedAt: time.Date(2026, 10, 2, 14, 0, 1, 0, time.UTC),
		Accepted: []store.AcceptedEvent{
			{Index: 0, EventID: eventA, Route: "ext.web_request", Envelope: envelope(f, eventA, "ext.web_request", key, digest)},
			{Index: 1, EventID: eventB, Route: "ext.page_context", Envelope: envelope(f, eventB, "ext.page_context", key, digest)},
		},
	})
	if err != nil {
		t.Fatalf("WriteBatch: %v", err)
	}
	if len(res.Outcomes) != 2 {
		t.Fatalf("outcomes = %d, want 2", len(res.Outcomes))
	}
	if res.Outcomes[0].Outcome != store.OutcomeInserted || res.Outcomes[1].Outcome != store.OutcomeMerged {
		t.Errorf("outcomes = %s then %s, want inserted then merged", res.Outcomes[0].Outcome, res.Outcomes[1].Outcome)
	}
	if res.Outcomes[0].SubmissionID == "" || res.Outcomes[0].SubmissionID != res.Outcomes[1].SubmissionID {
		t.Errorf("both observations must land on one submission: %s / %s",
			res.Outcomes[0].SubmissionID, res.Outcomes[1].SubmissionID)
	}
	if n := count(t, db, "ingest.observation", f.tenant); n != 2 {
		t.Errorf("observations = %d, want 2 (append-only, one row per observation)", n)
	}
	if n := count(t, db, "ingest.submission", f.tenant); n != 1 {
		t.Errorf("submissions = %d, want 1", n)
	}

	t.Run("a replay reports duplicate with the first receipt", func(t *testing.T) {
		res, err := st.WriteBatch(ctx, store.BatchWrite{
			TenantID: f.tenant, DeviceID: f.device, CredentialID: f.credential,
			ReceivedAt: time.Date(2026, 10, 2, 18, 0, 0, 0, time.UTC), // two hours later
			Accepted: []store.AcceptedEvent{
				{Index: 0, EventID: eventA, Route: "ext.web_request", Envelope: envelope(f, eventA, "ext.web_request", key, digest)},
			},
		})
		if err != nil {
			t.Fatalf("WriteBatch: %v", err)
		}
		got := res.Outcomes[0]
		if got.Outcome != store.OutcomeDuplicate {
			t.Fatalf("outcome = %s, want duplicate", got.Outcome)
		}
		if got.FirstReceivedAt == nil {
			t.Fatal("a duplicate must carry first_received_at")
		}
		// §5.3: the stored row keeps its first receipt; a retry is not a second receipt.
		if want := time.Date(2026, 10, 2, 14, 0, 1, 0, time.UTC); !got.FirstReceivedAt.UTC().Equal(want) {
			t.Errorf("first_received_at = %s, want the original receipt %s", got.FirstReceivedAt.UTC(), want)
		}
		if n := count(t, db, "ingest.observation", f.tenant); n != 2 {
			t.Errorf("observations = %d, want still 2: a replay is not a second observation", n)
		}
	})

	t.Run("a rejection is quarantined in the same transaction as the writes", func(t *testing.T) {
		res, err := st.WriteBatch(ctx, store.BatchWrite{
			TenantID: f.tenant, DeviceID: f.device, CredentialID: f.credential,
			ReceivedAt: time.Date(2026, 10, 2, 15, 0, 0, 0, time.UTC),
			Accepted:   []store.AcceptedEvent{},
			Rejected: []store.Rejection{{
				Index: 0, EventID: ladder.DeterministicUUID(9003),
				Reason:     protocol.ReasonUnknownKind,
				Detail:     &protocol.BatchRejectionDetail{Pointer: "/events/0/kind", Expected: "one of prompt | usage_rollup | model_detection"},
				Presence:   map[string][]string{"present": {"kind", "event_id"}, "missing": []string{}},
				Redacted:   map[string]json.RawMessage{"kind": json.RawMessage(`"process_telemetry"`)},
				Quarantine: true,
			}},
		})
		if err != nil {
			t.Fatalf("WriteBatch: %v", err)
		}
		if len(res.Outcomes) != 0 {
			t.Fatalf("outcomes = %d, want none", len(res.Outcomes))
		}
		if n := count(t, db, "ingest.rejected", f.tenant); n != 1 {
			t.Fatalf("rejected rows = %d, want 1", n)
		}
		var reason string
		if err := db.QueryRowContext(ctx,
			`SELECT reason_code FROM ingest.rejected WHERE tenant_id = $1::uuid`, f.tenant).Scan(&reason); err != nil {
			t.Fatalf("read rejected row: %v", err)
		}
		if reason != "unknown_kind" {
			t.Errorf("reason_code = %q, want unknown_kind", reason)
		}
	})
}

// TestTransactionBoundaryIsReal is the property the service's contract depends on and that only a
// driver can demonstrate: a write error in the middle of a batch leaves NOTHING committed.
//
// The failure is forced by a route that is not in ref.route_fidelity, which makes
// ingest.record_event() raise. The batch's first event would otherwise have been written.
func TestTransactionBoundaryIsReal(t *testing.T) {
	db := openLab(t)
	f := seed(t, db)
	ctx := context.Background()

	st, err := sqlpg.Open(parDSN())
	if err != nil {
		t.Fatalf("sqlpg.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	good := ladder.DeterministicUUID(9101)
	bad := ladder.DeterministicUUID(9102)
	key := ladder.Hash('c')
	digest := ladder.Hash('d')

	// The good one uses a valid route; the second is built with a route ref.route_fidelity does not
	// know, so the stored procedure raises on the second call and the transaction must abort.
	badEnvelope := envelope(f, bad, "ext.telepathy", key, digest)

	_, err = st.WriteBatch(ctx, store.BatchWrite{
		TenantID: f.tenant, DeviceID: f.device, CredentialID: f.credential,
		ReceivedAt: time.Date(2026, 10, 2, 14, 0, 0, 0, time.UTC),
		Accepted: []store.AcceptedEvent{
			{Index: 0, EventID: good, Route: "ext.web_request", Envelope: envelope(f, good, "ext.web_request", key, digest)},
			{Index: 1, EventID: bad, Route: "ext.telepathy", Envelope: badEnvelope},
		},
	})
	if err == nil {
		t.Fatal("the batch committed even though the second event's route does not exist")
	}
	t.Logf("the write failed as designed: %v", err)

	// Nothing at all: not the observation, not the submission. That is the whole claim.
	if n := count(t, db, "ingest.observation", f.tenant); n != 0 {
		t.Errorf("observations = %d, want 0: a write error must abort the whole batch (§6)", n)
	}
	if n := count(t, db, "ingest.submission", f.tenant); n != 0 {
		t.Errorf("submissions = %d, want 0", n)
	}

	t.Run("and the same batch succeeds once the bad event is removed", func(t *testing.T) {
		res, err := st.WriteBatch(ctx, store.BatchWrite{
			TenantID: f.tenant, DeviceID: f.device, CredentialID: f.credential,
			ReceivedAt: time.Date(2026, 10, 2, 14, 0, 0, 0, time.UTC),
			Accepted: []store.AcceptedEvent{
				{Index: 0, EventID: good, Route: "ext.web_request", Envelope: envelope(f, good, "ext.web_request", key, digest)},
			},
		})
		if err != nil {
			t.Fatalf("retry after the abort: %v", err)
		}
		if res.Outcomes[0].Outcome != store.OutcomeInserted {
			t.Errorf("outcome = %s, want inserted — a retry after a failed batch must be free", res.Outcomes[0].Outcome)
		}
		if n := count(t, db, "ingest.observation", f.tenant); n != 1 {
			t.Errorf("observations = %d, want 1", n)
		}
	})
}

// TestRevocationInsideTheTransaction covers §2.3 through a real connection: the credential is
// re-checked before commit, so a device revoked between admission and write leaves nothing behind.
func TestRevocationInsideTheTransaction(t *testing.T) {
	db := openLab(t)
	f := seed(t, db)
	ctx := context.Background()

	st, err := sqlpg.Open(parDSN())
	if err != nil {
		t.Fatalf("sqlpg.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if _, err := db.ExecContext(ctx,
		`UPDATE ops.device_credential SET revoked_at = now() WHERE tenant_id = $1::uuid`, f.tenant); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	eventID := ladder.DeterministicUUID(9201)
	_, err = st.WriteBatch(ctx, store.BatchWrite{
		TenantID: f.tenant, DeviceID: f.device, CredentialID: f.credential,
		ReceivedAt: time.Date(2026, 10, 2, 14, 0, 0, 0, time.UTC),
		Accepted: []store.AcceptedEvent{
			{Index: 0, EventID: eventID, Route: "ext.web_request",
				Envelope: envelope(f, eventID, "ext.web_request", ladder.Hash('e'), ladder.Hash('f'))},
		},
	})
	if err == nil {
		t.Fatal("a revoked credential wrote a batch")
	}
	if !strings.Contains(err.Error(), "revoked") {
		t.Errorf("error = %v, want it to name the revocation", err)
	}
	if n := count(t, db, "ingest.observation", f.tenant); n != 0 {
		t.Errorf("observations = %d, want 0: a revoked batch writes nothing", n)
	}
}
