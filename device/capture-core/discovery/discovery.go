// Package discovery turns what the discovery collectors find (installed apps, CLIs and IDE
// extensions, running apps, local models, inference connections) into discovery envelopes: at most
// one per key per UTC day, within the bundle's daily budget, counted on the finding collector's
// health row.
package discovery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/core"
	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/capture-core/state"
	"github.com/shadow-ai-capture/device/protocol"
)

// SeenFile is the state directory's file of the keys emitted today.
const SeenFile = "discovery-seen.json"

// UnattributedUserRef is the user_ref of a machine-wide fact, such as an app installed for every
// user.
const UnattributedUserRef = "unattributed"

// dayLayout is how a UTC day appears in the key and in the seen file.
const dayLayout = "2006-01-02"

// Record is one thing a collector found.
type Record struct {
	Type  protocol.DiscoveryType
	Basis protocol.DetectionBasis

	// Route is the emitting collector's route: inv.scan, proc.detect or net.flow.
	Route protocol.Route

	// AppKey is the catalog app; the record's tool_fingerprint is app:<AppKey>.
	AppKey          string
	Version         string
	Publisher       string
	HostApp         string
	DestinationHost string
	ModelNames      []string

	// UserRef is who the fact belongs to, UnattributedUserRef for a machine-wide one. Person, when
	// set, attributes the envelope; otherwise the envelope carries UserRef alone.
	UserRef string
	Person  *core.Person

	OccurredAt time.Time
}

// Pipeline is the part of core.Pipeline the emitter uses.
type Pipeline interface {
	Record(ctx context.Context, f core.Fact) error
}

// Config is what an Emitter is built with.
type Config struct {
	Pipeline Pipeline
	Dir      state.Dir
	Clock    func() time.Time
	// Bundles returns the bundle in force; nil when none is, which leaves a budget of zero.
	Bundles  func() *policy.Bundle
	DeviceID string
}

// seen is the seen file: the UTC day and the hex keys emitted on it.
type seen struct {
	Day  string   `json:"day"`
	Keys []string `json:"keys"`
}

// Emitter de-duplicates, budgets and emits discovery records. It is safe for concurrent use.
type Emitter struct {
	cfg       Config
	path      string
	startedAt time.Time

	mu  sync.Mutex
	day string
	set map[string]struct{}
	// loadErrors is a seen file found unreadable at construction, counted as an error on the
	// first collector set the emitter counts on.
	loadErrors uint64
}

// New builds an emitter and loads today's seen keys. A seen file that cannot be read or decoded is
// replaced with an empty one; it never stops the emitter.
func New(cfg Config) (*Emitter, error) {
	if cfg.Pipeline == nil || cfg.Clock == nil || cfg.Bundles == nil {
		return nil, errors.New("discovery: an emitter needs a pipeline, a clock and the bundle in force")
	}
	if cfg.DeviceID == "" {
		return nil, errors.New("discovery: an emitter needs the device id")
	}
	now := cfg.Clock()
	e := &Emitter{
		cfg:       cfg,
		path:      cfg.Dir.Path(SeenFile),
		startedAt: now,
		day:       utcDay(now),
		set:       map[string]struct{}{},
	}
	s, err := e.load()
	switch {
	case err != nil:
		e.loadErrors++
		_ = e.persist()
	case s.Day == e.day:
		for _, k := range s.Keys {
			e.set[k] = struct{}{}
		}
	}
	return e, nil
}

// Emit emits r unless its key was already emitted today or today's budget is spent. A repeat is
// dropped without counting; a record past the budget counts dropped; an emitted one counts
// emitted, and a failure counts errors, on the collector's set.
func (e *Emitter) Emit(ctx context.Context, collector *core.CounterSet, r Record) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.carryLoadErrors(collector)
	e.rollDay()

	if err := validate(r); err != nil {
		collector.Add(protocol.CounterErrors)
		return err
	}
	userRef := r.UserRef
	if userRef == "" && r.Person != nil {
		userRef = r.Person.UserRef
	}
	key := Key(e.cfg.DeviceID, userRef, r)
	if _, ok := e.set[key]; ok {
		return nil
	}
	if len(e.set) >= e.budget() {
		collector.Add(protocol.CounterDropped)
		return nil
	}

	person := r.Person
	if person == nil && r.UserRef != "" {
		person = &core.Person{UserRef: r.UserRef}
	}
	err := e.cfg.Pipeline.Record(ctx, core.Fact{
		Kind:              protocol.KindDiscovery,
		Route:             r.Route,
		ToolFingerprint:   "app:" + r.AppKey,
		Person:            person,
		OccurredAt:        r.OccurredAt,
		MonotonicOffsetMS: e.cfg.Clock().Sub(e.startedAt).Milliseconds(),
		DedupKey:          "sha256:" + key,
		FactFields: core.FactFields{
			DiscoveryType:   r.Type,
			DetectionBasis:  r.Basis,
			AppVersion:      r.Version,
			Publisher:       r.Publisher,
			HostApp:         r.HostApp,
			DestinationHost: r.DestinationHost,
			ModelNames:      r.ModelNames,
		},
	})
	if err != nil {
		collector.Add(protocol.CounterErrors)
		return fmt.Errorf("discovery: recording %s app:%s: %w", r.Type, r.AppKey, err)
	}
	collector.Add(protocol.CounterEmitted)
	e.set[key] = struct{}{}
	if err := e.persist(); err != nil {
		// The record is spooled and today's key is kept in memory; only a restart today could
		// emit it again.
		collector.Add(protocol.CounterErrors)
		return err
	}
	return nil
}

// Stop records that a running app stopped. A stop is the process monitor's bookkeeping, never an
// envelope: it counts observed and nothing else.
func (e *Emitter) Stop(_ context.Context, collector *core.CounterSet, _ Record) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.carryLoadErrors(collector)
	collector.Add(protocol.CounterObserved)
}

// Key is the record's daily key, sha256(device|user_ref|type|app_key|version|destination_host|day)
// in hex, over the UTC day of r.OccurredAt.
func Key(deviceID, userRef string, r Record) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		deviceID, userRef, string(r.Type), r.AppKey, r.Version, r.DestinationHost, utcDay(r.OccurredAt),
	}, "|")))
	return hex.EncodeToString(sum[:])
}

func validate(r Record) error {
	switch r.Route {
	case protocol.RouteInvScan, protocol.RouteProcDetect, protocol.RouteNetFlow:
	default:
		return fmt.Errorf("discovery: route %q is not a discovery route", r.Route)
	}
	if r.AppKey == "" {
		return errors.New("discovery: a record names no app")
	}
	if r.OccurredAt.IsZero() {
		return errors.New("discovery: a record has no time")
	}
	return nil
}

func (e *Emitter) budget() int {
	if b := e.cfg.Bundles(); b != nil {
		return b.Endpoint.DiscoveryDailyBudget
	}
	return 0
}

// rollDay starts an empty seen set when the clock's UTC day has moved on.
func (e *Emitter) rollDay() {
	if today := utcDay(e.cfg.Clock()); today != e.day {
		e.day = today
		e.set = map[string]struct{}{}
	}
}

func (e *Emitter) carryLoadErrors(collector *core.CounterSet) {
	if e.loadErrors > 0 {
		collector.Incr(protocol.CounterErrors, e.loadErrors)
		e.loadErrors = 0
	}
}

// load reads the seen file. A missing file is an empty set; one that cannot be read or decoded
// is an error.
func (e *Emitter) load() (seen, error) {
	raw, err := os.ReadFile(e.path)
	if errors.Is(err, fs.ErrNotExist) {
		return seen{}, nil
	}
	if err != nil {
		return seen{}, err
	}
	var s seen
	if err := json.Unmarshal(raw, &s); err != nil {
		return seen{}, err
	}
	if _, err := time.Parse(dayLayout, s.Day); err != nil {
		return seen{}, fmt.Errorf("discovery: the seen file's day %q: %w", s.Day, err)
	}
	return s, nil
}

func (e *Emitter) persist() error {
	s := seen{Day: e.day, Keys: make([]string, 0, len(e.set))}
	for k := range e.set {
		s.Keys = append(s.Keys, k)
	}
	slices.Sort(s.Keys)
	raw, err := json.Marshal(s)
	if err != nil {
		return err
	}
	if err := state.WriteFile(e.path, raw); err != nil {
		return fmt.Errorf("discovery: writing %s: %w", e.path, err)
	}
	return nil
}

func utcDay(t time.Time) string { return t.UTC().Format(dayLayout) }
