package providers

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
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
