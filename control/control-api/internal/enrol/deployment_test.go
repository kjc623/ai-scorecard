package enrol_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/protocol"

	"github.com/shadow-ai-capture/control-api/internal/apierr"
	"github.com/shadow-ai-capture/control-api/internal/enrol"
	"github.com/shadow-ai-capture/control-api/internal/intune"
	"github.com/shadow-ai-capture/control-api/internal/signer"
	"github.com/shadow-ai-capture/control-api/internal/store"
)

const (
	entraTenant = "9b1c2d3e-4f50-4a6b-8c7d-0e1f2a3b4c5d"
	intuneA     = "0f6a2b1c-3d4e-4f5a-8b6c-7d8e9f0a1b2c"
	intuneB     = "1f6a2b1c-3d4e-4f5a-8b6c-7d8e9f0a1b2c"
	serialA     = "PF3ABC12"
)

// userRefKeys is a fixed key source, standing in for directory.UserRefKeys.
type userRefKeys struct{ err error }

var fixedUserRefKey = bytes.Repeat([]byte{0x5a}, protocol.UserRefKeySize)

func (u userRefKeys) Key(context.Context, string) ([]byte, error) {
	return append([]byte(nil), fixedUserRefKey...), u.err
}

type graphTokens struct{}

func (graphTokens) Token(context.Context, string, string) (string, error) { return "t", nil }

// fakeIntune is the customer's Intune: a map of managed devices served the way Graph serves them,
// so the enrolment tests exercise the real GraphChecker end to end.
type fakeIntune struct {
	devices map[string]string // intune id -> managedDevice JSON
	status  int               // when set, every request answers this status
	calls   int
}

func (f *fakeIntune) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.calls++
	if f.status != 0 {
		w.WriteHeader(f.status)
		return
	}
	id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	body, ok := f.devices[id]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"ResourceNotFound"}}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(body))
}

func managedDevice(id, serial, entra, state string) string {
	return `{"id":"` + id + `","serialNumber":"` + serial + `","azureADDeviceId":"` + entra + `","managementState":"` + state + `"}`
}

type deployRig struct {
	store  *store.Memory
	svc    *enrol.Service
	now    time.Time
	intune *fakeIntune
}

func newDeployRig(t *testing.T, mutate func(*enrol.Config)) *deployRig {
	t.Helper()
	now := time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)
	st := store.NewMemory()
	st.AddTenant(activeTenant(tenantA, regionA))
	st.AddTenant(activeTenant(tenantB, regionA))
	ca, err := signer.NewLocalCA(nil, nil, 90*24*time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	fi := &fakeIntune{devices: map[string]string{}}
	srv := httptest.NewServer(fi)
	t.Cleanup(srv.Close)
	checker, err := intune.NewGraphChecker(graphTokens{}, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	checker.BaseURL = srv.URL
	cfg := enrol.Config{
		Region: regionA, CredentialTTL: 90 * 24 * time.Hour, Now: func() time.Time { return now },
		Deployment: st, Intune: checker, UserRefKeys: userRefKeys{},
	}
	if mutate != nil {
		mutate(&cfg)
	}
	svc, err := enrol.New(st, ca, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return &deployRig{store: st, svc: svc, now: now, intune: fi}
}

// addKey mints a deployment key for tenant and stores its hash, as the admin package route does.
func (r *deployRig) addKey(t *testing.T, tenantID string, mutate func(*store.DeploymentKey)) (string, store.DeploymentKey) {
	t.Helper()
	plaintext, err := enrol.MintDeploymentKey(tenantID)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := store.NewUUID()
	k := store.DeploymentKey{
		KeyID: id, TenantID: tenantID, KeyHash: enrol.HashDeploymentKey(plaintext),
		Label: "test", CreatedBy: "admin@example.com", CreatedAt: r.now.Add(-time.Hour),
	}
	if mutate != nil {
		mutate(&k)
	}
	r.store.AddDeploymentKey(k)
	return plaintext, k
}

// requireIntune turns on device_verification='intune' for tenantA with an active Entra connection.
func (r *deployRig) requireIntune() {
	r.store.AddIdentityConnection(tenantA, store.IdentityConnection{Provider: "entra", Status: "active", EntraTenantID: entraTenant})
	r.store.SetVerification(tenantA, store.VerificationIntune)
}

func keyRequest(t *testing.T, key, hwid string, att *protocol.DeviceAttestation) protocol.EnrolmentRequest {
	t.Helper()
	req, _, err := x509Request("", hwid)
	if err != nil {
		t.Fatal(err)
	}
	req.DeploymentKey = key
	req.Attestation = att
	return req
}

func (r *deployRig) enrol(req protocol.EnrolmentRequest) (protocol.EnrolmentResponse, error) {
	return r.svc.Enrol(context.Background(), enrol.Input{Request: req, HTM: "POST", HTU: htu})
}

func detailReason(err error) string {
	var e *apierr.Error
	if !errors.As(err, &e) {
		return ""
	}
	d, _ := e.Detail.(map[string]any)
	s, _ := d["reason"].(string)
	return s
}

// TestDeploymentKeyEnrolsCountsAndAudits: a valid key enrols a device, the response carries the
// tenant's user_ref_key, the key's enrolment_count and last_used_at move, and the enrolment is
// audited with the device as actor and no key material in the row.
func TestDeploymentKeyEnrolsCountsAndAudits(t *testing.T) {
	r := newDeployRig(t, nil)
	plaintext, k := r.addKey(t, tenantA, nil)
	resp, err := r.enrol(keyRequest(t, plaintext, "hw-key-1", nil))
	if err != nil {
		t.Fatalf("Enrol: %v", err)
	}
	if resp.TenantID != tenantA || resp.DeviceID == "" || resp.Reenrolled {
		t.Fatalf("response = %+v", resp)
	}
	key, err := protocol.DecodeUserRefKey(resp.UserRefKey)
	if err != nil || !bytes.Equal(key, fixedUserRefKey) {
		t.Fatalf("user_ref_key = %q (%v), want the tenant's key", resp.UserRefKey, err)
	}
	keys := r.store.DeploymentKeys(tenantA)
	if len(keys) != 1 || keys[0].EnrolmentCount != 1 || keys[0].LastUsedAt == nil || !keys[0].LastUsedAt.Equal(r.now) {
		t.Fatalf("key after enrolment = %+v", keys)
	}
	audits := r.store.Audits()
	if len(audits) != 1 {
		t.Fatalf("audits = %d, want 1", len(audits))
	}
	a := audits[0]
	if a.Action != "device.enrol" || a.ActorType != store.ActorDevice || a.ActorID != resp.DeviceID || a.Detail["key_id"] != k.KeyID {
		t.Fatalf("audit = %+v", a)
	}
	for _, v := range a.Detail {
		if s, ok := v.(string); ok && (strings.Contains(s, plaintext) || strings.HasPrefix(s, "sha256:")) {
			t.Fatalf("audit detail carries key material: %v", a.Detail)
		}
	}
}

func TestDeploymentKeyRefusals(t *testing.T) {
	r := newDeployRig(t, nil)
	revokedAt := r.now.Add(-time.Minute)
	expiredAt := r.now.Add(-time.Second)
	revoked, _ := r.addKey(t, tenantA, func(k *store.DeploymentKey) { k.RevokedAt = &revokedAt })
	expired, _ := r.addKey(t, tenantA, func(k *store.DeploymentKey) { k.ExpiresAt = &expiredAt })
	// A key minted for tenant B but presented with tenant A's prefix: the lookup is scoped to the
	// tenant the key names, so it is simply not A's key.
	foreign, _ := r.addKey(t, tenantB, nil)
	wrongTenant := strings.Replace(foreign, tenantB, tenantA, 1)
	unknown, _ := enrol.MintDeploymentKey(tenantA)

	for _, c := range []struct {
		name, key, code string
	}{
		{"revoked", revoked, apierr.CodeDeploymentKeyRevoked},
		{"expired", expired, apierr.CodeDeploymentKeyExpired},
		{"wrong tenant", wrongTenant, apierr.CodeDeploymentKeyInvalid},
		{"unknown", unknown, apierr.CodeDeploymentKeyInvalid},
		{"malformed", "sacdk_not-a-key", apierr.CodeDeploymentKeyInvalid},
		{"enrolment-token spelling", "sac1." + tenantA + ".AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", apierr.CodeDeploymentKeyInvalid},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := r.enrol(keyRequest(t, c.key, "hw-"+c.name, nil))
			if !isCode(err, c.code) {
				t.Fatalf("err = %v, want %s", err, c.code)
			}
		})
	}
	if len(r.store.Credentials()) != 0 || len(r.store.Devices()) != 0 {
		t.Fatal("a refused key wrote a device or a credential")
	}
}

func TestDeploymentKeyIsRateLimitedPerKey(t *testing.T) {
	r := newDeployRig(t, func(c *enrol.Config) { c.KeyRate = 0.001; c.KeyBurst = 2 })
	key1, _ := r.addKey(t, tenantA, nil)
	key2, _ := r.addKey(t, tenantA, nil)
	for i, hw := range []string{"hw-r1", "hw-r2"} {
		if _, err := r.enrol(keyRequest(t, key1, hw, nil)); err != nil {
			t.Fatalf("enrolment %d: %v", i, err)
		}
	}
	_, err := r.enrol(keyRequest(t, key1, "hw-r3", nil))
	var e *apierr.Error
	if !errors.As(err, &e) || e.Code != apierr.CodeRateLimited || e.Status != 429 {
		t.Fatalf("third enrolment on one key: err = %v, want 429 %s", err, apierr.CodeRateLimited)
	}
	// The limit is per key: another key of the same tenant is not throttled by the first.
	if _, err := r.enrol(keyRequest(t, key2, "hw-r4", nil)); err != nil {
		t.Fatalf("second key: %v", err)
	}
}

func TestDeploymentKeyNeedsTheDeploymentStoreAndOneBootstrap(t *testing.T) {
	r := newDeployRig(t, func(c *enrol.Config) { c.Deployment = nil })
	key, _ := r.addKey(t, tenantA, nil)
	if _, err := r.enrol(keyRequest(t, key, "hw-nostore", nil)); !isCode(err, apierr.CodeDeploymentKeyInvalid) {
		t.Fatalf("no deployment store: err = %v, want %s", err, apierr.CodeDeploymentKeyInvalid)
	}
	both := keyRequest(t, key, "hw-both", nil)
	both.EnrolmentToken = "sac1.x.y"
	if _, err := r.enrol(both); !isCode(err, apierr.CodeSchemaViolation) {
		t.Fatalf("token and key together: err = %v, want %s", err, apierr.CodeSchemaViolation)
	}
}

// TestIntuneVerification covers device_verification='intune': each of the contract's checks
// refuses with device_not_managed and its reason, and a refusal is audited.
func TestIntuneVerification(t *testing.T) {
	r := newDeployRig(t, nil)
	r.requireIntune()
	key, _ := r.addKey(t, tenantA, nil)
	r.intune.devices[intuneA] = managedDevice(intuneA, serialA, "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d", "managed")
	r.intune.devices[intuneB] = managedDevice(intuneB, "SERIAL-B", "", "wipePending")

	att := func(id, serial, entra string) *protocol.DeviceAttestation {
		return &protocol.DeviceAttestation{IntuneDeviceID: id, SerialNumber: serial, EntraDeviceID: entra}
	}
	for _, c := range []struct {
		name   string
		att    *protocol.DeviceAttestation
		reason string
	}{
		{"missing attestation", nil, intune.ReasonAttestationMissing},
		{"not found", att("2f6a2b1c-3d4e-4f5a-8b6c-7d8e9f0a1b2c", serialA, ""), intune.ReasonNotFound},
		{"unmanaged", att(intuneB, "SERIAL-B", ""), intune.ReasonNotManaged},
		{"serial mismatch", att(intuneA, "OTHER", ""), intune.ReasonSerialMismatch},
		{"entra device mismatch", att(intuneA, serialA, "11111111-2222-4333-8444-555555555555"), intune.ReasonEntraDeviceMismatch},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := r.enrol(keyRequest(t, key, "hw-"+c.name, c.att))
			var e *apierr.Error
			if !errors.As(err, &e) || e.Code != apierr.CodeDeviceNotManaged || e.Status != 403 || detailReason(err) != c.reason {
				t.Fatalf("err = %v (reason %q), want 403 %s reason %s", err, detailReason(err), apierr.CodeDeviceNotManaged, c.reason)
			}
		})
	}
	if len(r.store.Devices()) != 0 {
		t.Fatal("a refused device was written")
	}
	refusals := 0
	for _, a := range r.store.Audits() {
		if a.Action == "device.enrol_refused" {
			refusals++
		}
	}
	if refusals != 5 {
		t.Fatalf("refusal audits = %d, want 5", refusals)
	}

	resp, err := r.enrol(keyRequest(t, key, "hw-ok", att(strings.ToUpper(intuneA), strings.ToLower(serialA), "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d")))
	if err != nil {
		t.Fatalf("a managed device was refused: %v", err)
	}
	d, err := r.store.FindDeviceByIntuneID(context.Background(), tenantA, intuneA)
	if err != nil || d.DeviceID != resp.DeviceID {
		t.Fatalf("intune binding = %+v (%v), want device %s", d, err, resp.DeviceID)
	}
}

// TestIntuneDeviceIsOneProductDevice: a second enrolment naming the same Intune device returns the
// same product device even with a different hardware hash, and a re-imaged device (same hardware,
// new Intune id) keeps its product device and takes the new binding.
func TestIntuneDeviceIsOneProductDevice(t *testing.T) {
	r := newDeployRig(t, nil)
	r.requireIntune()
	key, _ := r.addKey(t, tenantA, nil)
	r.intune.devices[intuneA] = managedDevice(intuneA, serialA, "", "managed")
	r.intune.devices[intuneB] = managedDevice(intuneB, serialA, "", "managed")
	att := func(id string) *protocol.DeviceAttestation {
		return &protocol.DeviceAttestation{IntuneDeviceID: id, SerialNumber: serialA}
	}

	first, err := r.enrol(keyRequest(t, key, "hw-original", att(intuneA)))
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := r.enrol(keyRequest(t, key, "hw-different", att(intuneA)))
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if second.DeviceID != first.DeviceID || !second.Reenrolled {
		t.Fatalf("same Intune device returned %s (reenrolled %v), want %s", second.DeviceID, second.Reenrolled, first.DeviceID)
	}
	if n := len(r.store.Devices()); n != 1 {
		t.Fatalf("devices = %d, want 1", n)
	}
	// The stored hardware identity stands; the second request's hash is not adopted over it.
	if d, _ := r.store.Device(context.Background(), tenantA, first.DeviceID); d.HardwareIdentityHash != "hw-original" {
		t.Fatalf("hardware identity = %q, want hw-original", d.HardwareIdentityHash)
	}

	reimaged, err := r.enrol(keyRequest(t, key, "hw-original", att(intuneB)))
	if err != nil {
		t.Fatalf("re-imaged: %v", err)
	}
	if reimaged.DeviceID != first.DeviceID {
		t.Fatalf("re-imaged device got %s, want %s", reimaged.DeviceID, first.DeviceID)
	}
	if d, err := r.store.FindDeviceByIntuneID(context.Background(), tenantA, intuneB); err != nil || d.DeviceID != first.DeviceID {
		t.Fatalf("new Intune id not bound: %+v %v", d, err)
	}
	if _, err := r.store.FindDeviceByIntuneID(context.Background(), tenantA, intuneA); !errors.Is(err, store.ErrDeviceUnknown) {
		t.Fatalf("old Intune id still bound: %v", err)
	}

	// A revoked device is not handed back through its Intune id either.
	r.store.RevokeDevice(tenantA, first.DeviceID, r.now)
	if _, err := r.enrol(keyRequest(t, key, "hw-new", att(intuneB))); !isCode(err, apierr.CodeRevokedDevice) {
		t.Fatalf("revoked device via Intune id: err = %v, want %s", err, apierr.CodeRevokedDevice)
	}
}

// TestIntuneServiceFailuresAreRetryable: Graph refusing the app (no consent), a tenant with no
// active Entra connection, or no checker at all is the service's problem: 503, never a pass.
func TestIntuneServiceFailuresAreRetryable(t *testing.T) {
	att := &protocol.DeviceAttestation{IntuneDeviceID: intuneA, SerialNumber: serialA}

	r := newDeployRig(t, nil)
	r.requireIntune()
	key, _ := r.addKey(t, tenantA, nil)
	r.intune.status = http.StatusForbidden
	if _, err := r.enrol(keyRequest(t, key, "hw-503", att)); !isCode(err, apierr.CodeUnavailable) {
		t.Fatalf("graph 403: err = %v, want %s", err, apierr.CodeUnavailable)
	}

	noConn := newDeployRig(t, nil)
	noConn.store.SetVerification(tenantA, store.VerificationIntune)
	key2, _ := noConn.addKey(t, tenantA, nil)
	if _, err := noConn.enrol(keyRequest(t, key2, "hw-noconn", att)); !isCode(err, apierr.CodeUnavailable) {
		t.Fatalf("no entra connection: err = %v, want %s", err, apierr.CodeUnavailable)
	}

	noChecker := newDeployRig(t, func(c *enrol.Config) { c.Intune = nil })
	noChecker.requireIntune()
	key3, _ := noChecker.addKey(t, tenantA, nil)
	if _, err := noChecker.enrol(keyRequest(t, key3, "hw-nochecker", att)); !isCode(err, apierr.CodeUnavailable) {
		t.Fatalf("no checker: err = %v, want %s", err, apierr.CodeUnavailable)
	}
	for _, rr := range []*deployRig{r, noConn, noChecker} {
		if len(rr.store.Devices()) != 0 {
			t.Fatal("a device was written although the Intune check could not run")
		}
	}
}

// TestDPoPEnrolmentWithADeploymentKey is the package's default path (SAC_AUTH_MODE=dpop): the
// deployment key bootstraps, and the DPoP proof still has to prove possession of the key registered.
func TestDPoPEnrolmentWithADeploymentKey(t *testing.T) {
	r := newDeployRig(t, nil)
	key, _ := r.addKey(t, tenantA, nil)
	req, _, proof, err := dpopRequest("", "hw-dpop", r.now)
	if err != nil {
		t.Fatal(err)
	}
	req.DeploymentKey = key
	resp, err := r.svc.Enrol(context.Background(), enrol.Input{Request: req, Proof: proof, HTM: "POST", HTU: htu})
	if err != nil {
		t.Fatalf("Enrol: %v", err)
	}
	if resp.Credential.Mode != protocol.AuthModeDPoP || resp.Credential.JWK == nil || resp.UserRefKey == "" {
		t.Fatalf("response = %+v", resp)
	}
	again, _, _, err := dpopRequest("", "hw-dpop-2", r.now)
	if err != nil {
		t.Fatal(err)
	}
	again.DeploymentKey = key
	if _, err := r.svc.Enrol(context.Background(), enrol.Input{Request: again, HTM: "POST", HTU: htu}); !isCode(err, apierr.CodeInvalidProof) {
		t.Fatalf("no proof of possession: err = %v, want %s", err, apierr.CodeInvalidProof)
	}
}

// TestNoneVerificationIgnoresAttestation: a key-only tenant does not call Intune at all.
func TestNoneVerificationIgnoresAttestation(t *testing.T) {
	r := newDeployRig(t, nil)
	key, _ := r.addKey(t, tenantA, nil)
	if _, err := r.enrol(keyRequest(t, key, "hw-none", &protocol.DeviceAttestation{IntuneDeviceID: intuneA})); err != nil {
		t.Fatalf("Enrol: %v", err)
	}
	if r.intune.calls != 0 {
		t.Fatalf("Graph was called %d times for a key-only tenant", r.intune.calls)
	}
}

// TestUserRefKeyOnEveryBootstrap: the single-use token path carries the key too, and a key source
// that fails refuses the enrolment before any credential is issued.
func TestUserRefKeyOnEveryBootstrap(t *testing.T) {
	r := newDeployRig(t, nil)
	plaintext, err := enrol.MintEnrolmentToken(tenantA)
	if err != nil {
		t.Fatal(err)
	}
	r.store.AddEnrolmentToken(store.EnrolmentToken{TenantID: tenantA, TokenHash: enrol.HashEnrolmentToken(plaintext),
		IssuedAt: r.now, ExpiresAt: r.now.Add(time.Hour)})
	req, _, err := x509Request(plaintext, "hw-token")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := r.enrol(req)
	if err != nil {
		t.Fatalf("token enrolment: %v", err)
	}
	if resp.UserRefKey == "" {
		t.Fatal("token enrolment carried no user_ref_key")
	}

	failing := newDeployRig(t, func(c *enrol.Config) { c.UserRefKeys = userRefKeys{err: errors.New("vault down")} })
	key, _ := failing.addKey(t, tenantA, nil)
	if _, err := failing.enrol(keyRequest(t, key, "hw-nokey", nil)); !isCode(err, apierr.CodeUnavailable) {
		t.Fatalf("failing key source: err = %v, want %s", err, apierr.CodeUnavailable)
	}
	if len(failing.store.Credentials()) != 0 {
		t.Fatal("a credential was issued without the user_ref key")
	}
}

func TestDeploymentKeyFormat(t *testing.T) {
	k, err := enrol.MintDeploymentKey(tenantA)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(k, "sacdk_"+tenantA+".") {
		t.Fatalf("key %q does not carry the sacdk_ prefix and the tenant in the clear", k)
	}
	if got, err := enrol.ParseDeploymentKey(k); err != nil || got != tenantA {
		t.Fatalf("ParseDeploymentKey = %q, %v", got, err)
	}
	if h := enrol.HashDeploymentKey(k); !strings.HasPrefix(h, "sha256:") || len(h) != len("sha256:")+64 {
		t.Fatalf("hash %q is not sha256:<hex>", h)
	}
	if k2, _ := enrol.MintDeploymentKey(tenantA); k2 == k {
		t.Fatal("two minted keys are equal")
	}
	if _, err := enrol.ParseDeploymentKey("sacdk_" + tenantA + ".c2hvcnQ"); err == nil {
		t.Fatal("a short secret was accepted")
	}
}
