package tenant

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"testing"

	"github.com/shadow-ai-capture/jobs/internal/pgtest"
)

// TestIDsListsTenantsForTheJobsRole checks that sac_ops, which forced row-level security keeps
// from reading ops.tenant directly, still gets every tenant from IDs.
func TestIDsListsTenantsForTheJobsRole(t *testing.T) {
	tx := pgtest.Tx(t, pgtest.Open(t))
	id, _ := pgtest.Tenant(t, tx)
	pgtest.Exec(t, tx, `SELECT set_config('app.tenant_id', '', true)`)

	pgtest.AsJobs(t, tx)
	if got := pgtest.Scalar(t, tx, `SELECT count(*)::text FROM ops.tenant WHERE tenant_id = $1`, id); got != "0" {
		t.Fatalf("sac_ops with no tenant set reads ops.tenant directly (%s rows); the test proves nothing", got)
	}
	ids, err := IDs(context.Background(), tx)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(ids, id) {
		t.Errorf("IDs did not return tenant %s", id)
	}
	if !slices.IsSorted(ids) {
		t.Error("IDs are not in a stable order")
	}
}

// TestRunScopesTheTransactionAndRollsBackOnError checks that fn sees the tenant on its
// transaction and that an error from fn is returned rather than committed. Nothing is written.
func TestRunScopesTheTransactionAndRollsBackOnError(t *testing.T) {
	db := pgtest.Open(t)
	db.SetMaxOpenConns(1) // the leak check below must reuse the connection Run used
	const id = "0f6b8f0c-3f1e-4a52-9d7e-6c1d2b3a4f50"
	stop := errors.New("stop")
	err := Run(context.Background(), db, id, func(tx *sql.Tx) error {
		if got := pgtest.Scalar(t, tx, `SELECT current_setting('app.tenant_id')`); got != id {
			t.Errorf("app.tenant_id = %q, want %q", got, id)
		}
		return stop
	})
	if !errors.Is(err, stop) {
		t.Fatalf("Run returned %v, want fn's error", err)
	}
	var leaked sql.NullString
	if err := db.QueryRowContext(context.Background(), `SELECT nullif(current_setting('app.tenant_id', true), '')`).Scan(&leaked); err != nil {
		t.Fatal(err)
	}
	if leaked.Valid {
		t.Errorf("app.tenant_id outlived the transaction: %q", leaked.String)
	}
}
