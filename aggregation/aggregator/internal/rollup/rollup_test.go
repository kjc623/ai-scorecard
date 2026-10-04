package rollup

import (
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
		"mart.agg_org_period":       false,
		"mart.agg_user_period":      false,
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

// TestAggregatesReplaceTheirBuckets is the C28 assertion at the text level: every statement must
// upsert and must not increment a bucket's own value.
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
			// An increment would look like `SET submissions = submissions + …`; the replacement form
			// always assigns `= EXCLUDED.…`.
			for _, bad := range []string{"submissions +", "bytes_total +", "users +", "blocked +"} {
				if strings.Contains(a.SQL, bad) {
					t.Errorf("%s (%s): increments a bucket (%q)", a.Name, bucketSize, bad)
				}
			}
			if !strings.Contains(a.SQL, "received_at >= $2") || !strings.Contains(a.SQL, "received_at < $3") {
				t.Errorf("%s (%s): window predicate missing", a.Name, bucketSize)
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
