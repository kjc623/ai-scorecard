package providers

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// Store is the database the runner needs: the connections, their state and the tenant's policy,
// and what recording needs.
type Store interface {
	RecordStore
	// Targets returns every active connection of every tenant whose ingest is open.
	Targets(ctx context.Context) ([]Target, error)
	// Target returns one connection; ok is false when it is not active or its tenant's ingest is
	// closed.
	Target(ctx context.Context, tenantID, connectionID string) (Target, bool, error)
	// Lock holds the connection's pull for this process; ok is false when another process holds it.
	Lock(ctx context.Context, connectionID string) (unlock func(), ok bool, err error)
	Started(ctx context.Context, tenantID, connectionID string, at time.Time) error
	// Finished records how a pass ended and, when r.Cursor is set, where the next one resumes.
	Finished(ctx context.Context, tenantID, connectionID string, at time.Time, r Result) error
	// Policy reads the tenant's settings a pass records under.
	Policy(ctx context.Context, tenantID string) (Policy, error)
}

// Target is one active connection as the store holds it: its key still sealed, and its schedule.
type Target struct {
	Connection
	SealedKey []byte
	// LastStartedAt is when the connection's last pass started; zero before its first.
	LastStartedAt time.Time
	// PullEveryMinutes is how often the connection is pulled, one of PullEvery.
	PullEveryMinutes int
}

// Due reports whether the connection's scheduled pass is due at now.
func (t Target) Due(now time.Time) bool {
	next := NextPull(t.LastStartedAt, t.PullEveryMinutes)
	return next.IsZero() || !now.Before(next)
}

// Opener opens a key sealed under the tenant's directory key. *directory.Cipher implements it.
type Opener interface {
	Open(tenantID string, sealed []byte) (string, error)
}

// UserRefKeys yields a tenant's user-reference key. *directory.UserRefKeys implements it.
type UserRefKeys interface {
	Key(ctx context.Context, tenantID string) ([]byte, error)
}

// Result is one pass over one connection.
type Result struct {
	// Read counts the records the provider returned; New those that made a submission the tenant
	// did not hold; Skipped those that could not be attributed to a person or carried nothing.
	Read    int
	New     int
	Skipped int
	// Cursor is where the next pass resumes; empty when the pass reached no new point.
	Cursor string
	// Error is a closed code, empty when the pass succeeded.
	Error string
}

// PullEvery is every schedule an admin can choose for a connection, in minutes, shortest first. The
// check on ops.provider_connection.pull_every_minutes admits exactly these.
var PullEvery = []int{15, 30, 60, 360, 1440}

// DefaultPullEvery is a connection's schedule, in minutes, until its admin chooses another.
const DefaultPullEvery = 60

// NextPull is when a connection whose last pass started at lastStarted is next due, pulled every
// minutes: zero when it has never been pulled, which makes it due at once.
func NextPull(lastStarted time.Time, minutes int) time.Time {
	if lastStarted.IsZero() {
		return time.Time{}
	}
	if minutes <= 0 {
		minutes = DefaultPullEvery
	}
	return lastStarted.Add(time.Duration(minutes) * time.Minute)
}

// checkEvery is how often the runner looks for the connections whose pass is due.
const checkEvery = time.Minute

// maxRounds bounds how many times one pass goes back to a provider that has more records, so a
// backlog is caught up in a few passes rather than held in one.
const maxRounds = 24

// Runner pulls every active connection on its schedule and on demand. The schedule is the
// database's, not the process's: a connection is due its schedule after its last pass started,
// whichever replica ran it, so a restart or a second replica does not pull it more often.
type Runner struct {
	// Sources is the adapter of each provider this deployment reads.
	Sources     map[string]Source
	Store       Store
	Recorder    *Recorder
	Keys        Opener
	UserRefKeys UserRefKeys
	Logger      *slog.Logger
	Now         func() time.Time

	kick chan kick
}

type kick struct{ tenantID, connectionID string }

// NewRunner wires the scheduler.
func NewRunner(sources map[string]Source, st Store, rec *Recorder, keys Opener, refs UserRefKeys, logger *slog.Logger) (*Runner, error) {
	if st == nil || rec == nil || keys == nil || refs == nil {
		return nil, errors.New("providers: a store, a recorder, a key opener and the user_ref keys are required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Runner{Sources: sources, Store: st, Recorder: rec, Keys: keys, UserRefKeys: refs,
		Logger: logger, Now: time.Now, kick: make(chan kick, 16)}, nil
}

// Available reports whether this deployment reads provider.
func (r *Runner) Available(provider string) bool { return r.Sources[provider] != nil }

// Trigger asks for one connection's pull now. It never blocks: a request while the queue is full
// is dropped, and the next scheduled pass covers it.
func (r *Runner) Trigger(tenantID, connectionID string) bool {
	select {
	case r.kick <- kick{tenantID, connectionID}:
		return true
	default:
		return false
	}
}

// Run looks every minute for the connections whose pass is due and pulls them, and pulls a
// connection whenever Trigger names it, until ctx ends.
func (r *Runner) Run(ctx context.Context) {
	ticker := time.NewTicker(checkEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case k := <-r.kick:
			t, ok, err := r.Store.Target(ctx, k.tenantID, k.connectionID)
			if err != nil {
				r.Logger.Error("provider pull: read connection", "tenant", k.tenantID, "connection", k.connectionID, "error", err)
				continue
			}
			if ok {
				r.RunConnection(ctx, t)
			}
		case <-ticker.C:
			r.RunDue(ctx)
		}
	}
}

// RunDue pulls every connection whose scheduled pass is due.
func (r *Runner) RunDue(ctx context.Context) {
	targets, err := r.Store.Targets(ctx)
	if err != nil {
		r.Logger.Error("provider pull: list connections", "error", err)
	}
	for _, t := range targets {
		if ctx.Err() != nil {
			return
		}
		if t.Due(r.Now()) {
			r.run(ctx, t, true)
		}
	}
}

// RunConnection runs one connection's pass under its lock and records the outcome.
func (r *Runner) RunConnection(ctx context.Context, t Target) { r.run(ctx, t, false) }

// run runs t's pass under its lock. A scheduled pass reads the connection again once it holds the
// lock and goes ahead only if it is still due: another replica may have pulled it since the list
// was read.
func (r *Runner) run(ctx context.Context, t Target, scheduled bool) {
	log := r.Logger.With("tenant", t.TenantID, "connection", t.ConnectionID, "provider", t.Provider)
	unlock, ok, err := r.Store.Lock(ctx, t.ConnectionID)
	if err != nil {
		log.Error("provider pull: lock", "error", err)
		return
	}
	if !ok {
		return
	}
	defer unlock()
	if scheduled {
		fresh, ok, err := r.Store.Target(ctx, t.TenantID, t.ConnectionID)
		if err != nil {
			log.Error("provider pull: read connection", "error", err)
			return
		}
		if !ok || !fresh.Due(r.Now()) {
			return
		}
		t = fresh
	}
	if err := r.Store.Started(ctx, t.TenantID, t.ConnectionID, r.Now().UTC()); err != nil {
		log.Error("provider pull: record start", "error", err)
		return
	}
	started := time.Now()
	res := r.pass(ctx, t, log)
	if err := r.Store.Finished(context.WithoutCancel(ctx), t.TenantID, t.ConnectionID, r.Now().UTC(), res); err != nil {
		log.Error("provider pull: record result", "error", err)
	}
	log.Info("provider pull: pass complete", "read", res.Read, "new", res.New, "skipped", res.Skipped,
		"error", res.Error, "duration_ms", time.Since(started).Milliseconds())
}

func (r *Runner) pass(ctx context.Context, t Target, log *slog.Logger) Result {
	src := r.Sources[t.Provider]
	if src == nil {
		return Result{Error: ErrNotAvailable}
	}
	if t.Provider == Microsoft365Copilot && t.EntraTenantID == "" {
		return Result{Error: ErrEntraNotConnected}
	}
	c := t.Connection
	if len(t.SealedKey) > 0 {
		key, err := r.Keys.Open(c.TenantID, t.SealedKey)
		if err != nil {
			log.Error("provider pull: open key", "error", err)
			return Result{Error: ErrInternal}
		}
		c.Key = key
	}
	policy, err := r.Store.Policy(ctx, c.TenantID)
	if err != nil {
		log.Error("provider pull: read policy", "error", err)
		return Result{Error: ErrInternal}
	}
	refKey, err := r.UserRefKeys.Key(ctx, c.TenantID)
	if err != nil {
		log.Error("provider pull: user_ref key", "error", err)
		return Result{Error: ErrInternal}
	}
	var res Result
	emit := func(rec Record) error {
		res.Read++
		outcome, err := r.Recorder.Record(ctx, c, policy, refKey, rec)
		if IsSkipped(err) {
			res.Skipped++
			log.Warn("provider pull: a record was passed over", "error", err)
			return nil
		}
		if err != nil {
			return err
		}
		if outcome == OutcomeInserted {
			res.New++
		}
		return nil
	}
	for round := 0; round < maxRounds; round++ {
		next, more, err := src.Pull(ctx, c, emit)
		if next != "" {
			c.Cursor, res.Cursor = next, next
		}
		if err != nil {
			res.Error = failureCode(c.Provider, err)
			log.Error("provider pull: read records", "error", err, "code", res.Error)
			return res
		}
		if !more {
			break
		}
	}
	return res
}
