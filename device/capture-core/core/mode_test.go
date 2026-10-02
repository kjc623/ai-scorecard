package core

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/protocol"
)

func testBundle(version string, defaultMode protocol.CollectionMode, toolModes map[string]protocol.CollectionMode) *policy.Bundle {
	return &policy.Bundle{
		Version:       version,
		EffectiveAt:   time.Unix(1_700_000_000, 0),
		Actor:         "tenant-admin@example.invalid",
		TenantDefault: defaultMode,
		ToolModes:     toolModes,
		Classifier:    policy.ClassifierRelease{ReleaseID: "rel-1", State: policy.ReleaseEnforcing},
	}
}

func TestMostRestrictiveOrdersM0BelowM3(t *testing.T) {
	cases := []struct {
		in   []protocol.CollectionMode
		want protocol.CollectionMode
	}{
		{[]protocol.CollectionMode{protocol.ModeM3, protocol.ModeM1, protocol.ModeM2}, protocol.ModeM1},
		{[]protocol.CollectionMode{protocol.ModeM3, protocol.ModeM2}, protocol.ModeM2},
		{[]protocol.CollectionMode{protocol.ModeM0, protocol.ModeM3}, protocol.ModeM0},
		{[]protocol.CollectionMode{}, protocol.ModeM0},
		{[]protocol.CollectionMode{"m9"}, protocol.ModeM0},
	}
	for _, c := range cases {
		if got := MostRestrictive(c.in...); got != c.want {
			t.Errorf("MostRestrictive(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestResolveTakesTheLowestAcrossEveryAxis(t *testing.T) {
	b := testBundle("7", protocol.ModeM3, map[string]protocol.CollectionMode{"coding_assistant": protocol.ModeM2})
	b.PopulationModes = map[string]protocol.CollectionMode{"contractors": protocol.ModeM1}
	b.DeviceModes = map[string]protocol.CollectionMode{"dev-1": protocol.ModeM3}
	b.ClassPriors = map[string][]string{"coding_assistant": {"source_code", "customer_pii"}}
	b.ClassModes = map[string]protocol.CollectionMode{"source_code": protocol.ModeM3, "customer_pii": protocol.ModeM0}

	got := Resolve(b, ScopeQuery{ToolFingerprint: "coding_assistant", Population: "contractors", DeviceID: "dev-1", UserRef: "u-1"})
	if got.Mode != protocol.ModeM0 {
		t.Fatalf("mode = %q, want m0: the class ceiling for an admitted class must win", got.Mode)
	}
	// Without the health prior the tool resolves to m1 (tool m2, population m1).
	b.ClassPriors = map[string][]string{"coding_assistant": {"source_code"}}
	got = Resolve(b, ScopeQuery{ToolFingerprint: "coding_assistant", Population: "contractors", DeviceID: "dev-1", UserRef: "u-1"})
	if got.Mode != protocol.ModeM1 {
		t.Fatalf("mode = %q, want m1", got.Mode)
	}
	if len(got.Contributions) == 0 {
		t.Fatal("resolution reported no contributions, so an over-restriction could not be explained")
	}
}

func TestResolveUnsetScopeResolvesToTenantDefaultNeverToEverything(t *testing.T) {
	b := testBundle("7", protocol.ModeM1, map[string]protocol.CollectionMode{"known_tool": protocol.ModeM3})
	got := Resolve(b, ScopeQuery{ToolFingerprint: "known_tool", Population: "", DeviceID: "dev-9", UserRef: "u-9"})
	if got.Mode != protocol.ModeM1 {
		t.Fatalf("mode = %q, want the tenant default m1: a tool entry cannot exceed the tenant-wide default", got.Mode)
	}
	got = Resolve(b, ScopeQuery{ToolFingerprint: "unknown_tool", Population: "", DeviceID: "dev-9", UserRef: "u-9"})
	if got.Mode != protocol.ModeM1 {
		t.Fatalf("mode = %q, want the tenant default m1", got.Mode)
	}
	var sawDefault bool
	for _, r := range got.Reasons {
		if r == ReasonTenantDefault {
			sawDefault = true
		}
	}
	if !sawDefault {
		t.Fatal("the tenant default was applied but not reported as the reason")
	}
}

func TestResolveUnresolvableScopeEntryResolvesDownward(t *testing.T) {
	b := testBundle("7", protocol.ModeM3, map[string]protocol.CollectionMode{"broken_tool": "m7"})
	got := Resolve(b, ScopeQuery{ToolFingerprint: "broken_tool", DeviceID: "dev-1", UserRef: "u-1"})
	if got.Mode != protocol.ModeM0 {
		t.Fatalf("mode = %q, want m0: a matrix that fails to resolve resolves downward", got.Mode)
	}
}

func TestResolveWithoutBundleIsM0(t *testing.T) {
	got := Resolve(nil, ScopeQuery{ToolFingerprint: "anything", UserRef: "u-1"})
	if got.Mode != protocol.ModeM0 {
		t.Fatalf("mode = %q, want m0", got.Mode)
	}
	if len(got.Reasons) != 1 || got.Reasons[0] != ReasonNoBundle {
		t.Fatalf("reasons = %v, want the no-bundle reason", got.Reasons)
	}
}

func TestResolveNoticeGateLowersToM0AndReportsWhy(t *testing.T) {
	b := testBundle("7", protocol.ModeM3, map[string]protocol.CollectionMode{"tool": protocol.ModeM3})
	b.RequiredNoticeVersion = "notice-3"
	b.AcknowledgedNotices = map[string]string{"u-1": "notice-2"}

	got := Resolve(b, ScopeQuery{ToolFingerprint: "tool", DeviceID: "dev-1", UserRef: "u-1"})
	if got.Mode != protocol.ModeM0 {
		t.Fatalf("mode = %q, want m0 for an unacknowledged notice", got.Mode)
	}
	if len(got.Reasons) == 0 || got.Reasons[0] != ReasonNoticeUnacked {
		t.Fatalf("reasons = %v, want %s", got.Reasons, ReasonNoticeUnacked)
	}

	b.AcknowledgedNotices["u-1"] = "notice-3"
	got = Resolve(b, ScopeQuery{ToolFingerprint: "tool", DeviceID: "dev-1", UserRef: "u-1"})
	if got.Mode != protocol.ModeM3 {
		t.Fatalf("mode = %q, want m3 once the notice is acknowledged", got.Mode)
	}
}

// The property §13.3 is built around: a bundle that fails verification never changes what the
// device is enforcing. This test tampers with a bundle that would *widen* a tool from m1 to m3
// and asserts the widening never happens.
func TestTamperedBundleNeverWidensAMode(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	verifier, err := policy.NewVerifier("policy-key-1", pub)
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}
	store, err := policy.NewStore(verifier, nil)
	if err != nil {
		t.Fatalf("store: %v", err)
	}

	inForce := testBundle("10", protocol.ModeM3, map[string]protocol.CollectionMode{"tool": protocol.ModeM1})
	raw, err := policy.Sign("policy-key-1", priv, inForce)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if res := store.Apply(raw); res.Outcome != policy.OutcomeAccepted {
		t.Fatalf("first bundle: %+v", res)
	}
	if got := Resolve(store.InForce(), ScopeQuery{ToolFingerprint: "tool"}); got.Mode != protocol.ModeM1 {
		t.Fatalf("precondition: mode = %q, want m1", got.Mode)
	}

	widened := testBundle("11", protocol.ModeM3, map[string]protocol.CollectionMode{"tool": protocol.ModeM3})
	tamperedRaw, err := policy.Sign("policy-key-1", priv, widened)
	if err != nil {
		t.Fatalf("sign widened: %v", err)
	}
	// Flip one byte inside the payload: the signature no longer covers it.
	var env map[string]json.RawMessage
	if err := json.Unmarshal(tamperedRaw, &env); err != nil {
		t.Fatalf("unmarshal signed bundle: %v", err)
	}
	payload := string(env["payload"])
	if !contains(payload, `"m3"`) {
		t.Fatalf("test setup: widened payload does not contain m3: %s", payload)
	}
	env["payload"] = json.RawMessage(replaceFirst(payload, `"m3"`, `"m2"`))
	tamperedRaw, err = json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal tampered: %v", err)
	}

	res := store.Apply(tamperedRaw)
	if res.Outcome != policy.OutcomeRetainedPrevious {
		t.Fatalf("tampered bundle outcome = %q, want retained_previous", res.Outcome)
	}
	if res.Cause != policy.CauseSignatureInvalid {
		t.Fatalf("cause = %q, want %q", res.Cause, policy.CauseSignatureInvalid)
	}
	if got := Resolve(store.InForce(), ScopeQuery{ToolFingerprint: "tool"}); got.Mode != protocol.ModeM1 {
		t.Fatalf("mode after a tampered bundle = %q, want m1 (the previous bundle stays enforced)", got.Mode)
	}
}

// With no previous bundle, a failed verification is M0 — a reduction in capability, never an
// increase (§13.3 rule 5).
func TestFailedFirstBundleFallsToM0(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	verifier, _ := policy.NewVerifier("policy-key-1", pub)
	store, _ := policy.NewStore(verifier, nil)
	res := store.Apply([]byte(`{"key_id":"policy-key-1","algorithm":"ed25519","payload":{},"signature":"AAAA"}`))
	if res.Outcome != policy.OutcomeFellToM0 {
		t.Fatalf("outcome = %q, want fell_to_m0", res.Outcome)
	}
	if got := Resolve(store.InForce(), ScopeQuery{ToolFingerprint: "tool"}); got.Mode != protocol.ModeM0 {
		t.Fatalf("mode = %q, want m0 with no bundle in force", got.Mode)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}

func replaceFirst(h, old, new string) string {
	i := indexOf(h, old)
	if i < 0 {
		return h
	}
	return h[:i] + new + h[i+len(old):]
}
