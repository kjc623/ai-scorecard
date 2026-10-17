package providers

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"
)

func newTestRunner(st *fakeStore, sources map[string]Source) *Runner {
	r, err := NewRunner(sources, st, &Recorder{Store: st, Classifier: &fakeClassifier{}, Vault: &fakeVault{}},
		fakeOpener{}, fakeKeys{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		panic(err)
	}
	return r
}

func target(provider string) Target {
	return Target{Connection: Connection{TenantID: conn.TenantID, ConnectionID: conn.ConnectionID, Provider: provider,
		Account: "ws-1", EntraTenantID: "", Cursor: "2026-10-12T00:00:00Z"}, SealedKey: []byte("k1")}
}

func rec(id string) Record {
	r := prompt()
	r.ID = id
	return r
}

func TestAPassRecordsWhatTheSourceReadsAndMovesTheCursor(t *testing.T) {
	st := newFakeStore()
	src := &fakeSource{rounds: []fakeRound{{records: []Record{rec("a"), rec("b"), {ID: "c"}}, next: "2026-10-13T00:00:00Z"}}}
	r := newTestRunner(st, map[string]Source{ClaudeEnterprise: src})
	r.RunConnection(context.Background(), target(ClaudeEnterprise))

	if st.started != 1 || len(st.finished) != 1 {
		t.Fatalf("started %d, finished %d", st.started, len(st.finished))
	}
	got := st.finished[0]
	if got != (Result{Read: 3, New: 2, Skipped: 1, Cursor: "2026-10-13T00:00:00Z"}) {
		t.Fatalf("result %+v", got)
	}
	c := src.calls[0]
	if c.Key != "opened:k1" || c.Cursor != "2026-10-12T00:00:00Z" || c.Account != "ws-1" {
		t.Fatalf("the source read %+v", c)
	}
}

func TestAProviderWithMoreIsReadAgainFromWhereThePassGot(t *testing.T) {
	st := newFakeStore()
	src := &fakeSource{rounds: []fakeRound{
		{records: []Record{rec("a")}, next: "t1", more: true},
		{records: []Record{rec("b")}, next: "t2", more: true},
		{records: []Record{rec("c")}, next: "t3"},
	}}
	r := newTestRunner(st, map[string]Source{ClaudeEnterprise: src})
	r.RunConnection(context.Background(), target(ClaudeEnterprise))
	if len(src.calls) != 3 || src.calls[1].Cursor != "t1" || src.calls[2].Cursor != "t2" {
		t.Fatalf("calls %+v", src.calls)
	}
	if got := st.finished[0]; got.Read != 3 || got.Cursor != "t3" || got.Error != "" {
		t.Fatalf("result %+v", got)
	}
}

func TestAFailedPassKeepsThePointItReached(t *testing.T) {
	st := newFakeStore()
	src := &fakeSource{rounds: []fakeRound{{records: []Record{rec("a")}, next: "t1", err: &APIError{Status: http.StatusTooManyRequests}}}}
	r := newTestRunner(st, map[string]Source{ChatGPTEnterprise: src})
	r.RunConnection(context.Background(), target(ChatGPTEnterprise))
	if got := st.finished[0]; got.Cursor != "t1" || got.Error != ErrRateLimited || got.New != 1 {
		t.Fatalf("result %+v", got)
	}
}

func TestFailuresAreClosedCodes(t *testing.T) {
	for _, c := range []struct {
		provider string
		err      error
		want     string
	}{
		{ChatGPTEnterprise, &APIError{Status: 401}, ErrCredentialRefused},
		{ClaudeEnterprise, &APIError{Status: 403}, ErrCredentialRefused},
		{Microsoft365Copilot, &APIError{Status: 403}, ErrConsentMissing},
		{Microsoft365Copilot, &TokenError{errors.New("AADSTS7000229")}, ErrConsentMissing},
		{ClaudeEnterprise, &APIError{Status: 429}, ErrRateLimited},
		{ClaudeEnterprise, &APIError{Status: 503}, ErrProviderFailed},
		{ClaudeEnterprise, errors.New("dial tcp: i/o timeout"), ErrProviderFailed},
		{ClaudeEnterprise, recordError{errors.New("deadlock")}, ErrInternal},
	} {
		if got := failureCode(c.provider, c.err); got != c.want {
			t.Errorf("%s %v: %s, want %s", c.provider, c.err, got, c.want)
		}
	}
}

func TestAConnectionThisDeploymentCannotReadFailsWithItsCode(t *testing.T) {
	st := newFakeStore()
	r := newTestRunner(st, map[string]Source{ClaudeEnterprise: &fakeSource{}})
	r.RunConnection(context.Background(), target(ChatGPTEnterprise))
	m := target(Microsoft365Copilot)
	m.SealedKey = nil
	r.Sources[Microsoft365Copilot] = &fakeSource{}
	r.RunConnection(context.Background(), m)
	bad := target(ClaudeEnterprise)
	bad.SealedKey = []byte("unreadable")
	r.RunConnection(context.Background(), bad)
	if len(st.finished) != 3 || st.finished[0].Error != ErrNotAvailable ||
		st.finished[1].Error != ErrEntraNotConnected || st.finished[2].Error != ErrInternal {
		t.Fatalf("results %+v", st.finished)
	}
}

func TestAConnectionAnotherReplicaHoldsIsLeftToIt(t *testing.T) {
	st := newFakeStore()
	st.locked = true
	src := &fakeSource{}
	newTestRunner(st, map[string]Source{ClaudeEnterprise: src}).RunConnection(context.Background(), target(ClaudeEnterprise))
	if st.started != 0 || len(src.calls) != 0 {
		t.Fatal("a held connection was pulled")
	}
}

func TestTriggerRunsTheNamedConnection(t *testing.T) {
	st := newFakeStore()
	st.targets = []Target{target(ClaudeEnterprise)}
	src := &fakeSource{rounds: []fakeRound{{records: []Record{rec("a")}, next: "t1"}}, pulled: make(chan struct{}, 4)}
	r := newTestRunner(st, map[string]Source{ClaudeEnterprise: src})
	if !r.Available(ClaudeEnterprise) || r.Available(ChatGPTEnterprise) {
		t.Fatal("availability")
	}
	if !r.Trigger(conn.TenantID, "c0000000-0000-4000-8000-00000000ffff") || !r.Trigger(conn.TenantID, conn.ConnectionID) {
		t.Fatal("trigger refused")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	select {
	case <-src.pulled:
	case <-time.After(5 * time.Second):
		t.Fatal("the triggered connection was not pulled")
	}
	cancel()
	<-done
	if len(src.calls) != 1 || len(st.finished) != 1 {
		t.Fatalf("%d pulls, %d results; want 1: the unknown connection is not pulled", len(src.calls), len(st.finished))
	}
}

func TestAConnectionIsDueItsScheduleAfterItsLastPassStarted(t *testing.T) {
	at := time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		started time.Time
		every   int
		due     bool
	}{
		{time.Time{}, 60, true},
		{at.Add(-59 * time.Minute), 60, false},
		{at.Add(-time.Hour), 60, true},
		{at.Add(-16 * time.Minute), 15, true},
		{at.Add(-5 * time.Hour), 360, false},
		{at.Add(-23 * time.Hour), 1440, false},
		// A schedule the store did not read is the default.
		{at.Add(-61 * time.Minute), 0, true},
	} {
		tg := Target{LastStartedAt: c.started, PullEveryMinutes: c.every}
		if got := tg.Due(at); got != c.due {
			t.Errorf("started %v, every %d minutes: due %v, want %v", c.started, c.every, got, c.due)
		}
	}
	if !NextPull(time.Time{}, 60).IsZero() {
		t.Fatal("a connection never pulled has a next pull time")
	}
	if got := NextPull(at, 360); !got.Equal(at.Add(6 * time.Hour)) {
		t.Fatalf("next pull %v", got)
	}
}

func TestRunDuePullsOnlyTheConnectionsWhoseScheduleHasCome(t *testing.T) {
	at := time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)
	scheduled := func(id string, started time.Time, every int) Target {
		tg := target(ClaudeEnterprise)
		tg.ConnectionID, tg.LastStartedAt, tg.PullEveryMinutes = id, started, every
		return tg
	}
	st := newFakeStore()
	st.targets = []Target{
		scheduled("c-new", time.Time{}, 60),
		scheduled("c-hourly-waiting", at.Add(-30*time.Minute), 60),
		scheduled("c-hourly-due", at.Add(-61*time.Minute), 60),
		scheduled("c-quarterly-due", at.Add(-15*time.Minute), 15),
		scheduled("c-daily-waiting", at.Add(-12*time.Hour), 1440),
	}
	src := &fakeSource{}
	r := newTestRunner(st, map[string]Source{ClaudeEnterprise: src})
	r.Now = func() time.Time { return at }
	r.RunDue(context.Background())
	var pulled []string
	for _, c := range src.calls {
		pulled = append(pulled, c.ConnectionID)
	}
	if want := "c-new c-hourly-due c-quarterly-due"; strings.Join(pulled, " ") != want {
		t.Fatalf("pulled %v, want %s", pulled, want)
	}
	// Each pass recorded its start, so a minute later nothing is due.
	at = at.Add(time.Minute)
	r.RunDue(context.Background())
	if len(src.calls) != 3 {
		t.Fatalf("%d pulls; a connection was pulled twice inside its schedule", len(src.calls))
	}
	// Its schedule after that pass, a connection is due again.
	at = at.Add(15 * time.Minute)
	r.RunDue(context.Background())
	if len(src.calls) != 4 || src.calls[3].ConnectionID != "c-quarterly-due" {
		t.Fatalf("calls %+v", src.calls)
	}
}

func TestAScheduledPassAnotherReplicaJustRanIsLeftAlone(t *testing.T) {
	at := time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)
	stale := target(ClaudeEnterprise)
	stale.LastStartedAt, stale.PullEveryMinutes = at.Add(-2*time.Hour), 60
	fresh := stale
	fresh.LastStartedAt, fresh.Cursor = at.Add(-time.Minute), "2026-10-17T08:59:00Z"
	st := newFakeStore()
	st.listed, st.targets = []Target{stale}, []Target{fresh}
	src := &fakeSource{}
	r := newTestRunner(st, map[string]Source{ClaudeEnterprise: src})
	r.Now = func() time.Time { return at }
	r.RunDue(context.Background())
	if len(src.calls) != 0 || st.started != 0 {
		t.Fatal("a connection another replica pulled a minute ago was pulled again from a stale list")
	}
	// Once it is due again, the pass resumes from the cursor that replica left.
	at = at.Add(time.Hour)
	r.RunDue(context.Background())
	if len(src.calls) != 1 || src.calls[0].Cursor != fresh.Cursor {
		t.Fatalf("calls %+v", src.calls)
	}
}

func TestPullNowRunsWhateverTheSchedule(t *testing.T) {
	at := time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)
	tg := target(ClaudeEnterprise)
	tg.LastStartedAt, tg.PullEveryMinutes = at.Add(-time.Minute), 1440
	st := newFakeStore()
	st.targets = []Target{tg}
	src := &fakeSource{}
	r := newTestRunner(st, map[string]Source{ClaudeEnterprise: src})
	r.Now = func() time.Time { return at }
	r.RunConnection(context.Background(), tg)
	if len(src.calls) != 1 {
		t.Fatal("a pull asked for was held to the schedule")
	}
}
