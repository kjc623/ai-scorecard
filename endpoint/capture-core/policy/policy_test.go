package policy

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/protocol"
)

func testBundle(version string) *Bundle {
	return &Bundle{
		Version:       version,
		EffectiveAt:   time.Unix(1_700_000_000, 0),
		Actor:         "tenant-admin@example.invalid",
		TenantDefault: protocol.ModeM3,
		ToolModes:     map[string]protocol.CollectionMode{"coding_assistant": protocol.ModeM1},
		Interception: Interception{
			TenantHosts: []string{"api.example.invalid", ".corp.example.invalid"},
			SeedHosts:   []string{"api.openai.invalid"},
			Ports:       []int{443, 8443},
		},
		Loopback: LoopbackPolicy{Ports: []LoopbackPort{{
			ToolFingerprint: "ollama", Port: 11434, UpstreamPort: 21434, PreflightPath: "/", Mode: protocol.ModeM1, OriginalPort: 11434,
		}}},
	}
}

func newKeyPair(t *testing.T, keyID string) (*Verifier, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	v, err := NewVerifier(keyID, pub)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	return v, priv
}

func TestVerifySignAndOpenRoundTrip(t *testing.T) {
	v, priv := newKeyPair(t, "policy-key-1")
	b := testBundle("42")
	raw, err := Sign("policy-key-1", priv, b)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	got, err := v.Open(raw, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got.Version != "42" || got.ToolModes["coding_assistant"] != protocol.ModeM1 {
		t.Fatalf("opened bundle = %+v", got)
	}
}

func TestVerifySignatureFailuresAreNamed(t *testing.T) {
	v, priv := newKeyPair(t, "policy-key-1")
	otherVerifier, _ := newKeyPair(t, "policy-key-1") // same id, different key
	b := testBundle("42")
	raw, err := Sign("policy-key-1", priv, b)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	// Payload tampering: flip a byte inside the payload; the signature no longer covers it.
	var env map[string]json.RawMessage
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	env["payload"] = json.RawMessage(strings.Replace(string(env["payload"]), `"m1"`, `"m3"`, 1))
	tampered, _ := json.Marshal(env)
	if _, err := v.Open(tampered, nil); CauseOf(err) != CauseSignatureInvalid {
		t.Fatalf("tampered payload cause = %q, want %q (err=%v)", CauseOf(err), CauseSignatureInvalid, err)
	}

	// A signature from a different key under the same key id must not verify.
	if _, err := otherVerifier.Open(raw, nil); CauseOf(err) != CauseSignatureInvalid {
		t.Fatalf("wrong key cause = %q, want %q", CauseOf(err), CauseSignatureInvalid)
	}

	// A key id the device does not hold is a signature failure, not a "try the other key".
	wrongID, err := NewVerifier("policy-key-2", otherVerifier.pub)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	if _, err := wrongID.Open(raw, nil); CauseOf(err) != CauseSignatureInvalid {
		t.Fatalf("unknown key id cause = %q, want %q", CauseOf(err), CauseSignatureInvalid)
	}

	// A non-ed25519 algorithm is refused before anything else.
	env["algorithm"] = json.RawMessage(`"rsa"`)
	alg, _ := json.Marshal(env)
	if _, err := v.Open(alg, nil); CauseOf(err) != CauseSignatureInvalid {
		t.Fatalf("algorithm cause = %q, want %q", CauseOf(err), CauseSignatureInvalid)
	}
}

func TestVerifySchemaFailures(t *testing.T) {
	v, priv := newKeyPair(t, "policy-key-1")

	// A field the device does not understand: the bundle is refused rather than partly applied,
	// because a device that does not understand a policy field cannot claim to enforce it.
	b := testBundle("42")
	raw, _ := Sign("policy-key-1", priv, b)
	var env map[string]json.RawMessage
	_ = json.Unmarshal(raw, &env)
	var payload map[string]json.RawMessage
	_ = json.Unmarshal(env["payload"], &payload)
	payload["future_knob"] = json.RawMessage(`true`)
	newPayload, _ := json.Marshal(payload)
	env["payload"] = newPayload
	sig := ed25519.Sign(priv, newPayload)
	env["signature"] = json.RawMessage(`"` + base64.StdEncoding.EncodeToString(sig) + `"`)
	rawUnknown, _ := json.Marshal(env)
	if _, err := v.Open(rawUnknown, nil); CauseOf(err) != CauseSchemaInvalid {
		t.Fatalf("unknown field cause = %q, want %q", CauseOf(err), CauseSchemaInvalid)
	}

	// An unparseable mode is a matrix that cannot resolve, so the bundle is refused outright.
	bad := testBundle("43")
	bad.ToolModes = map[string]protocol.CollectionMode{"tool": "m9"}
	if _, err := Sign("policy-key-1", priv, bad); err == nil {
		t.Fatal("Sign accepted a bundle with an out-of-vocabulary mode")
	}
}

func TestVerifyVersionRegressionIsRefused(t *testing.T) {
	v, priv := newKeyPair(t, "policy-key-1")
	inForce := testBundle("10")
	older := testBundle("9")
	raw, _ := Sign("policy-key-1", priv, older)
	if _, err := v.Open(raw, inForce); CauseOf(err) != CauseVersionRegression {
		t.Fatalf("older bundle cause = %q, want %q", CauseOf(err), CauseVersionRegression)
	}
	same, _ := Sign("policy-key-1", priv, testBundle("10"))
	if _, err := v.Open(same, inForce); err != nil {
		t.Fatalf("same-version bundle refused: %v", err)
	}
	newer, _ := Sign("policy-key-1", priv, testBundle("11"))
	if _, err := v.Open(newer, inForce); err != nil {
		t.Fatalf("newer bundle refused: %v", err)
	}
	// An unorderable version is refused in the safe direction: retaining the previous bundle is
	// never a widening, accepting a downgrade is.
	opaque := testBundle("release-candidate")
	inForceOpaque := testBundle("release-other")
	rawOpaque, _ := Sign("policy-key-1", priv, opaque)
	if _, err := v.Open(rawOpaque, inForceOpaque); CauseOf(err) != CauseVersionRegression {
		t.Fatalf("unorderable version cause = %q, want %q", CauseOf(err), CauseVersionRegression)
	}
}

func TestStoreRetainsPreviousOnFailure(t *testing.T) {
	v, priv := newKeyPair(t, "policy-key-1")
	store, err := NewStore(v)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	raw, _ := Sign("policy-key-1", priv, testBundle("10"))
	if res := store.Apply(raw); res.Outcome != OutcomeAccepted || res.Version != "10" {
		t.Fatalf("first apply = %+v", res)
	}
	// A tampered bundle that would widen every tool.
	widened := testBundle("11")
	widened.TenantDefault = protocol.ModeM3
	rawWidened, _ := Sign("policy-key-1", priv, widened)
	rawWidened = []byte(strings.Replace(string(rawWidened), "11", "12", 1))
	res := store.Apply(rawWidened)
	if res.Outcome != OutcomeRetainedPrevious {
		t.Fatalf("outcome = %q, want retained_previous", res.Outcome)
	}
	if res.Cause != CauseSignatureInvalid {
		t.Fatalf("cause = %q, want %q", res.Cause, CauseSignatureInvalid)
	}
	if got := store.InForce(); got == nil || got.Version != "10" {
		t.Fatalf("bundle in force = %+v, want version 10", got)
	}
	if store.Failures() != 1 {
		t.Fatalf("failures = %d, want 1", store.Failures())
	}
}

// With no previous bundle a failed verification is M0 — a reduction, never an
// increase.
func TestStoreNoPreviousBundleMeansM0(t *testing.T) {
	v, _ := newKeyPair(t, "policy-key-1")
	store, _ := NewStore(v)
	res := store.Apply([]byte(`{"key_id":"policy-key-1","algorithm":"ed25519","payload":{},"signature":"AAAA"}`))
	if res.Outcome != OutcomeFellToM0 {
		t.Fatalf("outcome = %q, want fell_to_m0", res.Outcome)
	}
	if store.InForce() != nil {
		t.Fatal("a bundle is in force after a failed first verification")
	}
	if res.Severity != SeverityWarning {
		t.Fatalf("severity = %q, want warning", res.Severity)
	}
}

// Repeated failures escalate and back off, so a fleet-wide signing problem does not
// become a request storm.
func TestStoreEscalatesAndBacksOff(t *testing.T) {
	v, _ := newKeyPair(t, "policy-key-1")
	store, _ := NewStore(v)
	base := time.Minute
	bad := []byte(`{"key_id":"policy-key-1","algorithm":"ed25519","payload":{},"signature":"AAAA"}`)
	var last Result
	for i := 0; i < escalationThreshold; i++ {
		last = store.Apply(bad)
	}
	if last.Severity != SeverityCritical {
		t.Fatalf("severity after %d failures = %q, want critical", escalationThreshold, last.Severity)
	}
	if got := store.PollBackoff(base); got <= base {
		t.Fatalf("backoff = %v, want more than the base after escalation", got)
	}
	if got := store.PollBackoff(base); got > 64*base {
		t.Fatalf("backoff = %v, want a bounded value", got)
	}
	// No bundle was ever put in force by a failure.
	if store.InForce() != nil || store.Failures() != escalationThreshold {
		t.Fatalf("store state after failures: inForce=%v failures=%d", store.InForce(), store.Failures())
	}
}

func TestCauseDetailUsesTheClosedVocabulary(t *testing.T) {
	for _, c := range []Cause{CauseSignatureInvalid, CauseSchemaInvalid, CauseVersionRegression} {
		d := c.Detail()
		if !d.Valid() {
			t.Errorf("cause %q maps to detail %q, which is outside protocol.Detail's closed vocabulary", c, d)
		}
		h := protocol.HealthReport{
			DeviceID: "d", Collector: string(protocol.RouteProxyTLS), State: protocol.StateTampered,
			Detail: d, Since: time.Unix(0, 0), Counters: map[protocol.Counter]uint64{},
		}
		if err := h.Validate(); err != nil {
			t.Errorf("a tampered row naming cause %q does not validate: %v", c, err)
		}
	}
	if CauseAccepted.Detail() != protocol.DetailNone {
		t.Fatal("an accepted bundle has no cause to report")
	}
}

func TestBundleInterceptsIsAScopeNotADiscoveryMechanism(t *testing.T) {
	b := testBundle("42")
	cases := []struct {
		host string
		port int
		want bool
	}{
		{"api.example.invalid", 443, true},
		{"api.example.invalid", 8443, true},
		{"sub.corp.example.invalid", 443, true},  // a leading-dot entry covers the subtree
		{"corp.example.invalid", 443, true},      // and the apex
		{"api.openai.invalid", 443, true},        // the vendor seed set
		{"api.example.invalid", 993, false},      // a port outside the configured set
		{"unlisted.example.invalid", 443, false}, // blind-tunnelled
		{"evil-api.example.invalid.evil", 443, false},
	}
	for _, c := range cases {
		if got := b.Intercepts(c.host, c.port); got != c.want {
			t.Errorf("Intercepts(%q,%d) = %v, want %v", c.host, c.port, got, c.want)
		}
	}
	// Default port set is 443 only; non-443 interception is per-tenant opt-in.
	noPorts := testBundle("42")
	noPorts.Interception.Ports = nil
	if noPorts.Intercepts("api.example.invalid", 8443) {
		t.Fatal("a non-443 port was intercepted without a policy entry")
	}
	if !noPorts.Intercepts("api.example.invalid", 443) {
		t.Fatal("443 must be interceptable by default")
	}
	if (*Bundle)(nil).Intercepts("api.example.invalid", 443) {
		t.Fatal("a nil bundle intercepts nothing, because nil means M0")
	}
}

func TestBundleValidateRejectsWhatCannotBeEnforced(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Bundle)
	}{
		{"no version", func(b *Bundle) { b.Version = "" }},
		{"no effective_at", func(b *Bundle) { b.EffectiveAt = time.Time{} }},
		{"bad tenant default", func(b *Bundle) { b.TenantDefault = "m9" }},
		{"empty scope key", func(b *Bundle) { b.ToolModes = map[string]protocol.CollectionMode{"": protocol.ModeM1} }},
		{"kill switch names an unknown route", func(b *Bundle) {
			b.KillSwitches = []KillSwitch{{Provider: "proxy.magic", Mode: KillDisable, EffectiveAt: time.Unix(0, 0), ReasonCode: "x"}}
		}},
		{"kill switch without a reason", func(b *Bundle) {
			b.KillSwitches = []KillSwitch{{Provider: protocol.RouteProxyTLS, Mode: KillDisable, EffectiveAt: time.Unix(0, 0)}}
		}},
		{"default port 0", func(b *Bundle) {
			b.Loopback.Ports[0].Port = 0
		}},
		{"broker forwards to itself", func(b *Bundle) {
			b.Loopback.Ports[0].UpstreamPort = b.Loopback.Ports[0].Port
		}},
	}
	for _, c := range cases {
		b := testBundle("42")
		c.mutate(b)
		if err := b.Validate(); err == nil {
			t.Errorf("%s: bundle validated", c.name)
		}
	}
	if err := testBundle("42").Validate(); err != nil {
		t.Fatalf("the base bundle must validate: %v", err)
	}
}

func TestBundleKillSwitchIsLookedUpByRoute(t *testing.T) {
	b := testBundle("42")
	b.KillSwitches = []KillSwitch{{
		Provider: protocol.RouteProxyTLS, Mode: KillDisable,
		EffectiveAt: time.Unix(1_700_000_000, 0), ReasonCode: "fleet_regression_1234",
	}}
	got, ok := b.KillSwitchFor(protocol.RouteProxyTLS)
	if !ok || got.ReasonCode != "fleet_regression_1234" {
		t.Fatalf("kill switch = %+v ok=%v", got, ok)
	}
	if _, ok := b.KillSwitchFor(protocol.RouteProxyLoopback); ok {
		t.Fatal("a route without a kill switch reported one")
	}
	if _, ok := (*Bundle)(nil).KillSwitchFor(protocol.RouteProxyTLS); ok {
		t.Fatal("a nil bundle reported a kill switch")
	}
}

func TestStoreUnchangedVersionIsIdempotent(t *testing.T) {
	v, priv := newKeyPair(t, "policy-key-1")
	store, _ := NewStore(v)
	raw, _ := Sign("policy-key-1", priv, testBundle("10"))
	if res := store.Apply(raw); res.Outcome != OutcomeAccepted {
		t.Fatalf("first = %q", res.Outcome)
	}
	res := store.Apply(raw)
	if res.Outcome != OutcomeUnchanged {
		t.Fatalf("re-applying the same bundle = %q, want unchanged (a 304 bumps nothing)", res.Outcome)
	}
	if store.InForce().Version != "10" {
		t.Fatal("version changed on an unchanged poll")
	}
}

func TestNewStoreRefusesToExistWithoutAVerifier(t *testing.T) {
	if _, err := NewStore(nil); err == nil {
		t.Fatal("a store without a verifier would make unverified bundles enforceable")
	}
}

// A server that states the tenant has no bundle (GET /v1/policy 404) takes the device to M0, and
// the version check starts again from nothing: the next bundle the tenant mints is accepted even
// though its version cannot be ordered against a bundle that no longer exists.
func TestStoreWithdrawFallsToM0(t *testing.T) {
	v, priv := newKeyPair(t, "policy-key-1")
	store, _ := NewStore(v)
	raw, _ := Sign("policy-key-1", priv, testBundle("10"))
	if res := store.Apply(raw); res.Outcome != OutcomeAccepted {
		t.Fatalf("apply = %+v", res)
	}
	res := store.Withdraw()
	if res.Outcome != OutcomeFellToM0 || res.Err != nil {
		t.Fatalf("withdraw = %+v, want fell_to_m0 with no error", res)
	}
	if store.InForce() != nil || store.InForceRaw() != nil {
		t.Fatal("a bundle is still in force after the server withdrew it")
	}
	again, _ := Sign("policy-key-1", priv, testBundle("1"))
	if res := store.Apply(again); res.Outcome != OutcomeAccepted {
		t.Fatalf("apply after withdraw = %+v", res)
	}
}
