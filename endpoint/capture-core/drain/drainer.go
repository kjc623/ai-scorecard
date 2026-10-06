// Package drain is the device-to-cloud path: it enrols the device and rotates its certificate,
// reads the spool oldest-first, batches observations into POST /v1/events over mutual TLS, settles
// each record from its per-event outcome, uploads granted M3 content, fetches the signed policy
// bundle and sends the health report.
//
// It holds no policy and no counter of its own: the spool's stats are the accounting, and the
// drainer's own state is a healthy/degraded status with a closed protocol.Detail cause.
package drain

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"sync"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/credential"
	"github.com/shadow-ai-capture/device/protocol"
)

// Logger is the narrow logging seam the drainer uses.
type Logger interface {
	Printf(format string, args ...any)
}

type nopLogger struct{}

func (nopLogger) Printf(string, ...any) {}

// reasonStaleIdentity is the settle reason for a record minted under a different identity than
// the current credential. The write path would reject it, and its identity-derived dedup key
// cannot be recomputed, so it is settled as rejected with a visible reason.
const reasonStaleIdentity = "stale_identity"

// StoreFunc supplies the spool at drain time. The spool is opened by the supervisor after the
// drainer is constructed, so the drainer resolves it lazily.
type StoreFunc func() (protocol.Store, error)

// Config is everything the drainer needs.
type Config struct {
	// Endpoint is the device edge's base URL (https, no path).
	Endpoint string
	// DeploymentKey is the tenant's deployment key from the MDM-delivered tenant file. It enrols
	// the device the first time, and again if the certificate ever expires.
	DeploymentKey string
	// CAFile names a PEM CA set trusted in addition to the system roots.
	CAFile string
	// TenantID is the configured tenant. The issued credential is authoritative; a disagreement
	// is logged.
	TenantID string
	// HardwareSeed is what the operating system states about the hardware: the enrolment
	// idempotency seed, so a re-imaged device gets its existing device_id back.
	HardwareSeed string
	// Attestation, when non-nil, is called at each enrolment for what the device can say about its
	// own management; the server checks it against the customer's MDM.
	Attestation  func() *protocol.DeviceAttestation
	AgentVersion string
	// Hostname is sent at enrolment while the tenant's device identity setting is clear;
	// HostnameHash replaces it when the setting is hashed. ManagedState is the agent's report.
	Hostname     string
	HostnameHash string
	ManagedState string

	BackoffBase time.Duration
	BackoffCap  time.Duration

	// DrainInterval is how long one background pass may run, and how long the loop waits between
	// passes.
	DrainInterval time.Duration

	// Expire, when non-nil, is called before each pass to drop spooled records past their retention
	// deadline.
	Expire func(now time.Time) (int, error)

	// OnEnrolled, when non-nil, is called each time the drainer adopts a credential (loaded at
	// construction, first enrolment, rotation), so the caller can adopt the issued identity.
	OnEnrolled func(*credential.Credential)

	// Content, when non-nil, is the M3 content store: delivered M3 events become grant requests,
	// and granted content is uploaded.
	Content ContentSource
}

// Result is what one bounded drain pass achieved.
type Result struct {
	Delivered int
	Rejected  int
	// Empty reports that the spool held no pending records at the end of the pass.
	Empty bool
}

// Status is the drainer's health surface.
type Status struct {
	State       protocol.CollectorState
	Detail      protocol.Detail
	LastSuccess time.Time
	Endpoint    string
	Enrolled    bool
	NotAfter    time.Time
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

	enrolMu sync.Mutex // one enrolment or rotation at a time

	mu          sync.Mutex
	cred        *credential.Credential
	state       protocol.CollectorState
	detail      protocol.Detail
	lastSuccess time.Time

	stopCh  chan struct{}
	wg      sync.WaitGroup
	started bool
}

// New builds a drainer without starting anything. A credential already in the store is loaded
// now; an absent one is obtained by enrolment.
func New(cfg Config, store StoreFunc, creds *credential.Store, log Logger, clock func() time.Time) (*Drainer, error) {
	if cfg.Endpoint == "" {
		return nil, errors.New("drain: endpoint is required")
	}
	if creds == nil {
		return nil, errors.New("drain: a credential store is required")
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
		// A configured drain starts degraded, never absent: absent would claim there is no path
		// when there is one not yet proven to work. The first success makes it healthy.
		state:  protocol.StateDegraded,
		detail: protocol.DetailUpstreamUnreachable,
		stopCh: make(chan struct{}),
	}
	cred, err := creds.Load()
	switch {
	case err == nil:
		if err := d.setCredential(cred); err != nil {
			log.Printf("drain: the stored credential is unusable and will be replaced by enrolment: %v", err)
		}
	case !errors.Is(err, fs.ErrNotExist):
		log.Printf("drain: the stored credential cannot be read and will be replaced by enrolment: %v", err)
	}
	return d, nil
}

// Start launches the background drain loop. Credential acquisition is the loop's job, and its
// failures surface as a degraded status rather than an error.
func (d *Drainer) Start(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.started {
		return nil
	}
	d.started = true
	d.wg.Add(1)
	go d.run(ctx)
	return nil
}

// Stop ends the background loop and waits for it. The final bounded drain happens separately, on
// the supervisor's shutdown path, which calls Drain.
func (d *Drainer) Stop(context.Context) error {
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
		// One full interval between passes, so the drain never races the collectors filling the
		// spool and a stop can interrupt the wait.
		if !d.sleepUntil(ctx, d.clock().Add(d.cfg.DrainInterval), d.cfg.DrainInterval) {
			return
		}
		if _, err := d.Drain(ctx, d.clock().Add(d.cfg.DrainInterval)); err != nil {
			d.log.Printf("drain: pass failed: %v", err)
		}
	}
}

// EnsureEnrolled runs the credential path synchronously (rotate or enrol as needed) and reports
// whether the drainer holds a usable credential. The service calls it before providers start.
func (d *Drainer) EnsureEnrolled(ctx context.Context) bool { return d.ready(ctx) }

// ready reports whether the drainer holds a usable credential, obtaining one first when needed:
//
//   - a credential past its renewal point but not expired is rotated, authenticated by itself; a
//     failed rotation keeps the current credential and retries later;
//   - with no credential, or an expired one, the device enrols with the deployment key, reusing
//     the hardware identity it was first issued under so it keeps its device_id.
func (d *Drainer) ready(ctx context.Context) bool {
	d.enrolMu.Lock()
	defer d.enrolMu.Unlock()
	d.mu.Lock()
	current := d.cred
	d.mu.Unlock()
	now := d.clock()

	if current != nil && !current.Expired(now) {
		if now.Before(current.RenewAt()) {
			return true
		}
		if err := d.obtain(ctx, current.HardwareIdentityHash, true); err != nil {
			d.log.Printf("drain: certificate rotation failed; the current certificate stays in use until %s: %v", current.NotAfter.Format(time.RFC3339), err)
		}
		return true
	}
	if d.cfg.DeploymentKey == "" {
		detail := protocol.DetailUpstreamUnreachable
		if current != nil {
			detail = protocol.DetailCredentialExpired
		}
		d.setStatus(protocol.StateDegraded, detail)
		d.log.Printf("drain: no usable credential and no deployment key; the device cannot enrol")
		return false
	}
	hwid := HardwareIdentityHash(d.cfg.TenantID, d.cfg.HardwareSeed)
	if current != nil {
		d.log.Printf("drain: the certificate expired at %s; enrolling again with the deployment key", current.NotAfter.Format(time.RFC3339))
		if current.HardwareIdentityHash != "" {
			hwid = current.HardwareIdentityHash
		}
	}
	if err := d.obtain(ctx, hwid, false); err != nil {
		d.setStatus(protocol.StateDegraded, detailForErr(err))
		d.log.Printf("drain: enrolment failed: %v", err)
		return false
	}
	return true
}

// obtain enrols (or rotates), stores the issued credential and adopts it.
func (d *Drainer) obtain(ctx context.Context, hwid string, rotation bool) error {
	cred, err := d.enrol(ctx, hwid, rotation)
	if err != nil {
		return err
	}
	if err := d.creds.Save(cred); err != nil {
		return fmt.Errorf("storing the issued credential: %w", err)
	}
	if err := d.setCredential(cred); err != nil {
		return err
	}
	verb := "enrolled"
	if rotation {
		verb = "rotated the certificate of"
	}
	d.log.Printf("drain: %s device %s (certificate valid until %s)", verb, cred.DeviceID, cred.NotAfter.Format(time.RFC3339))
	return nil
}

func (d *Drainer) setCredential(c *credential.Credential) error {
	pair, err := c.KeyPair()
	if err != nil {
		return err
	}
	d.client.setCertificate(&pair)
	d.mu.Lock()
	d.cred = c
	d.mu.Unlock()
	if d.cfg.TenantID != "" && c.TenantID != d.cfg.TenantID {
		d.log.Printf("drain: the issued tenant %q differs from the configured tenant %q; the issued value is used", c.TenantID, d.cfg.TenantID)
	}
	if d.cfg.OnEnrolled != nil {
		d.cfg.OnEnrolled(c)
	}
	return nil
}

// credentialNow returns the credential in use.
func (d *Drainer) credentialNow() *credential.Credential {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.cred
}

// Drain runs one bounded pass: peek oldest-first, reject over-cap envelopes, batch, send
// (retrying with backoff while the deadline holds) and settle each record from its outcome.
func (d *Drainer) Drain(ctx context.Context, deadline time.Time) (Result, error) {
	var res Result
	defer func() {
		if res.Delivered > 0 || res.Rejected > 0 {
			d.log.Printf("drain: pass delivered=%d rejected=%d empty=%v", res.Delivered, res.Rejected, res.Empty)
		}
	}()
	for d.clock().Before(deadline) {
		cred := d.credentialNow()
		if cred == nil {
			return res, errors.New("drain: not enrolled; the spool is retained")
		}
		store, err := d.store()
		if err != nil {
			return res, err
		}
		if d.cfg.Expire != nil {
			if n, err := d.cfg.Expire(d.clock()); err != nil {
				d.log.Printf("drain: retention sweep failed: %v", err)
			} else if n > 0 {
				d.log.Printf("drain: retention dropped %d expired record(s)", n)
			}
		}

		// Content rides the same pass, after the events delivered by the previous iteration.
		d.contentPass(ctx, deadline)

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
				if err := store.Settle(e.Seq, protocol.SpoolRejected, string(protocol.ReasonOversize)); err == nil {
					res.Rejected++
				}
				continue
			}
			if tenantID, deviceID := envelopeIdentity(e.Payload); tenantID != cred.TenantID || deviceID != cred.DeviceID {
				if err := store.Settle(e.Seq, protocol.SpoolRejected, reasonStaleIdentity); err == nil {
					res.Rejected++
				}
				continue
			}
			sendable = append(sendable, e)
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
		for attempt := 0; ; attempt++ {
			resp, err := d.sendBatch(ctx, bb)
			if err == nil {
				d.settle(store, bb, resp, &res)
				d.markSuccess()
				break
			}
			ae, classified := isAPIError(err)
			if classified && !ae.Retryable() {
				// A refusal of the whole batch: retain the spool, report the cause, stop sending.
				d.release(store, bb.seqs, string(ae.code))
				d.setStatus(protocol.StateDegraded, protocol.DetailUpstreamFailure)
				d.log.Printf("drain: the edge refused the batch, retaining the spool: %v", err)
				return res, nil
			}
			// A retryable failure, or a 200 whose body would not parse or validate.
			d.setStatus(protocol.StateDegraded, detailForErr(err))
			if !d.sleepUntil(ctx, deadline, d.retryDelay(ae, attempt)) {
				d.release(store, bb.seqs, err.Error())
				return res, nil
			}
		}
	}
	return res, nil
}

// retryDelay is the backoff for attempt, or the server's stated delay when it gave one.
func (d *Drainer) retryDelay(ae *apiError, attempt int) time.Duration {
	if ae != nil && ae.retryAfterS > 0 {
		return time.Duration(ae.retryAfterS) * time.Second
	}
	return d.backoff.Delay(attempt)
}

// settle maps each per-event outcome onto the spool state through protocol.Outcome.SettleState,
// the single place that mapping lives.
func (d *Drainer) settle(store protocol.Store, bb *builtBatch, resp *protocol.EventBatchResponse, res *Result) {
	for i, r := range resp.Results {
		seq := bb.seqs[i]
		state, _ := r.Outcome.SettleState()
		switch state {
		case protocol.SpoolDelivered:
			if err := store.Settle(seq, protocol.SpoolDelivered, ""); err == nil {
				res.Delivered++
				d.noteDelivered(bb.entries[i].Payload)
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

// release returns in-flight records to pending, so a failure retains the spool rather than
// discarding it.
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
	st := Status{State: d.state, Detail: d.detail, LastSuccess: d.lastSuccess, Endpoint: d.cfg.Endpoint, Enrolled: d.cred != nil}
	if d.cred != nil {
		st.NotAfter = d.cred.NotAfter
	}
	return st
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

// detailForErr maps a failure onto the closed cause vocabulary: an unreachable edge is
// upstream_unreachable, any refusal or unusable answer is upstream_failure.
func detailForErr(err error) protocol.Detail {
	if ae, ok := isAPIError(err); ok && ae.transport {
		return protocol.DetailUpstreamUnreachable
	}
	return protocol.DetailUpstreamFailure
}

// HardwareIdentityHash derives the per-tenant enrolment idempotency key from the hardware seed
// the operating system states, so a re-imaged device enrols back into its existing device_id.
func HardwareIdentityHash(tenantID, seed string) string {
	sum := sha256.Sum256([]byte("sac-hwid\x1f" + tenantID + "\x1f" + seed))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// newID mints a UUIDv4-shaped identifier.
func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
