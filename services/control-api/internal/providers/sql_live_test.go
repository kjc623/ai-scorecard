package providers

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/shadow-ai-capture/control-api/internal/pgtest"
)

// The live test: connecting a provider and a pass that records its records, as sac_control against
// the database SAC_TEST_PG_DSN names, through the real row-level-security sessions, grants,
// constraints and ingest.record_provider_event. ops.audit is append-only and references the seeded
// tenant, so it stays behind, closed.

func TestStatementsPrepare(t *testing.T) {
	db := pgtest.OpenAs(t, "sac_control")
	ctx := context.Background()
	for i, s := range Statements {
		name := fmt.Sprintf("sac_providers_probe_%d", i)
		if _, err := db.ExecContext(ctx, "PREPARE "+name+" AS "+s); err != nil {
			t.Errorf("statement %d does not prepare: %v\n%s", i, err, s)
			continue
		}
		_, _ = db.ExecContext(ctx, "DEALLOCATE "+name)
	}
}

func TestConnectAndPullAgainstPostgres(t *testing.T) {
	db := pgtest.Open(t)
	ctx := context.Background()
	tenant := pgtest.UUID(t)
	if _, err := db.ExecContext(ctx, `INSERT INTO ops.tenant
	  (tenant_id, name, status, residency_region, ceiling_mode, collection_mode, content_search, device_identity)
	  VALUES ($1::uuid, $2, 'active', 'eu-west', 'm3', 'm3', 'full_text', 'clear')`, tenant, "providers-live-"+tenant[:8]); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	t.Cleanup(func() {
		for _, q := range []string{
			`DELETE FROM ops.usage_daily WHERE tenant_id = $1::uuid`,
			`UPDATE ops.provider_connection SET status = 'disabled', credential_enc = NULL WHERE tenant_id = $1::uuid`,
			`UPDATE ops.identity_connection SET status = 'disabled' WHERE tenant_id = $1::uuid`,
			`UPDATE ops.tenant SET status = 'closed', ingest_enabled = false, read_enabled = false,
			        status_reason = 'providers live test', status_changed_by = 'test', status_changed_at = now()
			  WHERE tenant_id = $1::uuid`,
		} {
			if _, err := db.ExecContext(context.Background(), q, tenant); err != nil {
				t.Logf("cleanup %q: %v", q, err)
			}
		}
	})

	control := pgtest.OpenAs(t, "sac_control")
	st := NewSQL(control)
	at := time.Now().UTC().Truncate(time.Second)
	connect := Audit{Actor: "admin@live.test", Action: "provider.connect", ObjectType: "provider_connection", At: at,
		Detail: map[string]any{"provider": ClaudeEnterprise}}

	if _, err := st.Providers(ctx, pgtest.UUID(t)); !errors.Is(err, ErrUnknownTenant) {
		t.Fatalf("Providers of an unknown tenant = %v", err)
	}
	list, err := st.Providers(ctx, tenant)
	if err != nil {
		t.Fatalf("Providers: %v", err)
	}
	byName := map[string]Provider{}
	for _, p := range list {
		byName[p.Provider] = p
		if len(p.Tools) == 0 || p.Connection != nil {
			t.Errorf("provider %+v", p)
		}
	}
	if c := byName[ClaudeEnterprise]; c.Credential != "api_key" || c.CredentialLabel == "" || c.AccountLabel == "" {
		t.Fatalf("Claude Enterprise is listed as %+v", c)
	}
	if m := byName[Microsoft365Copilot]; m.Credential != "entra_app" || len(m.Tools) != 1 || m.Tools[0].App != "m365_copilot" {
		t.Fatalf("Microsoft 365 Copilot is listed as %+v", m)
	}

	// A Microsoft provider is read through the tenant's Entra connection.
	if _, err := st.Connect(ctx, tenant, Microsoft365Copilot, "", nil, "", connect); !errors.Is(err, ErrNoEntra) {
		t.Fatalf("Connect Copilot without Entra = %v", err)
	}
	if _, err := st.Connect(ctx, tenant, "no_such_provider", "x", []byte("k"), "", connect); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Connect an unknown provider = %v", err)
	}
	id, err := st.Connect(ctx, tenant, ClaudeEnterprise, "org-1", []byte("sealed-1"), "", connect)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}

	// A pass reads the connection, records what the source reads, and files its outcome.
	targets, err := st.Targets(ctx)
	if err != nil {
		t.Fatalf("Targets: %v", err)
	}
	var target Target
	for _, tg := range targets {
		if tg.TenantID == tenant {
			target = tg
		}
	}
	if target.ConnectionID != id || target.Provider != ClaudeEnterprise || target.Account != "org-1" ||
		string(target.SealedKey) != "sealed-1" || target.Cursor != "" {
		t.Fatalf("target %+v", target)
	}
	if got, ok, err := st.Target(ctx, tenant, id); err != nil || !ok || got.ConnectionID != id {
		t.Fatalf("Target = %+v %v %v", got, ok, err)
	}

	unlock, ok, err := st.Lock(ctx, id)
	if err != nil || !ok {
		t.Fatalf("Lock = %v %v", ok, err)
	}
	if _, held, err := st.Lock(ctx, id); err != nil || held {
		t.Fatalf("a second Lock = %v %v; want held elsewhere", held, err)
	}
	unlock()

	when := at.Add(-time.Hour)
	first := Record{ID: "msg_live_1", OccurredAt: when, App: "claude_web", UPN: "ada@contoso.example", Text: "Summarise the Q3 forecast"}
	second := Record{ID: "msg_live_2", OccurredAt: when.Add(time.Minute), App: "claude_code", UPN: "grace@contoso.example", Text: "fix the failing test"}
	src := &fakeSource{rounds: []fakeRound{
		{records: []Record{first, second, {ID: "msg_live_3"}}, next: "cursor-1"},
		{records: []Record{first}, next: "cursor-2"},
	}}
	vault := &fakeVault{}
	runner, err := NewRunner(map[string]Source{ClaudeEnterprise: src}, st,
		&Recorder{Store: st, Classifier: &fakeClassifier{}, Vault: vault}, fakeOpener{}, fakeKeys{},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	runner.RunConnection(ctx, target)
	if len(src.calls) != 1 || src.calls[0].Key != "opened:sealed-1" {
		t.Fatalf("the source read %+v", src.calls)
	}
	var status, cursor string
	var read, added int64
	var lastError sql.NullString
	row := func() {
		t.Helper()
		if err := db.QueryRowContext(ctx, `SELECT last_status, last_error, cursor, records_read, records_new
		  FROM ops.provider_connection WHERE tenant_id = $1::uuid AND connection_id = $2::uuid`, tenant, id).
			Scan(&status, &lastError, &cursor, &read, &added); err != nil {
			t.Fatalf("read the connection: %v", err)
		}
	}
	row()
	if status != "ok" || lastError.Valid || cursor != "cursor-1" || read != 3 || added != 2 {
		t.Fatalf("after the first pass: %s %v %q read %d new %d", status, lastError, cursor, read, added)
	}

	// Each record is a submission the connection originates, filed under its tool, with its text
	// granted to content-vault at M3.
	var n, grants int
	var usage int64
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM ingest.submission
	  WHERE tenant_id = $1::uuid AND connection_id = $2::uuid AND device_id IS NULL
	    AND observed_routes = ARRAY['provider.api'] AND content_state = 'local_only'`, tenant, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM ops.grant WHERE tenant_id = $1::uuid AND connection_id = $2::uuid`, tenant, id).Scan(&grants); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT coalesce(sum(content_bytes_added), 0) FROM ops.usage_daily WHERE tenant_id = $1::uuid`, tenant).Scan(&usage); err != nil {
		t.Fatal(err)
	}
	if n != 2 || grants != 2 || len(vault.puts) != 2 || usage != int64(len(first.Text)+len(second.Text)) {
		t.Fatalf("%d submissions, %d grants, %d stored, %d bytes", n, grants, len(vault.puts), usage)
	}
	var apps string
	if err := db.QueryRowContext(ctx, `SELECT string_agg(ops.tool_id(tool_fingerprint), ',' ORDER BY first_occurred_at)
	  FROM ingest.submission WHERE tenant_id = $1::uuid AND connection_id = $2::uuid`, tenant, id).Scan(&apps); err != nil {
		t.Fatal(err)
	}
	if apps != "app:claude_web,app:claude_code" {
		t.Fatalf("filed under %q", apps)
	}

	// Read again, the record is the same event; its text still waits for content-vault, so it is
	// granted again.
	target.Cursor = cursor
	runner.RunConnection(ctx, target)
	row()
	if cursor != "cursor-2" || read != 4 || added != 2 || len(vault.puts) != 3 {
		t.Fatalf("after the second pass: %q read %d new %d, %d stored", cursor, read, added, len(vault.puts))
	}

	// Reconnecting to the same account resumes; to another, starts over.
	if _, err := st.Connect(ctx, tenant, ClaudeEnterprise, "org-1", []byte("sealed-2"), "", connect); err != nil {
		t.Fatal(err)
	}
	row()
	if cursor != "cursor-2" {
		t.Fatalf("a new key for the same account reset the cursor to %q", cursor)
	}
	var cur sql.NullString
	if again, err := st.Connect(ctx, tenant, ClaudeEnterprise, "org-2", []byte("sealed-3"), "", connect); err != nil || again != id {
		t.Fatalf("Connect another account = %q %v", again, err)
	}
	if err := db.QueryRowContext(ctx, `SELECT cursor FROM ops.provider_connection WHERE tenant_id = $1::uuid AND connection_id = $2::uuid`,
		tenant, id).Scan(&cur); err != nil || cur.Valid {
		t.Fatalf("another account kept the cursor %v (%v)", cur, err)
	}

	// The listing shows the connection and its last pass.
	list, err = st.Providers(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range list {
		if p.Provider == ClaudeEnterprise {
			c := p.Connection
			if c == nil || c.ConnectionID != id || c.Account != "org-2" || c.Status != "active" || c.LastStatus != "ok" ||
				c.RecordsRead != 4 || c.RecordsNew != 2 || c.LastCompletedAt == nil || c.CreatedBy != "admin@live.test" {
				t.Fatalf("listed connection %+v", c)
			}
		}
	}
	if got, active, err := st.Connection(ctx, tenant, ClaudeEnterprise); err != nil || got != id || !active {
		t.Fatalf("Connection = %q %v %v", got, active, err)
	}
	if _, _, err := st.Connection(ctx, tenant, ChatGPTEnterprise); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Connection to an unconnected provider = %v", err)
	}

	// Disconnecting forgets the key and stops the pulls; the records stay.
	disconnect := Audit{Actor: "admin@live.test", Action: "provider.disconnect", ObjectType: "provider_connection", At: at,
		Detail: map[string]any{"provider": ClaudeEnterprise}}
	if err := st.Disconnect(ctx, tenant, ClaudeEnterprise, disconnect); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	if err := st.Disconnect(ctx, tenant, ClaudeEnterprise, disconnect); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Disconnect twice = %v", err)
	}
	var sealed []byte
	if err := db.QueryRowContext(ctx, `SELECT status, credential_enc FROM ops.provider_connection WHERE tenant_id = $1::uuid AND connection_id = $2::uuid`,
		tenant, id).Scan(&status, &sealed); err != nil || status != "disabled" || sealed != nil {
		t.Fatalf("after Disconnect: %s %v (%v)", status, sealed, err)
	}
	if _, ok, err := st.Target(ctx, tenant, id); err != nil || ok {
		t.Fatalf("a disconnected connection is still a target: %v %v", ok, err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM ingest.submission WHERE tenant_id = $1::uuid AND connection_id = $2::uuid`,
		tenant, id).Scan(&n); err != nil || n != 2 {
		t.Fatalf("%d submissions after Disconnect (%v)", n, err)
	}

	// Each change is audited against the connection, and no audit carries the key.
	var actions string
	if err := db.QueryRowContext(ctx, `SELECT string_agg(action, ',' ORDER BY audit_seq) FROM ops.audit
	  WHERE tenant_id = $1::uuid AND object_type = 'provider_connection' AND object_id = $2`, tenant, id).Scan(&actions); err != nil {
		t.Fatal(err)
	}
	if actions != "provider.connect,provider.connect,provider.connect,provider.disconnect" {
		t.Fatalf("audits %q", actions)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM ops.audit WHERE tenant_id = $1::uuid AND detail::text LIKE '%sealed%'`,
		tenant).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d audits carry the key (%v)", n, err)
	}

	// Another tenant sees none of it.
	other := pgtest.Tenant(t, db, "eu-west")
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `UPDATE ops.tenant SET status = 'closed', ingest_enabled = false, read_enabled = false,
		  status_reason = 'providers live test', status_changed_by = 'test', status_changed_at = now() WHERE tenant_id = $1::uuid`, other)
	})
	list, err = st.Providers(ctx, other)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range list {
		if p.Connection != nil {
			t.Fatalf("another tenant sees the connection %+v", p.Connection)
		}
	}
	if _, ok, err := st.Target(ctx, other, id); err != nil || ok {
		t.Fatalf("another tenant reads the connection as a target: %v %v", ok, err)
	}
}

// The intake against Postgres: a token resolves its connection's tenant before any is known, a
// delivery is recorded under the connection and counted on it, and the token stops opening it when
// it is replaced or the connection is disconnected.
func TestIntakeAgainstPostgres(t *testing.T) {
	db := pgtest.Open(t)
	ctx := context.Background()
	tenant := pgtest.UUID(t)
	if _, err := db.ExecContext(ctx, `INSERT INTO ops.tenant
	  (tenant_id, name, status, residency_region, ceiling_mode, collection_mode, content_search, device_identity)
	  VALUES ($1::uuid, $2, 'active', 'eu-west', 'm1', 'm1', 'disabled', 'clear')`, tenant, "intake-live-"+tenant[:8]); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	t.Cleanup(func() {
		for _, q := range []string{
			`UPDATE ops.provider_connection SET status = 'disabled', intake_token_hash = NULL WHERE tenant_id = $1::uuid`,
			`UPDATE ops.tenant SET status = 'closed', ingest_enabled = false, read_enabled = false,
			        status_reason = 'intake live test', status_changed_by = 'test', status_changed_at = now()
			  WHERE tenant_id = $1::uuid`,
		} {
			if _, err := db.ExecContext(context.Background(), q, tenant); err != nil {
				t.Logf("cleanup %q: %v", q, err)
			}
		}
	})

	control := pgtest.OpenAs(t, "sac_control")
	st := NewSQL(control)
	connect := Audit{Actor: "admin@live.test", Action: "provider.connect", ObjectType: "provider_connection",
		At: time.Now().UTC(), Detail: map[string]any{"provider": PerplexityEnterprise, "intake_token": "issued"}}
	token, hash, err := NewIntakeToken()
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.Connect(ctx, tenant, PerplexityEnterprise, "", nil, hash, connect)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	for _, c := range []struct{ provider, connection, hash, want string }{
		{PerplexityEnterprise, id, hash, tenant},
		{AmazonBedrock, id, hash, ""},
		{PerplexityEnterprise, id, HashIntakeToken(token + "x"), ""},
		{PerplexityEnterprise, pgtest.UUID(t), hash, ""},
		{PerplexityEnterprise, "not-a-uuid", hash, ""},
	} {
		if got, err := st.IntakeTenant(ctx, c.provider, c.connection, c.hash); err != nil || got != c.want {
			t.Errorf("IntakeTenant(%s, %s) = %q %v, want %q", c.provider, c.connection, got, err, c.want)
		}
	}
	if open, err := st.IntakeOpen(ctx, tenant, id); err != nil || !open {
		t.Fatalf("IntakeOpen = %v %v", open, err)
	}
	// The runner never pulls an intake.
	if _, ok, err := st.Target(ctx, tenant, id); err != nil || ok {
		t.Fatalf("an intake connection is a pull target: %v %v", ok, err)
	}

	in, err := NewIntake(map[string]IntakeSource{PerplexityEnterprise: {Transport: &Webhook{}, Decode: DecodePerplexityEvent}},
		st, &Recorder{Store: st, Classifier: &fakeClassifier{}, Vault: &fakeVault{}}, fakeKeys{}, "https://app.live.test",
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	in.Register(mux)
	deliver := func(bearer string) int {
		event := `{"uuid":"` + id + `-q1","timestamp":"` + time.Now().UTC().Add(-time.Minute).Format(time.RFC3339) + `",
		  "event_type":"query","user_email":"ada@contoso.example","metadata":{"query_str":"draft the Q3 board update"}}`
		req := httptest.NewRequest(http.MethodPost, IntakePathPrefix+PerplexityEnterprise+"/"+id, strings.NewReader(event))
		req.Header.Set("Authorization", "Bearer "+bearer)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := deliver(token); code != http.StatusNoContent {
		t.Fatalf("delivery = %d", code)
	}
	if code := deliver(token); code != http.StatusNoContent {
		t.Fatalf("the delivery sent again = %d", code)
	}
	var n int
	var status, apps string
	var read, added int64
	if err := db.QueryRowContext(ctx, `SELECT count(*), string_agg(ops.tool_id(tool_fingerprint), ',') FROM ingest.submission
	  WHERE tenant_id = $1::uuid AND connection_id = $2::uuid AND observed_routes = ARRAY['provider.api']`, tenant, id).Scan(&n, &apps); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT last_status, records_read, records_new FROM ops.provider_connection
	  WHERE tenant_id = $1::uuid AND connection_id = $2::uuid`, tenant, id).Scan(&status, &read, &added); err != nil {
		t.Fatal(err)
	}
	if n != 1 || apps != "app:perplexity_web" || status != "ok" || read != 2 || added != 1 {
		t.Fatalf("%d submissions under %q; connection %s read %d new %d", n, apps, status, read, added)
	}

	// Connecting again issues a new token; the old one no longer opens the connection.
	next, nextHash, _ := NewIntakeToken()
	if again, err := st.Connect(ctx, tenant, PerplexityEnterprise, "", nil, nextHash, connect); err != nil || again != id {
		t.Fatalf("Connect again = %q %v", again, err)
	}
	if code := deliver(token); code != http.StatusUnauthorized {
		t.Fatalf("the replaced token = %d", code)
	}
	if code := deliver(next); code != http.StatusNoContent {
		t.Fatalf("the new token = %d", code)
	}

	disconnect := Audit{Actor: "admin@live.test", Action: "provider.disconnect", ObjectType: "provider_connection",
		At: time.Now().UTC(), Detail: map[string]any{"provider": PerplexityEnterprise}}
	if err := st.Disconnect(ctx, tenant, PerplexityEnterprise, disconnect); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	var stored sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT intake_token_hash FROM ops.provider_connection WHERE tenant_id = $1::uuid AND connection_id = $2::uuid`,
		tenant, id).Scan(&stored); err != nil || stored.Valid {
		t.Fatalf("after Disconnect the hash is %v (%v)", stored, err)
	}
	if code := deliver(next); code != http.StatusUnauthorized {
		t.Fatalf("a disconnected intake = %d", code)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM ops.audit WHERE tenant_id = $1::uuid AND detail::text LIKE '%' || $2 || '%'`,
		tenant, IntakeTokenPrefix).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d audits carry a token (%v)", n, err)
	}
}
