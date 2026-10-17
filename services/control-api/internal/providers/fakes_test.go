package providers

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/shadow-ai-capture/device/protocol"
)

// fakeStore is the database as a pass sees it: connections, the tenant's policy, and what the pass
// recorded, with record_provider_event's outcomes keyed by event id.
type fakeStore struct {
	mu        sync.Mutex
	policy    Policy
	targets   []Target
	locked    bool
	events    []map[string]any
	outcomes  map[string]string // event id -> outcome to report the first time
	seen      map[string]bool
	pending   bool
	grants    []Grant
	usage     int64
	started   int
	finished  []Result
	recordErr error
	// listed, when set, is what Targets lists instead of targets: a list read before another
	// replica's pass.
	listed []Target
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		policy:   Policy{CeilingMode: "m1", Mode: "m1", ContentSearch: "disabled"},
		outcomes: map[string]string{},
		seen:     map[string]bool{},
	}
}

func (s *fakeStore) RecordEvent(_ context.Context, _ string, env map[string]any) (string, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.recordErr != nil {
		return "", "", s.recordErr
	}
	// The envelope travels as JSON; keep what the database would receive.
	raw, err := json.Marshal(env)
	if err != nil {
		return "", "", err
	}
	var round map[string]any
	_ = json.Unmarshal(raw, &round)
	s.events = append(s.events, round)
	id, _ := env["event_id"].(string)
	if s.seen[id] {
		return OutcomeDuplicate, "50b00000-0000-4000-8000-000000000001", nil
	}
	s.seen[id] = true
	if o := s.outcomes[id]; o != "" {
		return o, "50b00000-0000-4000-8000-000000000001", nil
	}
	return OutcomeInserted, "50b00000-0000-4000-8000-000000000001", nil
}

func (s *fakeStore) ContentPending(context.Context, string, string, string) (bool, error) {
	return s.pending, nil
}

func (s *fakeStore) GrantContent(_ context.Context, _ string, g Grant) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.grants = append(s.grants, g)
	return nil
}

func (s *fakeStore) AddContentUsage(_ context.Context, _ string, n int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.usage += n
	return nil
}

func (s *fakeStore) Targets(context.Context) ([]Target, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listed != nil {
		return s.listed, nil
	}
	return append([]Target(nil), s.targets...), nil
}

func (s *fakeStore) Target(_ context.Context, _, id string) (Target, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.targets {
		if t.ConnectionID == id {
			return t, true, nil
		}
	}
	return Target{}, false, nil
}

func (s *fakeStore) Lock(context.Context, string) (func(), bool, error) {
	if s.locked {
		return nil, false, nil
	}
	return func() {}, true, nil
}

// Started counts the pass and records its start on the connection, as the database does.
func (s *fakeStore) Started(_ context.Context, _, id string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.started++
	for i := range s.targets {
		if s.targets[i].ConnectionID == id {
			s.targets[i].LastStartedAt = at
		}
	}
	return nil
}

func (s *fakeStore) Finished(_ context.Context, _, _ string, _ time.Time, r Result) error {
	s.finished = append(s.finished, r)
	return nil
}

func (s *fakeStore) Policy(context.Context, string) (Policy, error) { return s.policy, nil }

// fakeClassifier labels every prompt it is asked about with one class, and records the requests.
type fakeClassifier struct {
	requests []protocol.ClassifyRequest
}

func (c *fakeClassifier) Classify(_ context.Context, req protocol.ClassifyRequest) protocol.ClassifyResponse {
	c.requests = append(c.requests, req)
	resp := protocol.ClassifyResponse{
		Labels:            []protocol.Label{{Class: "customer_pii", Score: 0.9, RuleID: "EMAIL_ADDRESS"}},
		ClassifierVersion: "rules-1.0.0+model-1.0.0",
		Confidence:        protocol.ConfidenceHigh,
	}
	if req.Mode == protocol.ModeM2 {
		resp.Excerpt = &protocol.Excerpt{Kind: protocol.ExcerptMatchSpan, Text: "a@b.example"}
	}
	return resp
}

// fakeVault stores what it is handed.
type fakeVault struct {
	puts []string
	err  error
}

func (v *fakeVault) Put(_ context.Context, _, eventID, grantID, rawDigest string, body []byte) (bool, error) {
	if v.err != nil {
		return false, v.err
	}
	v.puts = append(v.puts, eventID+" "+grantID+" "+rawDigest+" "+string(body))
	return true, nil
}

// fakeKeys hands every tenant the same user_ref key.
type fakeKeys struct{}

func (fakeKeys) Key(context.Context, string) ([]byte, error) { return testRefKey, nil }

var testRefKey = []byte("0123456789abcdef0123456789abcdef")

// fakeOpener opens a "sealed" key by reading it back.
type fakeOpener struct{}

func (fakeOpener) Open(_ string, sealed []byte) (string, error) {
	if string(sealed) == "unreadable" {
		return "", errors.New("authentication failed")
	}
	return "opened:" + string(sealed), nil
}

// fakeSource returns its rounds' records one pass round at a time.
type fakeSource struct {
	rounds []fakeRound
	calls  []Connection
	// pulled, when set, hears of each pull.
	pulled chan struct{}
}

type fakeRound struct {
	records []Record
	next    string
	more    bool
	err     error
}

func (f *fakeSource) Pull(_ context.Context, c Connection, emit func(Record) error) (string, bool, error) {
	f.calls = append(f.calls, c)
	if f.pulled != nil {
		defer func() { f.pulled <- struct{}{} }()
	}
	if len(f.calls) > len(f.rounds) {
		return "", false, nil
	}
	r := f.rounds[len(f.calls)-1]
	for _, rec := range r.records {
		if err := emit(rec); err != nil {
			return "", false, err
		}
	}
	return r.next, r.more, r.err
}
