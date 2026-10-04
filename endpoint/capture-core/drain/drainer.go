// Package drain is the capture-core device-to-cloud drain: it reads the spool oldest-first, batches
// observations into POST /v1/events requests over the configured HTTPS transport (mTLS from the
// issued x509 leaf, or DPoP-signed requests), and settles each record from the per-event outcome.
//
// It holds no policy and no counter of its own: the spool's Stats (depth, dropped, rejected,
// delivered) are the accounting, and the drainer's own state is surfaced as a degraded/healthy
// status with a closed protocol.Detail error code for the health channel (C23/C25).
package drain

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/credential"
	"github.com/shadow-ai-capture/device/protocol"
)

// Logger is the narrow logging seam the drainer uses for the facts that must not be lost: an
// enrolment failure, a terminal rejection, and a batch that could not be sent.
type Logger interface {
	Printf(format string, args ...any)
}

type nopLogger struct{}

func (nopLogger) Printf(string, ...any) {}

// StoreFunc supplies the spool at drain time. The spool is opened by the supervisor after the
// drainer is constructed, so the drainer resolves it lazily.
type StoreFunc func() (protocol.Store, error)

// Config is everything the drainer needs. It is resolved from capture-core's flags; the endpoint is
// deliberately NOT part of the policy bundle, because where a device sends is operator configuration
// (an MDM profile fact), not a signed per-tenant policy fact.
type Config struct {
	Endpoint       string
	AuthMode       protocol.AuthMode
	EnrolmentToken string
	CAFile         string

	TenantID     string
	DeviceID     string
	MDMID        string
	AgentVersion string

	BackoffBase time.Duration
	BackoffCap  time.Duration

	// DrainInterval is how long a single background pass may run, and how long the loop sleeps when
	// the spool is empty. Zero defaults to a short interval.
	DrainInterval time.Duration

	// Expire, when non-nil, is called before each pass to drop records past their device retention
	// deadline through the concrete spool's Expire (which is not part of protocol.Store).
	Expire func(now time.Time) (int, error)
}

// Result is what one bounded drain pass achieved.
type Result struct {
	Delivered int
	Rejected  int
	// Empty reports that the spool held no pending records at the end of the pass.
	Empty bool
}

// Status is the drainer's health surface: the same state + error_code vocabulary the coverage rows
// use, so a failing drain is visible rather than silent (C23/C25).
type Status struct {
	State       protocol.CollectorState
	Detail      protocol.Detail
	LastSuccess time.Time
	Endpoint    string
	Enrolled    bool
}

// Drainer is the background device-to-cloud drain.
type Drainer struct {
	cfg     Config
	store   StoreFunc
	creds   *credential.Store
	client  *client
	log     Logger
	clock   func() time.Time
	backoff Backoff

	mu          sync.Mutex
	cred        *credential.Credential
	key         *ecdsa.PrivateKey
	token       string
	tokenExp    time.Time
	enrolled    bool
	state       protocol.CollectorState
	detail      protocol.Detail
	lastSuccess time.Time

	stopCh  chan struct{}
	wg      sync.WaitGroup
	started bool
}

// New builds a drainer without starting anything. The credential store may already hold an issued
// credential, which New loads eagerly; an absent credential is left for Start's loop to obtain via
// enrolment.
func New(cfg Config, store StoreFunc, creds *credential.Store, log Logger, clock func() time.Time) (*Drainer, error) {
	if cfg.Endpoint == "" {
		return nil, errors.New("drain: endpoint is required")
	}
	if !cfg.AuthMode.Valid() {
		return nil, fmt.Errorf("drain: auth mode %q outside the closed set {x509,dpop}", cfg.AuthMode)
	}
	if log == nil {
		log = nopLogger{}
	}
	if clock == nil {
		clock = time.Now
	}
	if cfg.DrainInterval <= 0 {
		cfg.DrainInterval = time.Second
	}
	c, err := newClient(cfg.Endpoint, cfg.CAFile)
	if err != nil {
		return nil, err
	}
	d := &Drainer{
		cfg:     cfg,
		store:   store,
		creds:   creds,
		client:  c,
		log:     log,
		clock:   clock,
		backoff: Backoff{Base: cfg.BackoffBase, Cap: cfg.BackoffCap},
		// A wired drainer starts degraded, never absent: `absent` claims there is no coverage
		// when the drain is in fact configured and simply not yet proven to work (C25). The
		// first success flips it to healthy.
		state:  protocol.StateDegraded,
		detail: protocol.DetailUpstreamUnreachable,
		stopCh: make(chan struct{}),
	}
	if creds != nil {
		if cred, err := creds.Load(); err == nil {
			_ = d.setCredential(cred)
		}
	}
	return d, nil
}

// Start launches the background drain loop. It never fails startup: credential acquisition is the
// loop's job, and its failures surface as a degraded status rather than an error.
func (d *Drainer) Start(ctx context.Context) error {
	d.mu.Lock()
	if d.started {
		d.mu.Unlock()
		return nil
	}
	d.started = true
	d.mu.Unlock()
	d.wg.Add(1)
	go d.run(ctx)
	return nil
}

// Stop ends the background loop and waits for it. The final bounded drain happens separately, on the
// supervisor's shutdown path, which calls Drain.
func (d *Drainer) Stop(ctx context.Context) error {
	d.mu.Lock()
	if !d.started {
		d.mu.Unlock()
		return nil
	}
	d.started = false
	d.mu.Unlock()
	close(d.stopCh)
	d.wg.Wait()
	return nil
}

func (d *Drainer) run(ctx context.Context) {
	defer d.wg.Done()
	failures := 0
	for {
		select {
		case <-d.stopCh:
			return
		case <-ctx.Done():
			return
		default:
		}
		if !d.ready(ctx) {
			d.sleep(ctx, d.backoff.Delay(failures))
			failures++
			continue
		}
		failures = 0
		// Pace one full interval between passes, so the drain never races the collectors still
		// filling the spool, and a shut-down request can still interrupt the wait. The shutdown
		// drain (Drain on the supervisor's path) is what flushes deterministically.
		if !d.sleepUntil(ctx, d.clock().Add(d.cfg.DrainInterval), d.cfg.DrainInterval) {
			return
		}
		if _, err := d.Drain(ctx, d.clock().Add(d.cfg.DrainInterval)); err != nil {
			d.log.Printf("drain: pass failed: %v", err)
		}
	}
}

// ready reports whether the drainer holds a usable credential, enrolling first when it does not.
// Enrolment failure is recorded as a degraded status and retried with backoff by the loop.
func (d *Drainer) ready(ctx context.Context) bool {
	d.mu.Lock()
	enrolled := d.enrolled
	current := d.cred
	d.mu.Unlock()

	expired := d.credentialExpired(current)
	if enrolled && !expired {
		return true
	}
	if d.cfg.EnrolmentToken == "" {
		detail := protocol.DetailUpstreamUnreachable
		if expired {
			// An expired leaf cannot authenticate and cannot be renewed without a token: say so
			// rather than retrying a request that will 401 forever.
			detail = protocol.DetailCredentialExpired
		}
		d.setStatus(protocol.StateDegraded, detail)
		d.log.Printf("drain: no usable credential and no enrolment token; the device cannot reach the ingest path")
		return false
	}
	if expired {
		d.log.Printf("drain: x509 credential expired (NotAfter %s); re-enrolling", current.NotAfter)
	}
	hwid := HardwareIdentityHash(d.cfg.TenantID, d.cfg.DeviceID, d.cfg.MDMID)
	// A re-enrolment of an expired credential reuses the hardware-identity hash it was first
	// issued under, so the edge returns the existing device_id instead of minting a duplicate.
	if expired && current != nil && current.HardwareIdentityHash != "" {
		hwid = current.HardwareIdentityHash
	}
	cred, err := d.enrol(ctx, hwid)
	if err != nil {
		d.setStatus(protocol.StateDegraded, detailForErr(err))
		d.log.Printf("drain: enrolment failed: %v", err)
		return false
	}
	if d.creds == nil {
		d.setStatus(protocol.StateDegraded, protocol.DetailUpstreamFailure)
		d.log.Printf("drain: no credential store to persist the issued credential")
		return false
	}
	if err := d.creds.Save(cred); err != nil {
		d.setStatus(protocol.StateDegraded, protocol.DetailUpstreamFailure)
		d.log.Printf("drain: could not store the issued credential: %v", err)
		return false
	}
	if err := d.setCredential(cred); err != nil {
		d.setStatus(protocol.StateDegraded, protocol.DetailUpstreamFailure)
		d.log.Printf("drain: issued credential is unusable: %v", err)
		return false
	}
	d.log.Printf("drain: enrolled device %s (mode %s)", cred.DeviceID, cred.Mode)
	return true
}

func (d *Drainer) setCredential(c *credential.Credential) error {
	key, err := c.ECPrivateKey()
	if err != nil {
		return err
	}
	d.mu.Lock()
	d.cred = c
	d.key = key
	d.enrolled = true
	d.mu.Unlock()
	return nil
}

// credentialExpired reports whether an x509 credential has passed its NotAfter. A DPoP
// credential carries no NotAfter (its key does not expire; only the short-lived access token
// does), so it never expires here. A zero NotAfter is treated as "no expiry" rather than as
// "expired at the epoch".
func (d *Drainer) credentialExpired(c *credential.Credential) bool {
	if c == nil || c.Mode != protocol.AuthModeX509 {
		return false
	}
	if c.NotAfter.IsZero() {
		return false
	}
	return !d.clock().Before(c.NotAfter)
}

// Drain runs one bounded pass: peek oldest-first, reject over-cap envelopes, batch, send (retrying
// with backoff while the deadline holds), and settle each record from its outcome.
func (d *Drainer) Drain(ctx context.Context, deadline time.Time) (Result, error) {
	var res Result
	// A drain that sends nothing is otherwise silent: a service has no console, and --native-frames
	// prints only the frames. One line per pass says what left the spool and why it stopped.
	defer func() {
		d.mu.Lock()
		state, detail := d.state, d.detail
		d.mu.Unlock()
		d.log.Printf("drain: pass delivered=%d rejected=%d empty=%v state=%s detail=%s", res.Delivered, res.Rejected, res.Empty, state, detail)
	}()
	for {
		if !d.clock().Before(deadline) {
			break
		}
		d.mu.Lock()
		cred := d.cred
		key := d.key
		d.mu.Unlock()
		if cred == nil {
			d.log.Printf("drain: not enrolled; the spool is retained and nothing is sent")
			return res, errors.New("drain: not enrolled")
		}

		store, err := d.store()
		if err != nil {
			return res, err
		}
		if d.cfg.Expire != nil {
			if n, err := d.cfg.Expire(d.clock()); err != nil {
				d.log.Printf("drain: retention sweep failed: %v", err)
			} else if n > 0 {
				d.log.Printf("drain: retention dropped %d expired record(s) (occurred_at + retention is in the past)", n)
			}
		}

		entries, err := store.Peek(protocol.MaxBatchEvents)
		if err != nil {
			return res, err
		}
		if len(entries) == 0 {
			res.Empty = true
			return res, nil
		}

		var sendable []protocol.Entry
		for _, e := range entries {
			if len(e.Payload) > protocol.MaxEnvelopeBytes {
				if err := settleOversize(store, e); err != nil {
					d.log.Printf("drain: rejecting oversize entry %d: %v", e.Seq, err)
				} else {
					res.Rejected++
				}
			} else {
				sendable = append(sendable, e)
			}
		}
		if len(sendable) == 0 {
			continue
		}

		bb, err := buildBatch(sendable, d.clock())
		if err != nil {
			return res, err
		}
		if err := store.MarkInFlight(bb.seqs); err != nil {
			return res, err
		}

		var token string
		if d.cfg.AuthMode == protocol.AuthModeDPoP {
			if err := d.ensureToken(ctx); err != nil {
				d.release(store, bb.seqs, "token")
				d.setStatus(protocol.StateDegraded, detailForErr(err))
				d.log.Printf("drain: token acquisition failed: %v", err)
				return res, nil
			}
			d.mu.Lock()
			token = d.token
			d.mu.Unlock()
		}

		attempt := 0
		for {
			resp, err := d.sendBatch(ctx, bb, cred, key, token)
			if err == nil {
				d.settle(store, bb, resp, &res)
				d.markSuccess()
				break
			}
			ae, ok := err.(*apiError)
			if !ok {
				// A non-*apiError from sendBatch is not a classified §7 rejection: a 200 whose
				// body will not parse or validate, or a local credential/proof failure. It must
				// not be dereferenced as an *apiError; release the in-flight batch, surface the
				// failure, and keep draining.
				d.release(store, bb.seqs, string(protocol.DetailUpstreamFailure))
				d.setStatus(protocol.StateDegraded, protocol.DetailUpstreamFailure)
				d.log.Printf("drain: sending batch failed: %v", err)
				if !d.sleepUntil(ctx, deadline, d.backoff.Delay(attempt)) {
					return res, nil // deadline or stop: records are released, retried next open
				}
				attempt++
				continue
			}
			if ae.Retryable() {
				if ae.code == protocol.ReasonDuplicateBatch {
					newBB, berr := buildBatch(bb.entries, d.clock())
					if berr != nil {
						d.release(store, bb.seqs, string(ae.code))
						return res, berr
					}
					bb = newBB
				}
				if !d.sleepUntil(ctx, deadline, d.backoff.Delay(attempt)) {
					return res, nil // deadline or stop: records stay in-flight, retried next open
				}
				attempt++
				continue
			}
			// Terminal: retain the spool, report the cause, and stop sending (§13).
			d.release(store, bb.seqs, string(ae.code))
			d.setStatus(protocol.StateDegraded, detailFor(ae))
			d.log.Printf("drain: terminal failure, retaining spool: status %d code %q", ae.status, ae.code)
			return res, nil
		}
	}
	return res, nil
}

func (d *Drainer) ensureToken(ctx context.Context) error {
	d.mu.Lock()
	token := d.token
	exp := d.tokenExp
	cred := d.cred
	key := d.key
	d.mu.Unlock()
	if token != "" && d.clock().Before(exp.Add(-30*time.Second)) {
		return nil
	}
	tok, exp, err := d.fetchToken(ctx, key, cred.DeviceID, cred.TenantID)
	if err != nil {
		return err
	}
	d.mu.Lock()
	d.token = tok
	d.tokenExp = exp
	d.mu.Unlock()
	return nil
}

// settle maps each per-event outcome onto the spool state via protocol.Outcome.SettleState, which is
// the single place that mapping lives.
func (d *Drainer) settle(store protocol.Store, bb *builtBatch, resp *protocol.EventBatchResponse, res *Result) {
	for i, r := range resp.Results {
		seq := bb.seqs[i]
		state, _ := r.Outcome.SettleState(r.Reason)
		switch state {
		case protocol.SpoolDelivered:
			if err := store.Settle(seq, protocol.SpoolDelivered, ""); err == nil {
				res.Delivered++
			}
		case protocol.SpoolRejected:
			if err := store.Settle(seq, protocol.SpoolRejected, string(r.Reason)); err == nil {
				res.Rejected++
			}
		default:
			_ = store.Settle(seq, protocol.SpoolPending, string(r.Reason))
		}
	}
}

// release returns in-flight records to pending with a last error, so a terminal failure retains the
// spool rather than discarding it (§13: the data is not discarded because the device may re-enrol).
func (d *Drainer) release(store protocol.Store, seqs []uint64, reason string) {
	for _, seq := range seqs {
		_ = store.Settle(seq, protocol.SpoolPending, reason)
	}
}

func (d *Drainer) markSuccess() {
	d.mu.Lock()
	d.lastSuccess = d.clock()
	d.state = protocol.StateHealthy
	d.detail = protocol.DetailNone
	d.mu.Unlock()
}

func (d *Drainer) setStatus(state protocol.CollectorState, detail protocol.Detail) {
	d.mu.Lock()
	d.state = state
	d.detail = detail
	d.mu.Unlock()
}

// Status is the drainer's current health, for the health snapshot.
func (d *Drainer) Status() Status {
	d.mu.Lock()
	defer d.mu.Unlock()
	return Status{
		State:       d.state,
		Detail:      d.detail,
		LastSuccess: d.lastSuccess,
		Endpoint:    d.cfg.Endpoint,
		Enrolled:    d.enrolled,
	}
}

func (d *Drainer) sleepUntil(ctx context.Context, deadline time.Time, dur time.Duration) bool {
	if rem := deadline.Sub(d.clock()); dur > rem {
		dur = rem
	}
	if dur <= 0 {
		return false
	}
	t := time.NewTimer(dur)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-d.stopCh:
		return false
	case <-t.C:
		return true
	}
}

func (d *Drainer) sleep(ctx context.Context, dur time.Duration) {
	if dur <= 0 {
		dur = time.Second
	}
	t := time.NewTimer(dur)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-d.stopCh:
	case <-t.C:
	}
}

// detailFor maps a classified failure onto the closed error-code vocabulary. Reused values, not new
// ones: an unreachable endpoint is upstream_unreachable, any endpoint refusal is upstream_failure.
func detailFor(ae *apiError) protocol.Detail {
	if ae == nil || ae.transport {
		return protocol.DetailUpstreamUnreachable
	}
	return protocol.DetailUpstreamFailure
}

func detailForErr(err error) protocol.Detail {
	var ae *apiError
	if errors.As(err, &ae) {
		return detailFor(ae)
	}
	return protocol.DetailUpstreamFailure
}

// HardwareIdentityHash derives the per-tenant enrolment idempotency key (C11). The preferred seed is
// the MDM-delivered device identifier (mdmID); when none is supplied it falls back to hashing the
// tenant and device identity, which is stable across re-enrolment of the same configured device but
// is NOT a hardware binding.
//
// ASSUMPTION: the fallback does not survive a re-image that assigns a new device identity, so an MDM
// id should be supplied wherever the platform delivers one.
func HardwareIdentityHash(tenantID, deviceID, mdmID string) string {
	seed := mdmID
	if seed == "" {
		seed = "device:" + deviceID
	}
	sum := sha256.Sum256([]byte("sac-hwid\x1f" + tenantID + "\x1f" + seed))
	return "sha256:" + hex.EncodeToString(sum[:])
}
