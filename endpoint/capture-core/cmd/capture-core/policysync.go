package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/drain"
	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/capture-core/state"
)

// The policy bundle the device fetches for itself: after enrolment it asks GET /v1/policy,
// verifies the answer under the pinned policy key, keeps the last verified bundle in the state
// directory so a restart enforces it before the network answers, and polls with If-None-Match at
// the server's cadence. Nothing here can widen what the device does: an unverified, older or
// unreadable bundle leaves the one in force where it is (M0 when there is none), and only the
// server's own statement that the tenant has no bundle withdraws one, which narrows to M0.

const (
	// defaultPolicyInterval is the poll cadence when the server states none. Polling is cheap (a 304
	// is a few hundred bytes), and a policy change should reach the fleet within a working session.
	defaultPolicyInterval = 15 * time.Minute
	minPolicyInterval     = time.Minute
	maxPolicyInterval     = 24 * time.Hour
	// policyRetryFloor is the first retry after a failed fetch; failures double it up to the
	// ordinary interval, so an outage is not met with a request storm.
	policyRetryFloor = time.Minute
)

// policyCache is the last verified bundle and its ETag. The bundle is stored as served; its
// integrity is the signature, verified again on every load, so a modified cache is refused like any
// other bad bundle rather than trusted because it is local.
type policyCache struct{ dir string }

func (c policyCache) bundlePath() string { return filepath.Join(c.dir, "bundle.json") }
func (c policyCache) etagPath() string   { return filepath.Join(c.dir, "bundle.etag") }

func (c policyCache) load() ([]byte, string, error) {
	raw, err := os.ReadFile(c.bundlePath())
	if err != nil {
		return nil, "", err
	}
	etag, _ := os.ReadFile(c.etagPath())
	return raw, strings.TrimSpace(string(etag)), nil
}

func (c policyCache) save(raw []byte, etag string) error {
	if err := os.MkdirAll(c.dir, 0o700); err != nil {
		return err
	}
	if err := state.WriteFile(c.bundlePath(), raw); err != nil {
		return err
	}
	if etag == "" {
		_ = os.Remove(c.etagPath())
		return nil
	}
	return state.WriteFile(c.etagPath(), []byte(etag))
}

func (c policyCache) saveETag(etag string) error {
	if etag == "" {
		return nil
	}
	return state.WriteFile(c.etagPath(), []byte(etag))
}

func (c policyCache) remove() {
	_ = os.Remove(c.bundlePath())
	_ = os.Remove(c.etagPath())
}

// policyFetcher is the drain's GET /v1/policy, behind a seam so the sync can be tested without a
// device cloud.
type policyFetcher interface {
	FetchPolicy(ctx context.Context, etag string) (drain.PolicyFetch, error)
}

// policySync is the poller. It owns no bundle: the policy.Store decides what is in force, and the
// service records the result and applies an accepted bundle to the providers.
type policySync struct {
	store    *policy.Store
	cache    policyCache
	fetcher  policyFetcher
	accepted func(policy.Result) // records every result; applies the bundle when one was accepted
	log      *slog.Logger
	clock    func() time.Time
	interval time.Duration

	mu       sync.Mutex
	etag     string
	failures int
	status   policySyncStatus

	stop chan struct{}
	wg   sync.WaitGroup
}

// policySyncStatus is the poller's state in the health document, so an owner can see that the
// device fetched, what it was told and when it asks next.
type policySyncStatus struct {
	LastFetchAt  *time.Time `json:"last_fetch_at,omitempty"`
	LastAnswer   string     `json:"last_answer,omitempty"` // served | not_modified | no_bundle | error
	LastError    string     `json:"last_error,omitempty"`
	ETag         string     `json:"etag,omitempty"`
	NextFetchAt  *time.Time `json:"next_fetch_at,omitempty"`
	CachedBundle string     `json:"cached_bundle,omitempty"`
}

func newPolicySync(store *policy.Store, cache policyCache, fetcher policyFetcher, accepted func(policy.Result), log *slog.Logger) *policySync {
	p := &policySync{store: store, cache: cache, fetcher: fetcher, accepted: accepted, log: log,
		clock: time.Now, interval: defaultPolicyInterval, stop: make(chan struct{})}
	if _, etag, err := cache.load(); err == nil {
		p.etag = etag
	}
	return p
}

// once performs one fetch and returns how long to wait before the next.
func (p *policySync) once(ctx context.Context) time.Duration {
	p.mu.Lock()
	etag := p.etag
	p.mu.Unlock()
	// A 304 is only an answer about the bundle in force. With none in force (a cache that failed
	// verification, or a withdrawn bundle) the device must be sent the bundle, not told it has it.
	if p.store.InForce() == nil {
		etag = ""
	}

	f, err := p.fetcher.FetchPolicy(ctx, etag)
	now := p.clock()
	if err != nil {
		p.mu.Lock()
		p.failures++
		wait := policyRetryFloor << min(p.failures-1, 10)
		if wait > p.interval {
			wait = p.interval
		}
		if f.NextPoll > 0 {
			wait = clampPolicyInterval(f.NextPoll)
		}
		p.record(now, "error", err, wait)
		p.mu.Unlock()
		p.log.Warn("policy: fetch failed; the bundle in force is unchanged", "error", err, "retry_in", wait)
		return wait
	}

	wait := p.interval
	if f.NextPoll > 0 {
		wait = clampPolicyInterval(f.NextPoll)
	}
	p.mu.Lock()
	p.failures = 0
	p.mu.Unlock()

	switch f.Status {
	case drain.PolicyNotModified:
		if f.ETag != "" && f.ETag != etag {
			_ = p.cache.saveETag(f.ETag)
			p.setETag(f.ETag)
		}
		p.recordLocked(now, "not_modified", nil, wait)

	case drain.PolicyNone:
		res := p.store.Withdraw()
		p.cache.remove()
		p.setETag("")
		p.accepted(res)
		p.recordLocked(now, "no_bundle", nil, wait)
		p.log.Warn("policy: the server states the tenant has no policy bundle; the device is at M0 (metadata only)")

	case drain.PolicyServed:
		res := p.store.Apply(f.Envelope)
		p.accepted(res)
		if res.Err != nil {
			// The bundle is discarded, the previous one stays in force (M0 with none), the cause is
			// reported, and repeated failures back the polling off.
			wait = p.store.PollBackoff(wait)
			p.recordLocked(now, "error", res.Err, wait)
			p.log.Warn("policy: fetched bundle refused; the bundle in force is unchanged",
				"cause", res.Cause, "outcome", res.Outcome, "severity", res.Severity, "error", res.Err)
			return wait
		}
		if err := p.cache.save(f.Envelope, f.ETag); err != nil {
			p.log.Warn("policy: could not cache the verified bundle; a restart will run at M0 until the next fetch", "error", err)
		}
		p.setETag(f.ETag)
		p.recordLocked(now, "served", nil, wait)
		if res.Outcome == policy.OutcomeAccepted {
			p.log.Info("policy: new bundle in force", "version", res.Version)
		}
	}
	return wait
}

func (p *policySync) setETag(etag string) {
	p.mu.Lock()
	p.etag = etag
	p.mu.Unlock()
}

func (p *policySync) recordLocked(now time.Time, answer string, err error, wait time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.record(now, answer, err, wait)
}

// record must be called with p.mu held.
func (p *policySync) record(now time.Time, answer string, err error, wait time.Duration) {
	next := now.Add(wait)
	p.status.LastFetchAt = &now
	p.status.LastAnswer = answer
	p.status.LastError = ""
	if err != nil {
		p.status.LastError = err.Error()
	}
	p.status.ETag = p.etag
	p.status.NextFetchAt = &next
}

// Status is the poller's state for the health document.
func (p *policySync) Status() policySyncStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.status
	if _, err := os.Stat(p.cache.bundlePath()); err == nil {
		st.CachedBundle = p.cache.bundlePath()
	}
	return st
}

// Start polls until Stop. The first fetch waits for first: the service fetches once synchronously
// right after enrolment, so the loop begins one interval later.
func (p *policySync) Start(ctx context.Context, first time.Duration) {
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		wait := first
		for {
			t := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-p.stop:
				t.Stop()
				return
			case <-t.C:
			}
			wait = p.once(ctx)
		}
	}()
}

// Stop ends the loop and waits for it.
func (p *policySync) Stop() {
	select {
	case <-p.stop:
		return
	default:
	}
	close(p.stop)
	p.wg.Wait()
}

func clampPolicyInterval(d time.Duration) time.Duration {
	switch {
	case d < minPolicyInterval:
		return minPolicyInterval
	case d > maxPolicyInterval:
		return maxPolicyInterval
	default:
		return d
	}
}
