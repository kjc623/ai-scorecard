package rollup

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"io"
	"strings"
	"testing"
	"time"
)

func at(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("bad test time %q: %v", s, err)
	}
	return ts
}

func TestWindowForDayCoversLookbackAndAlignsToNextDay(t *testing.T) {
	now := at(t, "2026-10-04T21:07:42Z")
	w, err := WindowFor(BucketDay, now, 7)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := w.From.Format(time.RFC3339), "2026-09-28T00:00:00Z"; got != want {
		t.Errorf("From = %s, want %s", got, want)
	}
	if got, want := w.To.Format(time.RFC3339), "2026-10-05T00:00:00Z"; got != want {
		t.Errorf("To = %s, want %s", got, want)
	}
}

func TestWindowForHourAlignsToNextHour(t *testing.T) {
	now := at(t, "2026-10-04T21:07:42Z")
	w, err := WindowFor(BucketHour, now, 48)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := w.From.Format(time.RFC3339), "2026-10-02T22:00:00Z"; got != want {
		t.Errorf("From = %s, want %s", got, want)
	}
	if got, want := w.To.Format(time.RFC3339), "2026-10-04T22:00:00Z"; got != want {
		t.Errorf("To = %s, want %s", got, want)
	}
}

func TestWindowForRejectsUnknownBucketAndLookback(t *testing.T) {
	now := at(t, "2026-10-04T21:07:42Z")
	if _, err := WindowFor("week", now, 7); err == nil {
		t.Error("WindowFor accepted an unknown bucket size")
	}
	if _, err := WindowFor(BucketDay, now, 0); err == nil {
		t.Error("WindowFor accepted a zero lookback")
	}
}

func TestLastCompleteBucketLeavesTheOpenBucketOut(t *testing.T) {
	now := at(t, "2026-10-04T21:07:42Z")
	day, err := LastCompleteBucket(BucketDay, now)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := day.Format(time.RFC3339), "2026-10-03T00:00:00Z"; got != want {
		t.Errorf("day = %s, want %s", got, want)
	}
	hour, err := LastCompleteBucket(BucketHour, now)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := hour.Format(time.RFC3339), "2026-10-04T20:00:00Z"; got != want {
		t.Errorf("hour = %s, want %s", got, want)
	}
}

func TestAggregatesRejectsUnknownBucket(t *testing.T) {
	if _, err := Aggregates("month"); err == nil {
		t.Error("Aggregates accepted an unknown bucket size")
	}
}

func TestAggregatesCoverTheMartUsageTables(t *testing.T) {
	aggs, err := Aggregates(BucketDay)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"mart.agg_tool_period":      false,
		"mart.agg_tool_user_period": false,
		"mart.agg_class_period":     false,
		"mart.agg_rule_period":      false,
		"mart.agg_org_period":       false,
		"mart.agg_team_period":      false,
		"mart.agg_user_period":      false,
		"mart.agg_discovery_period": false,
		"mart.agg_agent_period":     false,
	}
	for _, a := range aggs {
		if _, ok := want[a.Name]; !ok {
			t.Errorf("unexpected aggregate %q", a.Name)
			continue
		}
		want[a.Name] = true
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("aggregate %s missing", name)
		}
	}
}

// TestAggregatesReplaceTheirBuckets checks at the text level that every statement upserts and
// none increments a bucket's own value.
func TestAggregatesReplaceTheirBuckets(t *testing.T) {
	for _, bucketSize := range BucketSizes {
		aggs, err := Aggregates(bucketSize)
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range aggs {
			if !strings.Contains(a.SQL, "ON CONFLICT") || !strings.Contains(a.SQL, "DO UPDATE") {
				t.Errorf("%s (%s): not an upsert", a.Name, bucketSize)
			}
			// An increment would read `SET submissions = submissions + …`; the replacement form
			// always assigns `= EXCLUDED.…`.
			for _, bad := range []string{"submissions +", "bytes_total +", "users +", "blocked +"} {
				if strings.Contains(a.SQL, bad) {
					t.Errorf("%s (%s): increments a bucket (%q)", a.Name, bucketSize, bad)
				}
			}
			if !strings.Contains(a.SQL, "received_at >= $2") || !strings.Contains(a.SQL, "received_at < $3") {
				t.Errorf("%s (%s): window predicate missing", a.Name, bucketSize)
			}
			if !strings.Contains(a.SQL, "'"+bucketSize+"'") {
				t.Errorf("%s (%s): bucket size literal missing", a.Name, bucketSize)
			}
		}
	}
}

func TestWatermarkSQLIsAnUpsert(t *testing.T) {
	if !strings.Contains(WatermarkSQL, "ON CONFLICT") || !strings.Contains(WatermarkSQL, "DO UPDATE") {
		t.Fatal("WatermarkSQL is not an upsert")
	}
	if !strings.Contains(WatermarkSQL, "last_run_at") {
		t.Fatal("WatermarkSQL does not record last_run_at")
	}
}

// TestFindingsSQLIsIdempotentAndPresentTense checks at the text level that only a published rule
// raises a finding, that re-evaluation cannot duplicate one, and that a finding stores the match
// rather than the rule's current attributes.
func TestFindingsSQLIsIdempotentAndPresentTense(t *testing.T) {
	if !strings.Contains(FindingsSQL, "ON CONFLICT (tenant_id, submission_id, rule_id) DO NOTHING") {
		t.Error("FindingsSQL does not use the finding natural key with DO NOTHING, so a re-run could duplicate")
	}
	if strings.Contains(FindingsSQL, "DO UPDATE") {
		t.Error("FindingsSQL upserts; a finding that already exists must never be rewritten")
	}
	if !strings.Contains(FindingsSQL, "JOIN ref.rule") {
		t.Error("FindingsSQL does not join ref.rule, so an unpublished rule_id could raise a finding")
	}
	if !strings.Contains(FindingsSQL, "s.received_at >= $2") || !strings.Contains(FindingsSQL, "s.received_at < $3") {
		t.Error("FindingsSQL is missing the half-open received_at window")
	}
	if strings.Contains(FindingsSQL, "severity") || strings.Contains(FindingsSQL, "class_code") {
		t.Error("FindingsSQL stores severity/class_code; those are present-tense ref.rule configuration")
	}
}

// recorder is an Execer that records every statement and reports a fixed row count.
type recorder struct {
	rows  int64
	moved *time.Time
	calls []call
}

type call struct {
	query string
	args  []any
}

func (r *recorder) ExecContext(_ context.Context, query string, args ...any) (sql.Result, error) {
	r.calls = append(r.calls, call{query: query, args: args})
	return driverResult(r.rows), nil
}

// QueryRowContext records the statement and answers it with one row holding r.moved (NULL when
// nil), through a driver that serves nothing else, because a *sql.Row comes only from a *sql.DB.
func (r *recorder) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	r.calls = append(r.calls, call{query: query, args: args})
	var v driver.Value
	if r.moved != nil {
		v = *r.moved
	}
	return oneRowDB(v).QueryRowContext(ctx, "row")
}

// oneRowDB is a *sql.DB whose every query returns one row with one column holding v.
func oneRowDB(v driver.Value) *sql.DB {
	return sql.OpenDB(oneRowConnector{v: v})
}

type oneRowDriver struct{}

func (oneRowDriver) Open(string) (driver.Conn, error) { return oneRowConn{}, nil }

type oneRowConnector struct{ v driver.Value }

func (c oneRowConnector) Connect(context.Context) (driver.Conn, error) {
	return oneRowConn{v: c.v}, nil
}
func (oneRowConnector) Driver() driver.Driver { return oneRowDriver{} }

type oneRowConn struct{ v driver.Value }

func (c oneRowConn) Prepare(string) (driver.Stmt, error) { return oneRowStmt(c), nil }
func (oneRowConn) Close() error                          { return nil }
func (oneRowConn) Begin() (driver.Tx, error)             { return nil, io.EOF }

type oneRowStmt struct{ v driver.Value }

func (oneRowStmt) Close() error                               { return nil }
func (oneRowStmt) NumInput() int                              { return -1 }
func (oneRowStmt) Exec([]driver.Value) (driver.Result, error) { return nil, io.EOF }
func (s oneRowStmt) Query([]driver.Value) (driver.Rows, error) {
	return &oneRows{v: s.v}, nil
}

type oneRows struct {
	v    driver.Value
	done bool
}

func (*oneRows) Columns() []string { return []string{"v"} }
func (*oneRows) Close() error      { return nil }
func (r *oneRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	dest[0] = r.v
	return nil
}

type driverResult int64

func (n driverResult) LastInsertId() (int64, error) { return 0, nil }
func (n driverResult) RowsAffected() (int64, error) { return int64(n), nil }

// TestTenantWritesEachAggregateThenItsWatermark checks the statement sequence of one tenant's
// pass: the person refs moved first, then every aggregate followed by its watermark carrying that
// aggregate's row count, then findings, then coverage, each with the tenant and its window.
func TestTenantWritesEachAggregateThenItsWatermark(t *testing.T) {
	const tenant = "5d1c2f0e-8a3b-4c6d-9e7f-0a1b2c3d4e5f"
	now := at(t, "2026-10-04T21:07:42Z")
	rec := &recorder{rows: 3}

	written, err := Tenant(context.Background(), rec, tenant, now)
	if err != nil {
		t.Fatal(err)
	}

	if rec.calls[0].query != PersonRefSQL || rec.calls[0].args[0] != tenant {
		t.Fatalf("the pass does not start by moving person refs for the tenant: %v", rec.calls[0])
	}
	i := 1
	for _, bucketSize := range BucketSizes {
		lookback := HourLookback
		if bucketSize == BucketDay {
			lookback = DayLookback
		}
		window, _ := WindowFor(bucketSize, now, lookback)
		lastComplete, _ := LastCompleteBucket(bucketSize, now)
		aggs, _ := Aggregates(bucketSize)
		for _, a := range aggs {
			agg, wm := rec.calls[i], rec.calls[i+1]
			i += 2
			if agg.query != a.SQL {
				t.Fatalf("statement %d is not %s (%s)", i-2, a.Name, bucketSize)
			}
			if agg.args[0] != tenant || agg.args[1] != window.From || agg.args[2] != window.To {
				t.Errorf("%s (%s): args %v, want tenant and window %v..%v", a.Name, bucketSize, agg.args, window.From, window.To)
			}
			if wm.query != WatermarkSQL {
				t.Fatalf("%s (%s) is not followed by its watermark", a.Name, bucketSize)
			}
			wantWM := []any{tenant, a.Name, bucketSize, lastComplete, int64(3)}
			for k := range wantWM {
				if wm.args[k] != wantWM[k] {
					t.Errorf("%s (%s) watermark arg %d = %v, want %v", a.Name, bucketSize, k, wm.args[k], wantWM[k])
				}
			}
			if written[a.Name+" "+bucketSize] != 3 {
				t.Errorf("written[%q] = %d, want 3", a.Name+" "+bucketSize, written[a.Name+" "+bucketSize])
			}
		}
	}
	if len(rec.calls) != i+2 {
		t.Fatalf("got %d statements, want %d", len(rec.calls), i+2)
	}
	if rec.calls[i].query != FindingsSQL || rec.calls[i+1].query != CoverageSnapshotSQL {
		t.Error("findings and coverage are not the last two statements")
	}
	if written[FindingName] != 3 || written[CoverageSnapshotName] != 3 {
		t.Errorf("findings/coverage counts missing from %v", written)
	}
}

// TestTenantRecomputesTheBucketsOfMovedRows checks that a pass which moved submissions to a
// person's canonical ref recomputes every bucket they fall in, older than the trailing window too,
// and leaves the windows alone when the moved rows are inside them.
func TestTenantRecomputesTheBucketsOfMovedRows(t *testing.T) {
	const tenant = "5d1c2f0e-8a3b-4c6d-9e7f-0a1b2c3d4e5f"
	now := at(t, "2026-10-04T21:07:42Z")
	for _, tc := range []struct {
		moved             string
		wantHour, wantDay string
	}{
		{"2026-09-20T13:45:00Z", "2026-09-20T13:00:00Z", "2026-09-20T00:00:00Z"},
		{"2026-10-04T20:10:00Z", "2026-10-02T22:00:00Z", "2026-09-28T00:00:00Z"},
	} {
		moved := at(t, tc.moved)
		rec := &recorder{rows: 1, moved: &moved}
		if _, err := Tenant(context.Background(), rec, tenant, now); err != nil {
			t.Fatal(err)
		}
		hourAgg, _ := Aggregates(BucketHour)
		dayAgg, _ := Aggregates(BucketDay)
		for _, c := range rec.calls {
			want := ""
			switch c.query {
			case hourAgg[0].SQL:
				want = tc.wantHour
			case dayAgg[0].SQL:
				want = tc.wantDay
			default:
				continue
			}
			if got := c.args[1].(time.Time).Format(time.RFC3339); got != want {
				t.Errorf("moved at %s: window starts %s, want %s", tc.moved, got, want)
			}
		}
	}
}
