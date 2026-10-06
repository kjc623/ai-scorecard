package erase

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
)

const testTenant = "5d1c2f0e-8a3b-4c6d-9e7f-0a1b2c3d4e5f"
const testSubject = "u_test_subject"

type rowsAffected int64

func (n rowsAffected) LastInsertId() (int64, error) { return 0, nil }
func (n rowsAffected) RowsAffected() (int64, error) { return int64(n), nil }

// fakeExecer returns the next count from batches[query], then 0.
type fakeExecer struct {
	batches map[string][]int64
	failOn  string
	calls   []call
}

type call struct {
	query string
	args  []any
}

func (f *fakeExecer) ExecContext(_ context.Context, query string, args ...any) (sql.Result, error) {
	f.calls = append(f.calls, call{query: query, args: args})
	if query == f.failOn {
		return nil, errors.New("statement timeout")
	}
	queue := f.batches[query]
	if len(queue) == 0 {
		return rowsAffected(0), nil
	}
	f.batches[query] = queue[1:]
	return rowsAffected(queue[0]), nil
}

func TestDeleteAllDeletesInBatchesUntilAShortBatch(t *testing.T) {
	stmt := fmt.Sprintf(deleteBatchSQL, "ingest.observation", "event_id")
	f := &fakeExecer{batches: map[string][]int64{stmt: {3, 3, 1}}}
	n, err := deleteAll(context.Background(), f, "ingest.observation", "event_id", testTenant, testSubject, 3)
	if err != nil {
		t.Fatal(err)
	}
	if n != 7 {
		t.Fatalf("removed %d, want 7", n)
	}
	stmts := f.calls
	if len(stmts) != 3 {
		t.Fatalf("%d statements, want 3", len(stmts))
	}
	for _, c := range stmts {
		if len(c.args) != 3 || c.args[0] != testTenant || c.args[1] != testSubject || c.args[2] != 3 {
			t.Errorf("args %v, want [tenant subject 3]", c.args)
		}
	}
}

func TestDeleteAllSurfacesAFailedDelete(t *testing.T) {
	stmt := fmt.Sprintf(deleteBatchSQL, "ingest.submission", "submission_id")
	f := &fakeExecer{failOn: stmt}
	if _, err := deleteAll(context.Background(), f, "ingest.submission", "submission_id", testTenant, testSubject, BatchSize); err == nil || !strings.Contains(err.Error(), "ingest.submission") {
		t.Fatalf("err = %v, want one naming ingest.submission", err)
	}
}

func TestEraseStatementsAreScopedAndShapeCorrect(t *testing.T) {
	// Every statement is confined to the tenant and the subject, and the deletes are bounded.
	for _, stmt := range []string{contentDeleteSQL, searchTextDeleteSQL, findingCountSQL} {
		if !strings.Contains(stmt, "tenant_id = $1::uuid") || !strings.Contains(stmt, "user_ref = $2::text") {
			t.Errorf("statement is not tenant- and subject-scoped:\n%s", stmt)
		}
	}
	if !strings.Contains(contentDeleteSQL, "c.submission_id IN") || !strings.Contains(contentDeleteSQL, "c.event_id IN") {
		t.Errorf("content delete does not cover both submission and event linkage:\n%s", contentDeleteSQL)
	}
	if !strings.Contains(receiptSQL, "'subject'") || !strings.Contains(receiptSQL, "subject_ref") || !strings.Contains(receiptSQL, "'{database}'") {
		t.Errorf("receipt is not a subject receipt with a database mechanism:\n%s", receiptSQL)
	}
	if !strings.Contains(completeSQL, "completed_at = now()") {
		t.Errorf("request completion does not record completed_at:\n%s", completeSQL)
	}
	for _, spec := range []struct{ table, key string }{
		{"ingest.observation", "event_id"},
		{"ingest.submission", "submission_id"},
	} {
		stmt := fmt.Sprintf(deleteBatchSQL, spec.table, spec.key)
		for _, fragment := range []string{"WHERE tenant_id = $1::uuid", "user_ref = $2::text", "LIMIT $3::int"} {
			if !strings.Contains(stmt, fragment) {
				t.Errorf("%s delete lacks %q:\n%s", spec.table, fragment, stmt)
			}
		}
		if !strings.Contains(stmt, "DELETE FROM "+spec.table) {
			t.Errorf("%s delete does not delete from its own table:\n%s", spec.table, stmt)
		}
	}
}
