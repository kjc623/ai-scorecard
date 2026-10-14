package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"
	"time"
)

func TestEdgeTrustWatchAsksAfterTheGraceThenEveryRecheck(t *testing.T) {
	since := time.Unix(1_800_000_000, 0)
	var w edgeTrustWatch
	n := edgeTrustFailures
	if w.due(since.Add(time.Hour), time.Time{}, 0) {
		t.Fatal("due while the edge verifies")
	}
	if w.due(since.Add(edgeTrustGrace-time.Second), since, n) {
		t.Fatal("due before the grace ended")
	}
	asked := since.Add(edgeTrustGrace)
	if w.due(asked, since, n-1) {
		t.Fatal("due after too few failures")
	}
	if !w.due(asked, since, n) {
		t.Fatal("not due once the grace ended")
	}
	w.asked(asked)
	if w.due(asked.Add(edgeTrustRecheck-time.Second), since, n+1) {
		t.Fatal("due again before the recheck interval")
	}
	if !w.due(asked.Add(edgeTrustRecheck), since, n+1) {
		t.Fatal("not due again after the recheck interval")
	}
	// A verified handshake starts the watch over.
	if w.due(asked.Add(edgeTrustRecheck), time.Time{}, 0) {
		t.Fatal("due after the edge verified")
	}
	again := asked.Add(2 * edgeTrustRecheck)
	if !w.due(again.Add(edgeTrustGrace), again, n) {
		t.Fatal("a new run of failures is not asked about after its grace")
	}
}

func TestEdgeTrustCheckExitCodes(t *testing.T) {
	cloud := startFakeCloud(t, nil)
	var stderr bytes.Buffer
	if code := runEdgeTrustCheck(nil, &stderr); code != 2 {
		t.Fatalf("no arguments: exit %d, want 2", code)
	}
	if code := runEdgeTrustCheck([]string{cloud.srv.URL, cloud.caFile}, &stderr); code != 0 {
		t.Fatalf("an edge the CA file trusts: exit %d (%s), want 0", code, stderr.String())
	}
	stderr.Reset()
	if code := runEdgeTrustCheck([]string{cloud.srv.URL}, &stderr); code != 1 || stderr.Len() == 0 {
		t.Fatalf("an edge the roots do not trust: exit %d (%q), want 1 with the reason", code, stderr.String())
	}
}

// A service whose own checks of the edge fail restarts when a new process verifies the edge, does
// not restart when it does not, and does not restart twice within edgeTrustRestartGap.
func TestServiceRestartsWhenOnlyANewProcessVerifiesTheEdge(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cloud := startFakeCloud(t, nil)
	cfg := testConfig(t, cloud, pub)
	cfg.CAFile = ""
	svc, err := newService(context.Background(), cfg, testLogger(t))
	if err != nil {
		t.Fatalf("newService: %v", err)
	}
	since, failures := svc.drainer.UnverifiedSince()
	if failures == 0 {
		t.Fatal("the startup enrolment did not record a verification failure")
	}
	// The drain's later requests fail verification too.
	for range edgeTrustFailures {
		_, _ = svc.drainer.FetchPolicy(context.Background(), "")
	}
	ctx := context.Background()
	now := since.Add(edgeTrustGrace)
	var asked []string
	untrusted := func(_ context.Context, endpoint, caFile string) error {
		asked = append(asked, endpoint+" "+caFile)
		return errors.New("x509: certificate signed by unknown authority")
	}
	trusted := func(context.Context, string, string) error { return nil }

	var w edgeTrustWatch
	if svc.edgeTrustStep(ctx, &w, now, untrusted) {
		t.Fatal("restarted although the edge does not verify in a new process either")
	}
	if len(asked) != 1 || asked[0] != cloud.srv.URL+" " {
		t.Fatalf("asked %q, want the configured endpoint and no CA file", asked)
	}
	w = edgeTrustWatch{}
	if !svc.edgeTrustStep(ctx, &w, now, trusted) {
		t.Fatal("not restarted although a new process verifies the edge")
	}
	if last, ok := svc.lastEdgeRestart(); !ok || !last.Equal(now) {
		t.Fatalf("recorded restart = %v %v, want %v", last, ok, now)
	}
	w = edgeTrustWatch{}
	if svc.edgeTrustStep(ctx, &w, now.Add(edgeTrustRestartGap-time.Minute), trusted) {
		t.Fatal("restarted again within the gap")
	}
	w = edgeTrustWatch{}
	if !svc.edgeTrustStep(ctx, &w, now.Add(edgeTrustRestartGap), trusted) {
		t.Fatal("not restarted once the gap had passed")
	}
}

// The agent asking to be restarted stops it and ends run in errRestart, which the service manager
// restarts.
func TestRunEndsInErrRestartWhenTheAgentAsksToBeRestarted(t *testing.T) {
	cloud := startFakeCloud(t, nil)
	svc, err := newService(context.Background(), testConfig(t, cloud, nil), testLogger(t))
	if err != nil {
		t.Fatalf("newService: %v", err)
	}
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- svc.run(context.Background(), func() { close(started) }) }()
	select {
	case <-started:
	case err := <-done:
		t.Fatalf("run ended before it started: %v", err)
	}
	svc.requestRestart()
	svc.requestRestart()
	select {
	case err := <-done:
		if !errors.Is(err, errRestart) {
			t.Fatalf("run = %v, want errRestart", err)
		}
	case <-time.After(serviceStopBudget + 10*time.Second):
		t.Fatal("run did not stop after the restart request")
	}
}
