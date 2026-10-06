package expire

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

const testTenant = "5d1c2f0e-8a3b-4c6d-9e7f-0a1b2c3d4e5f"

// fakeTx is an Execer that records every statement. Each table's DELETE returns the next count
// from batches[table], then 0 once the list is exhausted; failOn makes that table's DELETE fail.
type fakeTx struct {
	batches map[string][]int64
	failOn  string
	calls   []call
}

type call struct {
	query string
	args  []any
}

type rowsAffected int64

func (n rowsAffected) LastInsertId() (int64, error) { return 0, nil }
func (n rowsAffected) RowsAffected() (int64, error) { return int64(n), nil }

func (f *fakeTx) ExecContext(_ context.Context, query string, args ...any) (sql.Result, error) {
	f.calls = append(f.calls, call{query: query, args: args})
	for _, t := range tables {
		if query != t.deleteSQL {
			continue
		}
		if t.name == f.failOn {
			return nil, errors.New("statement timeout")
		}
		queue := f.batches[t.name]
		if len(queue) == 0 {
			return rowsAffected(0), nil
		}
		f.batches[t.name] = queue[1:]
		return rowsAffected(queue[0]), nil
	}
	return rowsAffected(1), nil
}

func (f *fakeTx) statements(query string) []call {
	var out []call
	for _, c := range f.calls {
		if c.query == query {
			out = append(out, c)
		}
	}
	return out
}

func tableNamed(t *testing.T, name string) table {
	t.Helper()
	for _, tb := range tables {
		if tb.name == name {
			return tb
		}
	}
	t.Fatalf("no table %s", name)
	return table{}
}

func TestDeletesInBatchesUntilABatchComesBackShort(t *testing.T) {
	tx := &fakeTx{batches: map[string][]int64{
		"ingest.observation": {3, 3, 1},
		"ingest.submission":  {3, 3},
	}}
	removed, err := expireTenant(context.Background(), tx, testTenant, 3)
	if err != nil {
		t.Fatal(err)
	}
	// Three batches for 7 observations (the third is short); three for 6 submissions (the third
	// finds nothing left); one for every table with nothing expired.
	want := map[string]struct {
		statements int
		removed    int64
	}{
		"ingest.search_text": {1, 0},
		"ops.content":        {1, 0},
		"ingest.observation": {3, 7},
		"ingest.submission":  {3, 6},
		"ingest.rejected":    {1, 0},
	}
	for name, w := range want {
		stmts := tx.statements(tableNamed(t, name).deleteSQL)
		if len(stmts) != w.statements {
			t.Errorf("%s: %d DELETE statements, want %d", name, len(stmts), w.statements)
		}
		for _, s := range stmts {
			if len(s.args) != 2 || s.args[0] != testTenant || s.args[1] != 3 {
				t.Errorf("%s: DELETE args %v, want [tenant 3]", name, s.args)
			}
		}
		if removed[name] != w.removed {
			t.Errorf("removed[%s] = %d, want %d", name, removed[name], w.removed)
		}
	}
}

func TestOpensTheRetentionPathBeforeAnyDelete(t *testing.T) {
	tx := &fakeTx{batches: map[string][]int64{}}
	if _, err := expireTenant(context.Background(), tx, testTenant, BatchSize); err != nil {
		t.Fatal(err)
	}
	if tx.calls[0].query != retentionPathSQL {
		t.Fatalf("first statement is %q, want the retention path flag", tx.calls[0].query)
	}
}

func TestSweepsEveryRetainedTableInOrder(t *testing.T) {
	tx := &fakeTx{batches: map[string][]int64{}}
	if _, err := expireTenant(context.Background(), tx, testTenant, BatchSize); err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, c := range tx.calls {
		for _, tb := range tables {
			if c.query == tb.deleteSQL {
				order = append(order, tb.name)
			}
		}
	}
	want := []string{"ingest.search_text", "ops.content", "ingest.observation", "ingest.submission", "ingest.rejected"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Errorf("deletion order %v, want %v", order, want)
	}
}

func TestNoReceiptWhenNothingExpired(t *testing.T) {
	tx := &fakeTx{batches: map[string][]int64{}}
	removed, err := expireTenant(context.Background(), tx, testTenant, BatchSize)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(tx.statements(receiptSQL)); n != 0 {
		t.Fatalf("%d receipts written for a pass that removed nothing", n)
	}
	for name, n := range removed {
		if n != 0 {
			t.Errorf("removed[%s] = %d, want 0", name, n)
		}
	}
}

func TestOneReceiptStatesWhatWasRemovedFromEachTable(t *testing.T) {
	tx := &fakeTx{batches: map[string][]int64{
		"ingest.search_text": {2},
		"ops.content":        {1},
		"ingest.rejected":    {4},
	}}
	if _, err := expireTenant(context.Background(), tx, testTenant, BatchSize); err != nil {
		t.Fatal(err)
	}
	receipts := tx.statements(receiptSQL)
	if len(receipts) != 1 {
		t.Fatalf("%d receipts, want 1", len(receipts))
	}
	if tx.calls[len(tx.calls)-1].query != receiptSQL {
		t.Error("the receipt is not written after every DELETE")
	}
	r := receipts[0]
	if r.args[0] != testTenant {
		t.Errorf("receipt tenant = %v", r.args[0])
	}
	var counts map[string]int64
	if err := json.Unmarshal([]byte(r.args[1].(string)), &counts); err != nil {
		t.Fatalf("removed_counts is not JSON: %v", err)
	}
	want := map[string]int64{
		"ingest.search_text": 2, "ops.content": 1, "ingest.observation": 0,
		"ingest.submission": 0, "ingest.rejected": 4,
	}
	if len(counts) != len(want) {
		t.Errorf("removed_counts = %v, want %v", counts, want)
	}
	for name, n := range want {
		if got, ok := counts[name]; !ok || got != n {
			t.Errorf("removed_counts[%s] = %d (present %v), want %d", name, got, ok, n)
		}
	}
	for _, fragment := range []string{"'retention'", "'expire-job'", "'{database}'", "completed_at"} {
		if !strings.Contains(receiptSQL, fragment) {
			t.Errorf("receipt statement does not record %s", fragment)
		}
	}
}

func TestAFailedDeleteNamesTheTableAndWritesNoReceipt(t *testing.T) {
	tx := &fakeTx{batches: map[string][]int64{"ingest.search_text": {5}}, failOn: "ops.content"}
	_, err := expireTenant(context.Background(), tx, testTenant, BatchSize)
	if err == nil || !strings.Contains(err.Error(), "ops.content") {
		t.Fatalf("err = %v, want one naming ops.content", err)
	}
	if n := len(tx.statements(receiptSQL)); n != 0 {
		t.Errorf("%d receipts written by a failed pass", n)
	}
}

func TestDeleteStatementsAreTenantScopedAndBounded(t *testing.T) {
	for _, tb := range tables {
		for _, fragment := range []string{
			"WHERE tenant_id = $1 AND expires_at < now()",
			"LIMIT $2",
		} {
			if !strings.Contains(tb.deleteSQL, fragment) {
				t.Errorf("%s: DELETE lacks %q:\n%s", tb.name, fragment, tb.deleteSQL)
			}
		}
		// The DELETE must be the outer statement, so its row count is the rows removed, and it must
		// be confined to the tenant as well as to the batch.
		outer := tb.deleteSQL[strings.LastIndex(tb.deleteSQL, "\nDELETE FROM "):]
		if !strings.HasPrefix(outer, "\nDELETE FROM "+tb.name) {
			t.Errorf("%s: the outer statement is not its DELETE:\n%s", tb.name, tb.deleteSQL)
		}
		if !strings.Contains(outer, "tenant_id = $1") {
			t.Errorf("%s: the DELETE is not confined to the tenant:\n%s", tb.name, outer)
		}
	}
}

// TestExpiredContentMarksItsSubmissionShredded checks that the content DELETE also marks the
// submission that claimed the content, and only one that still claims it.
func TestExpiredContentMarksItsSubmissionShredded(t *testing.T) {
	stmt := tableNamed(t, "ops.content").deleteSQL
	for _, fragment := range []string{
		"UPDATE ingest.submission s",
		"SET content_state = 'shredded', shredded_reason = 'retention_expired'",
		"s.tenant_id = $1",
		"s.submission_id = batch.submission_id",
		"s.content_state = 'uploaded'",
		"DELETE FROM ops.content c\n USING batch",
		"c.object_id = batch.object_id",
	} {
		if !strings.Contains(stmt, fragment) {
			t.Errorf("content DELETE lacks %q:\n%s", fragment, stmt)
		}
	}
	// Both the UPDATE and the DELETE read the one batch, and only the DELETE is counted.
	if strings.Count(stmt, "FROM ops.content") != 2 || strings.Count(stmt, "LIMIT") != 1 {
		t.Errorf("content DELETE does not select exactly one batch:\n%s", stmt)
	}
}
