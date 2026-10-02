// Package ladder is the §4 deduplication conformance fixture: the envelope builders and the
// scenario table that BOTH the in-memory ladder test and the database-gated test run.
//
// It is a normal package rather than a _test.go file for one reason: two test packages must run the
// same table. If the in-memory double and the live ingest.record_event() ever disagree, the
// disagreement shows up as one of these scenarios failing on one side, rather than as a comment
// claiming the two implementations match.
package ladder

import (
	"encoding/json"
	"fmt"
	"time"
)

// Identities used by the fixture. They are constants so the in-memory run and the database run
// describe the same submissions.
const (
	TenantID = "11111111-1111-1111-1111-111111111111"
	DeviceID = "22222222-2222-2222-2222-222222222222"
	UserRef  = "user-1"
)

// Hash returns a well-formed sha256: value whose hex is filled with a repeated nibble, so a test
// failure reads as "key a" rather than as a wall of hex.
func Hash(nibble byte) string {
	b := make([]byte, 64)
	for i := range b {
		b[i] = nibble
	}
	return "sha256:" + string(b)
}

// DeterministicUUID builds a uuid-shaped string from a small integer, so the fixture's event ids
// are readable and stable across both runs.
func DeterministicUUID(n int) string {
	return fmt.Sprintf("%08x-0000-4000-8000-%012x", n, n)
}

// PromptSpec describes one prompt envelope.
type PromptSpec struct {
	EventID       string
	Tool          string
	OccurredAt    string // RFC3339; the device's own clock (§4.3)
	Source        string
	Mode          string // m0 | m1 | m2 | m3
	SizeBytes     int64
	ContentDigest string // required above m0
	DedupKey      string
	// Attachments are name/digest pairs; an empty digest means the route could not read the bytes.
	Attachments []AttachmentSpec
}

// AttachmentSpec is one attachment descriptor.
type AttachmentSpec struct {
	Name          string
	ContentDigest string
	SizeBytes     int64
}

// Prompt builds a prompt envelope that satisfies the contract for its mode. It returns the bytes a
// device would send, which is what both the service and the stored procedure must accept.
func Prompt(s PromptSpec) json.RawMessage {
	m := map[string]any{
		"schema_version":      "1.0",
		"event_id":            s.EventID,
		"tenant_id":           TenantID,
		"device_id":           DeviceID,
		"user_ref":            UserRef,
		"tool_fingerprint":    s.Tool,
		"direction":           "egress",
		"kind":                "prompt",
		"occurred_at":         s.OccurredAt,
		"monotonic_offset_ms": 5,
		"source":              s.Source,
		"collection_mode":     s.Mode,
		"size_bytes":          s.SizeBytes,
		"policy_decision": map[string]any{
			"rule_id": "RULE_1", "action": "logged", "decided_locally": true,
		},
		"dedup_key": s.DedupKey,
	}
	if s.Mode != "m0" {
		m["confidence"] = "high"
		m["content_digest"] = s.ContentDigest
		m["labels"] = []any{}
		m["classifier_version"] = "2026.01.0-shadow"
	}
	if s.Mode == "m2" {
		m["content_excerpt"] = map[string]any{"kind": "match_span", "text": "redacted span"}
	}
	if len(s.Attachments) > 0 {
		list := make([]any, 0, len(s.Attachments))
		for _, a := range s.Attachments {
			entry := map[string]any{"name": a.Name, "size_bytes": a.SizeBytes}
			if a.ContentDigest != "" {
				entry["content_digest"] = a.ContentDigest
			}
			list = append(list, entry)
		}
		m["attachments"] = list
	}
	return mustJSON(m)
}

// RollupSpec describes one usage_rollup envelope (§4.5 Tier R).
type RollupSpec struct {
	EventID      string
	Tool         string
	OccurredAt   string
	WindowStart  string
	WindowEnd    string
	Source       string
	Mode         string
	SubmissionN  int64
	BytesTotal   int64
	DedupKey     string
}

// Rollup builds a usage_rollup envelope. A rollup carries no size, no content and no labels: the
// contract forbids all of them.
func Rollup(s RollupSpec) json.RawMessage {
	return mustJSON(map[string]any{
		"schema_version":      "1.0",
		"event_id":            s.EventID,
		"tenant_id":           TenantID,
		"device_id":           DeviceID,
		"user_ref":            UserRef,
		"tool_fingerprint":    s.Tool,
		"direction":           "none",
		"kind":                "usage_rollup",
		"occurred_at":         s.OccurredAt,
		"monotonic_offset_ms": 7,
		"source":              s.Source,
		"collection_mode":     s.Mode,
		"window_start":        s.WindowStart,
		"window_end":          s.WindowEnd,
		"submission_count":    s.SubmissionN,
		"bytes_total":         s.BytesTotal,
		"dedup_key":           s.DedupKey,
	})
}

// DetectionSpec describes one model_detection envelope (§4.5 Tier D).
type DetectionSpec struct {
	EventID        string
	Tool           string
	OccurredAt     string
	Source         string
	Mode           string
	DetectionBasis string
	DedupKey       string
}

// Detection builds a model_detection envelope.
func Detection(s DetectionSpec) json.RawMessage {
	return mustJSON(map[string]any{
		"schema_version":      "1.0",
		"event_id":            s.EventID,
		"tenant_id":           TenantID,
		"device_id":           DeviceID,
		"user_ref":            UserRef,
		"tool_fingerprint":    s.Tool,
		"direction":           "none",
		"kind":                "model_detection",
		"occurred_at":         s.OccurredAt,
		"monotonic_offset_ms": 9,
		"source":              s.Source,
		"collection_mode":     s.Mode,
		"detection_basis":     s.DetectionBasis,
		"dedup_key":           s.DedupKey,
	})
}

// Step is one submission inside a scenario.
type Step struct {
	Name      string
	Envelope  json.RawMessage
	SentAt    string // the batch's receive time
	// Expect is the ladder outcome the store must produce.
	Expect Expect
}

// Expect is what a step asserts.
type Expect struct {
	// Outcome is "inserted", "merged" or "duplicate" -- ingest.record_event()'s own vocabulary.
	Outcome string
	// SameSubmissionAs is the index of an earlier step whose submission_id must be reused, or -1.
	SameSubmissionAs int
	// WonFields is the expected tie-break result, or nil when it is not asserted.
	WonFields *bool
}

// Scenario is a named sequence of submissions.
type Scenario struct {
	Name  string
	Steps []Step
	// Why records what the scenario exists to prove.
	Why string
}

func at(hhmmss string) string { return "2026-10-02T" + hhmmss + "Z" }

func boolp(b bool) *bool { return &b }

// Scenarios is the ladder table. Both runners execute every scenario in order, one transaction.
var Scenarios = []Scenario{
	{
		Name: "same-event-two-routes",
		Why: "§4.4: two routes observing one submission produce two observations and ONE logical " +
			"submission; the higher-fidelity route wins the contested fields and the record carries both routes.",
		Steps: []Step{
			{
				Name: "route ext.web_request (rank 40) sees the prompt first",
				Envelope: Prompt(PromptSpec{
					EventID: DeterministicUUID(1), Tool: "chat-tool", OccurredAt: at("14:00:00"),
					Source: "ext.web_request", Mode: "m1", SizeBytes: 120,
					ContentDigest: Hash('a'), DedupKey: Hash('b'),
				}),
				SentAt: at("14:00:01"),
				Expect: Expect{Outcome: "inserted", SameSubmissionAs: -1, WonFields: boolp(true)},
			},
			{
				Name: "route ext.page_context (rank 10) sees the same prompt second",
				Envelope: Prompt(PromptSpec{
					EventID: DeterministicUUID(2), Tool: "chat-tool", OccurredAt: at("14:00:00"),
					Source: "ext.page_context", Mode: "m1", SizeBytes: 118,
					ContentDigest: Hash('a'), DedupKey: Hash('b'),
				}),
				SentAt: at("14:00:02"),
				Expect: Expect{Outcome: "merged", SameSubmissionAs: 0, WonFields: boolp(true)},
			},
		},
	},
	{
		Name: "same-event-two-routes-lower-fidelity-last",
		Why: "§4.4: arrival order must not matter. The lower-fidelity route arriving last changes no " +
			"winning field; it is recorded as a route and counted.",
		Steps: []Step{
			{
				Name: "ext.page_context first",
				Envelope: Prompt(PromptSpec{
					EventID: DeterministicUUID(3), Tool: "order-tool", OccurredAt: at("14:10:00"),
					Source: "ext.page_context", Mode: "m1", SizeBytes: 118,
					ContentDigest: Hash('c'), DedupKey: Hash('d'),
				}),
				SentAt: at("14:10:01"),
				Expect: Expect{Outcome: "inserted", SameSubmissionAs: -1, WonFields: boolp(true)},
			},
			{
				Name: "ext.web_request second and lower",
				Envelope: Prompt(PromptSpec{
					EventID: DeterministicUUID(4), Tool: "order-tool", OccurredAt: at("14:10:00"),
					Source: "ext.web_request", Mode: "m1", SizeBytes: 120,
					ContentDigest: Hash('c'), DedupKey: Hash('d'),
				}),
				SentAt: at("14:10:02"),
				Expect: Expect{Outcome: "merged", SameSubmissionAs: 0, WonFields: boolp(false)},
			},
		},
	},
	{
		Name: "unreconcilable-tier-t-then-tier-s",
		Why: "§4.5: a Tier-S observation has no text to compare, so it must NOT auto-merge with a " +
			"Tier-T one. Both are stored, both are counted, and the pair is left for reconciliation.",
		Steps: []Step{
			{
				Name: "Tier T: a content-reading route reports the prompt",
				Envelope: Prompt(PromptSpec{
					EventID: DeterministicUUID(5), Tool: "canvas-tool", OccurredAt: at("15:00:00"),
					Source: "ext.web_request", Mode: "m1", SizeBytes: 200,
					ContentDigest: Hash('e'), DedupKey: Hash('f'),
				}),
				SentAt: at("15:00:01"),
				Expect: Expect{Outcome: "inserted", SameSubmissionAs: -1},
			},
			{
				Name: "Tier S: M0 sees the same bucket, tool and size but cannot read content",
				Envelope: Prompt(PromptSpec{
					EventID: DeterministicUUID(6), Tool: "canvas-tool", OccurredAt: at("15:00:00"),
					Source: "ext.dom", Mode: "m0", SizeBytes: 200,
					DedupKey: Hash('1'),
				}),
				SentAt: at("15:00:02"),
				Expect: Expect{Outcome: "inserted", SameSubmissionAs: -1},
			},
		},
	},
	{
		Name: "bucket-boundary",
		Why: "§4.3: the bucket is floor(occurred_at/300s). Two M0 observations of the same size in " +
			"the same tool are one submission inside a bucket and two across a boundary.",
		Steps: []Step{
			{
				Name: "tier S at 14:34:59 (bucket 14:30:00)",
				Envelope: Prompt(PromptSpec{
					EventID: DeterministicUUID(7), Tool: "bucket-tool", OccurredAt: at("14:34:59"),
					Source: "ext.dom", Mode: "m0", SizeBytes: 42, DedupKey: Hash('2'),
				}),
				SentAt: at("14:35:01"),
				Expect: Expect{Outcome: "inserted", SameSubmissionAs: -1},
			},
			{
				Name: "tier S at 14:35:01 (bucket 14:35:00) -- one second later, a different bucket",
				Envelope: Prompt(PromptSpec{
					EventID: DeterministicUUID(8), Tool: "bucket-tool", OccurredAt: at("14:35:01"),
					Source: "ext.dom", Mode: "m0", SizeBytes: 42, DedupKey: Hash('3'),
				}),
				SentAt: at("14:35:02"),
				Expect: Expect{Outcome: "inserted", SameSubmissionAs: -1},
			},
			{
				Name: "tier S at 14:35:59 -- same bucket as the previous step, same route and material",
				Envelope: Prompt(PromptSpec{
					EventID: DeterministicUUID(9), Tool: "bucket-tool", OccurredAt: at("14:35:59"),
					Source: "ext.dom", Mode: "m0", SizeBytes: 42, DedupKey: Hash('3'),
				}),
				SentAt: at("14:36:00"),
				Expect: Expect{Outcome: "merged", SameSubmissionAs: 1},
			},
		},
	},
	{
		Name: "idempotent-replay",
		Why: "§6: a replay of an accepted event returns `duplicate` with its first submission, and " +
			"the stored row keeps its first received_at because a retry is not a second receipt.",
		Steps: []Step{
			{
				Name: "accepted",
				Envelope: Prompt(PromptSpec{
					EventID: DeterministicUUID(10), Tool: "replay-tool", OccurredAt: at("16:00:00"),
					Source: "ext.web_request", Mode: "m1", SizeBytes: 90,
					ContentDigest: Hash('4'), DedupKey: Hash('5'),
				}),
				SentAt: at("16:00:05"),
				Expect: Expect{Outcome: "inserted", SameSubmissionAs: -1},
			},
			{
				Name: "replayed two hours later after a perceived failure",
				Envelope: Prompt(PromptSpec{
					EventID: DeterministicUUID(10), Tool: "replay-tool", OccurredAt: at("16:00:00"),
					Source: "ext.web_request", Mode: "m1", SizeBytes: 90,
					ContentDigest: Hash('4'), DedupKey: Hash('5'),
				}),
				SentAt: at("18:00:00"),
				Expect: Expect{Outcome: "duplicate", SameSubmissionAs: 0},
			},
		},
	},
	{
		Name: "tier-r-rollup-supersedes",
		Why: "§4.5: a usage_rollup describes a period, so a re-sent window lands on the same row " +
			"(aggregates are upserts, never increments).",
		Steps: []Step{
			{
				Name: "window 09:00-10:00",
				Envelope: Rollup(RollupSpec{
					EventID: DeterministicUUID(11), Tool: "rollup-tool", OccurredAt: at("10:00:00"),
					WindowStart: at("09:00:00"), WindowEnd: at("10:00:00"),
					Source: "proxy.tls", Mode: "m1", SubmissionN: 5, BytesTotal: 5000,
					DedupKey: Hash('6'),
				}),
				SentAt: at("10:00:01"),
				Expect: Expect{Outcome: "inserted", SameSubmissionAs: -1},
			},
			{
				Name: "the same window re-sent after a flush",
				Envelope: Rollup(RollupSpec{
					EventID: DeterministicUUID(12), Tool: "rollup-tool", OccurredAt: at("10:00:00"),
					WindowStart: at("09:00:00"), WindowEnd: at("10:00:00"),
					Source: "proxy.tls", Mode: "m1", SubmissionN: 5, BytesTotal: 5000,
					DedupKey: Hash('6'),
				}),
				SentAt: at("10:05:00"),
				Expect: Expect{Outcome: "merged", SameSubmissionAs: 0},
			},
		},
	},
	{
		Name: "tier-d-detection-collapses-per-bucket",
		Why: "§4.5: a detection records 'a model ran', which is per device, tool and bucket, not per " +
			"invocation, so two detections in one bucket are one submission.",
		Steps: []Step{
			{
				Name: "process scan at 17:00:10",
				Envelope: Detection(DetectionSpec{
					EventID: DeterministicUUID(13), Tool: "detect-tool", OccurredAt: at("17:00:10"),
					Source: "proc.detect", Mode: "m0", DetectionBasis: "process_scan", DedupKey: Hash('7'),
				}),
				SentAt: at("17:00:11"),
				Expect: Expect{Outcome: "inserted", SameSubmissionAs: -1},
			},
			{
				Name: "process scan at 17:04:50 -- same bucket, same basis, same tool",
				Envelope: Detection(DetectionSpec{
					EventID: DeterministicUUID(14), Tool: "detect-tool", OccurredAt: at("17:04:50"),
					Source: "proc.detect", Mode: "m0", DetectionBasis: "process_scan", DedupKey: Hash('7'),
				}),
				SentAt: at("17:04:51"),
				Expect: Expect{Outcome: "merged", SameSubmissionAs: 0},
			},
		},
	},
}

// ReceiveTime parses a step's SentAt.
func (s Step) ReceiveTime() time.Time {
	t, err := time.Parse(time.RFC3339, s.SentAt)
	if err != nil {
		panic("ladder: bad SentAt " + s.SentAt + ": " + err.Error())
	}
	return t.UTC()
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic("ladder: marshal fixture: " + err.Error())
	}
	return b
}
