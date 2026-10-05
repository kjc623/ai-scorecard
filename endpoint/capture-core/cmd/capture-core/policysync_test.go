package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/drain"
	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/protocol"
)

// signedTestBundle signs a minimal bundle of the given version with priv.
func signedTestBundle(t *testing.T, priv ed25519.PrivateKey, version string, mode protocol.CollectionMode) []byte {
	t.Helper()
	raw, err := policy.Sign("policy-key-1", priv, &policy.Bundle{
		Version:       version,
		EffectiveAt:   time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC),
		TenantDefault: mode,
		Interception:  policy.Interception{ProxyListen: "127.0.0.1:8843", ProxyCanary: "api.anthropic.com:443"},
		Classifier:    policy.ClassifierRelease{ReleaseID: "rel-1", State: policy.ReleaseEnforcing},
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

type fetchAnswer struct {
	fetch drain.PolicyFetch
	err   error
}

// scriptedFetcher answers GET /v1/policy from a script and records each If-None-Match it was sent.
type scriptedFetcher struct {
	answers []fetchAnswer
	etags   []string
}

func (f *scriptedFetcher) FetchPolicy(_ context.Context, etag string) (drain.PolicyFetch, error) {
	f.etags = append(f.etags, etag)
	a := f.answers[0]
	f.answers = f.answers[1:]
	return a.fetch, a.err
}

type syncFixture struct {
	store   *policy.Store
	cache   policyCache
	fetcher *scriptedFetcher
	sync    *policySync
	results []policy.Result
	priv    ed25519.PrivateKey
}

func newSyncFixture(t *testing.T) *syncFixture {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	v, err := policy.NewVerifier("policy-key-1", pub)
	if err != nil {
		t.Fatal(err)
	}
	store, err := policy.NewStore(v, nil)
	if err != nil {
		t.Fatal(err)
	}
	f := &syncFixture{store: store, cache: policyCache{dir: filepath.Join(t.TempDir(), policyCacheDir)}, fetcher: &scriptedFetcher{}, priv: priv}
	f.sync = newPolicySync(store, f.cache, f.fetcher, func(r policy.Result) { f.results = append(f.results, r) },
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	return f
}

func (f *syncFixture) answer(a fetchAnswer) { f.fetcher.answers = append(f.fetcher.answers, a) }

func (f *syncFixture) inForce(t *testing.T) string {
	t.Helper()
	if b := f.store.InForce(); b != nil {
		return b.Version
	}
	return ""
}

func TestPolicySyncAcceptsCachesAndHonoursTheServerCadence(t *testing.T) {
	f := newSyncFixture(t)
	v5 := signedTestBundle(t, f.priv, "5", protocol.ModeM1)
	f.answer(fetchAnswer{fetch: drain.PolicyFetch{Status: drain.PolicyServed, Envelope: v5, ETag: `"5"`, NextPoll: 10 * time.Minute}})

	wait := f.sync.once(context.Background())
	if f.inForce(t) != "5" || f.results[0].Outcome != policy.OutcomeAccepted {
		t.Fatalf("in force %q, result %+v; want version 5 accepted", f.inForce(t), f.results)
	}
	if wait != 10*time.Minute {
		t.Fatalf("next fetch in %s, want the server's 10m", wait)
	}
	raw, etag, err := f.cache.load()
	if err != nil || string(raw) != string(v5) || etag != `"5"` {
		t.Fatalf("cache = %q %q %v; want the verified bytes and their ETag", raw, etag, err)
	}
	if f.fetcher.etags[0] != "" {
		t.Fatalf("first fetch sent If-None-Match %q with nothing in force", f.fetcher.etags[0])
	}
}

func TestPolicySyncNotModifiedKeepsTheBundle(t *testing.T) {
	f := newSyncFixture(t)
	f.answer(fetchAnswer{fetch: drain.PolicyFetch{Status: drain.PolicyServed, Envelope: signedTestBundle(t, f.priv, "5", protocol.ModeM1), ETag: `"5"`}})
	f.answer(fetchAnswer{fetch: drain.PolicyFetch{Status: drain.PolicyNotModified, ETag: `"5"`}})
	f.sync.once(context.Background())
	wait := f.sync.once(context.Background())
	if f.fetcher.etags[1] != `"5"` {
		t.Fatalf("second fetch sent If-None-Match %q, want the version in force", f.fetcher.etags[1])
	}
	if f.inForce(t) != "5" || len(f.results) != 1 {
		t.Fatalf("after 304: in force %q, results %d; a 304 changes nothing", f.inForce(t), len(f.results))
	}
	if wait != defaultPolicyInterval {
		t.Fatalf("next fetch in %s, want the default %s when the server states no cadence", wait, defaultPolicyInterval)
	}
}

// docs/01 §13.3: a bundle that fails verification is discarded and the previous one stays in force;
// it is not cached, so a restart cannot pick it up either; polling backs off.
func TestPolicySyncBadSignatureKeepsThePreviousBundle(t *testing.T) {
	f := newSyncFixture(t)
	v5 := signedTestBundle(t, f.priv, "5", protocol.ModeM1)
	tampered := []byte(strings.Replace(string(signedTestBundle(t, f.priv, "6", protocol.ModeM1)), `"m1"`, `"m3"`, 1))
	f.answer(fetchAnswer{fetch: drain.PolicyFetch{Status: drain.PolicyServed, Envelope: v5, ETag: `"5"`}})
	f.answer(fetchAnswer{fetch: drain.PolicyFetch{Status: drain.PolicyServed, Envelope: tampered, ETag: `"6"`}})
	f.sync.once(context.Background())
	f.sync.once(context.Background())

	if f.inForce(t) != "5" {
		t.Fatalf("in force %q after a tampered bundle, want 5", f.inForce(t))
	}
	last := f.results[len(f.results)-1]
	if last.Outcome != policy.OutcomeRetainedPrevious || last.Cause != policy.CauseSignatureInvalid {
		t.Fatalf("result = %+v, want retained_previous / bundle_signature_invalid", last)
	}
	if raw, etag, _ := f.cache.load(); string(raw) != string(v5) || etag != `"5"` {
		t.Fatal("the refused bundle replaced the cached one")
	}
	if st := f.sync.Status(); st.LastAnswer != "error" || st.LastError == "" {
		t.Fatalf("status = %+v, want the refusal reported", st)
	}
}

// docs/02 §5.2: a 404 means the tenant has no bundle, and the device is at M0 — never "no policy,
// no restriction". The cache goes too, and the next fetch asks for a bundle rather than a 304.
func TestPolicySyncNoBundleFallsToM0(t *testing.T) {
	f := newSyncFixture(t)
	f.answer(fetchAnswer{fetch: drain.PolicyFetch{Status: drain.PolicyServed, Envelope: signedTestBundle(t, f.priv, "5", protocol.ModeM3), ETag: `"5"`}})
	f.answer(fetchAnswer{fetch: drain.PolicyFetch{Status: drain.PolicyNone}})
	f.answer(fetchAnswer{fetch: drain.PolicyFetch{Status: drain.PolicyNone}})
	f.sync.once(context.Background())
	f.sync.once(context.Background())
	if f.inForce(t) != "" {
		t.Fatalf("in force %q after the server stated there is no bundle, want none (M0)", f.inForce(t))
	}
	if f.results[len(f.results)-1].Outcome != policy.OutcomeFellToM0 {
		t.Fatalf("result = %+v, want fell_to_m0", f.results[len(f.results)-1])
	}
	if _, err := os.Stat(f.cache.bundlePath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the withdrawn bundle is still cached; a restart would enforce it")
	}
	f.sync.once(context.Background())
	if f.fetcher.etags[2] != "" {
		t.Fatalf("a fetch with nothing in force sent If-None-Match %q", f.fetcher.etags[2])
	}
}

// An outage, a refusal or a route the server does not serve changes nothing, and is retried soon
// rather than at the full interval.
func TestPolicySyncFetchErrorKeepsTheBundle(t *testing.T) {
	f := newSyncFixture(t)
	f.answer(fetchAnswer{fetch: drain.PolicyFetch{Status: drain.PolicyServed, Envelope: signedTestBundle(t, f.priv, "5", protocol.ModeM1), ETag: `"5"`}})
	f.answer(fetchAnswer{err: errors.New("drain: transport error")})
	f.answer(fetchAnswer{err: errors.New("drain: transport error")})
	f.answer(fetchAnswer{fetch: drain.PolicyFetch{NextPoll: 2 * time.Minute}, err: errors.New("drain: status 503")})
	f.sync.once(context.Background())
	if wait := f.sync.once(context.Background()); wait != policyRetryFloor {
		t.Fatalf("first retry in %s, want %s", wait, policyRetryFloor)
	}
	if wait := f.sync.once(context.Background()); wait != 2*policyRetryFloor {
		t.Fatalf("second retry in %s, want it doubled", wait)
	}
	if wait := f.sync.once(context.Background()); wait != 2*time.Minute {
		t.Fatalf("retry after a 503 in %s, want the server's Retry-After", wait)
	}
	if f.inForce(t) != "5" {
		t.Fatalf("in force %q after failed fetches, want 5", f.inForce(t))
	}
}

// The cached bundle is verified again at start, so a restart enforces the last verified bundle
// before the network answers, and a modified cache is refused like any other bad bundle.
func TestFetchedPolicyCacheIsVerifiedAtStart(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cfg := Config{DeviceEndpoint: "https://devices.example.com", CredentialFile: filepath.Join(dir, "credential.sealed"),
		PolicyKey: hex.EncodeToString(pub), PolicyKeyID: "policy-key-1"}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	refs := policy.ArtefactResolverFunc(func(policy.ArtefactRef) error { return nil })

	store, res, err := openFetchedPolicy(cfg, refs, log)
	if err != nil || store.InForce() != nil || res.Outcome != policy.OutcomeFellToM0 {
		t.Fatalf("no cache: store in force %v, result %+v, err %v; want M0", store.InForce(), res, err)
	}

	cache := policyCache{dir: filepath.Join(dir, policyCacheDir)}
	if err := cache.save(signedTestBundle(t, priv, "7", protocol.ModeM1), `"7"`); err != nil {
		t.Fatal(err)
	}
	store, res, err = openFetchedPolicy(cfg, refs, log)
	if err != nil || store.InForce() == nil || store.InForce().Version != "7" || res.Outcome != policy.OutcomeAccepted {
		t.Fatalf("cached bundle not in force at start: %+v %v", res, err)
	}

	raw, _, _ := cache.load()
	if err := cache.save([]byte(strings.Replace(string(raw), `"m1"`, `"m3"`, 1)), `"7"`); err != nil {
		t.Fatal(err)
	}
	store, res, err = openFetchedPolicy(cfg, refs, log)
	if err != nil || store.InForce() != nil || res.Cause != policy.CauseSignatureInvalid {
		t.Fatalf("a modified cache: in force %v, result %+v; want M0 and a signature cause", store.InForce(), res)
	}
}
