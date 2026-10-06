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
	"github.com/shadow-ai-capture/control-api/internal/deviceca/devicecatest"
	"github.com/shadow-ai-capture/control-api/internal/enrol"
	"github.com/shadow-ai-capture/control-api/internal/intune"
	"github.com/shadow-ai-capture/control-api/internal/store"
	"github.com/shadow-ai-capture/control-api/internal/store/storetest"
)

const (
	entraTenant = "9b1c2d3e-4f50-4a6b-8c7d-0e1f2a3b4c5d"
	intuneA     = "0f6a2b1c-3d4e-4f5a-8b6c-7d8e9f0a1b2c"
	intuneB     = "1f6a2b1c-3d4e-4f5a-8b6c-7d8e9f0a1b2c"
	serialA     = "PF3ABC12"
)

// userRefKeys is a fixed key source.
type userRefKeys struct{ err error }

var fixedUserRefKey = bytes.Repeat([]byte{0x5a}, protocol.UserRefKeySize)

func (u userRefKeys) Key(context.Context, string) ([]byte, error) {
	return append([]byte(nil), fixedUserRefKey...), u.err
}

type graphTokens struct{}

func (graphTokens) Token(context.Context, string, string) (string, error) { return "t", nil }

// fakeIntune serves managed devices the way Microsoft Graph does, so the tests run the real
// GraphChecker end to end.
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

type rig struct {
	store  *storetest.Memory
	ca     *devicecatest.Authority
	svc    *enrol.Service
	now    time.Time
	intune *fakeIntune
}

func newRig(t *testing.T, mutate func(*enrol.Config)) *rig {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	st := storetest.New()
	st.AddTenant(activeTenant(tenantA, regionA))
	ca := devicecatest.New(t)
	fi := &fakeIntune{devices: map[string]string{}}
	srv := httptest.NewServer(fi)
	t.Cleanup(srv.Close)
	checker, err := intune.NewGraphChecker(graphTokens{}, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	checker.BaseURL = srv.URL
	cfg := enrol.Config{Region: regionA, Now: func() time.Time { return now }, Intune: checker, UserRefKeys: userRefKeys{}}
	if mutate != nil {
		mutate(&cfg)
	}
	svc, err := enrol.New(st, ca.CA, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return &rig{store: st, ca: ca, svc: svc, now: now, intune: fi}
}

func (r *rig) enrol(req protocol.EnrolmentRequest) (protocol.EnrolmentResponse, error) {
	return r.svc.Enrol(context.Background(), enrol.Input{Request: req})
}

// addKey mints a deployment key for the tenant and stores its hash, as the package download does.
func (r *rig) addKey(t *testing.T, tenantID string, mutate func(*store.DeploymentKey)) (string, store.DeploymentKey) {
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

// requireIntune sets device_verification 'intune' for tenantA with an active Entra connection.
func (r *rig) requireIntune() {
	r.store.AddIdentityConnection(tenantA, store.IdentityConnection{Provider: "entra", Status: "active", EntraTenantID: entraTenant})
	r.store.SetVerification(tenantA, store.VerificationIntune)
}

func attested(t *testing.T, key, hwid string, att *protocol.DeviceAttestation) protocol.EnrolmentRequest {
	req := request(t, key, hwid)
	req.Attestation = att
	return req
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

func TestDeploymentKeyEnrolsCountsAndAudits(t *testing.T) {
	r := newRig(t, nil)
	plaintext, k := r.addKey(t, tenantA, nil)
	resp, err := r.enrol(request(t, plaintext, "hw-key-1"))
	if err != nil {
		t.Fatal(err)
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
	r := newRig(t, nil)
	r.store.AddTenant(activeTenant(tenantB, regionA))
	revokedAt := r.now.Add(-time.Minute)
	expiredAt := r.now.Add(-time.Second)
	revoked, _ := r.addKey(t, tenantA, func(k *store.DeploymentKey) { k.RevokedAt = &revokedAt })
	expired, _ := r.addKey(t, tenantA, func(k *store.DeploymentKey) { k.ExpiresAt = &expiredAt })
	// A key minted for tenant B presented under tenant A's prefix: the lookup is scoped to the
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
		{"other prefix", "sac1." + tenantA + ".AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", apierr.CodeDeploymentKeyInvalid},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := r.enrol(request(t, c.key, "hw-"+c.name)); !isCode(err, c.code) {
				t.Fatalf("err = %v, want %s", err, c.code)
			}
		})
	}
	if len(r.store.Credentials()) != 0 || len(r.store.Devices()) != 0 {
		t.Fatal("a refused key wrote a device or a credential")
	}
}

func TestDeploymentKeyIsRateLimitedPerKey(t *testing.T) {
	r := newRig(t, func(c *enrol.Config) { c.KeyRate = 0.001; c.KeyBurst = 2 })
	key1, _ := r.addKey(t, tenantA, nil)
	key2, _ := r.addKey(t, tenantA, nil)
	for i, hw := range []string{"hw-r1", "hw-r2"} {
		if _, err := r.enrol(request(t, key1, hw)); err != nil {
			t.Fatalf("enrolment %d: %v", i, err)
		}
	}
	_, err := r.enrol(request(t, key1, "hw-r3"))
	var e *apierr.Error
	if !errors.As(err, &e) || e.Code != apierr.CodeRateLimited || e.Status != http.StatusTooManyRequests {
		t.Fatalf("third enrolment on one key: err = %v, want 429 %s", err, apierr.CodeRateLimited)
	}
	if _, err := r.enrol(request(t, key2, "hw-r4")); err != nil {
		t.Fatalf("another key of the same tenant was throttled: %v", err)
	}
}

func TestIntuneVerification(t *testing.T) {
	r := newRig(t, nil)
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
			_, err := r.enrol(attested(t, key, "hw-"+c.name, c.att))
			var e *apierr.Error
			if !errors.As(err, &e) || e.Code != apierr.CodeDeviceNotManaged || e.Status != http.StatusForbidden || detailReason(err) != c.reason {
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
	resp, err := r.enrol(attested(t, key, "hw-ok", att(strings.ToUpper(intuneA), strings.ToLower(serialA), "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d")))
	if err != nil {
		t.Fatalf("a managed device was refused: %v", err)
	}
	if d, err := r.store.FindDeviceByIntuneID(context.Background(), tenantA, intuneA); err != nil || d.DeviceID != resp.DeviceID {
		t.Fatalf("intune binding = %+v (%v), want device %s", d, err, resp.DeviceID)
	}
}

// One Intune device is one product device: a second enrolment naming it returns the same device
// even with another hardware hash, and a re-imaged machine (same hardware, new Intune id) keeps its
// device and takes the new binding.
func TestIntuneDeviceIsOneProductDevice(t *testing.T) {
	r := newRig(t, nil)
	r.requireIntune()
	key, _ := r.addKey(t, tenantA, nil)
	r.intune.devices[intuneA] = managedDevice(intuneA, serialA, "", "managed")
	r.intune.devices[intuneB] = managedDevice(intuneB, serialA, "", "managed")
	att := func(id string) *protocol.DeviceAttestation {
		return &protocol.DeviceAttestation{IntuneDeviceID: id, SerialNumber: serialA}
	}
	first, err := r.enrol(attested(t, key, "hw-original", att(intuneA)))
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.enrol(attested(t, key, "hw-different", att(intuneA)))
	if err != nil {
		t.Fatal(err)
	}
	if second.DeviceID != first.DeviceID || !second.Reenrolled || len(r.store.Devices()) != 1 {
		t.Fatalf("same Intune device returned %s (reenrolled %v), want %s", second.DeviceID, second.Reenrolled, first.DeviceID)
	}
	if d, _ := r.store.Device(context.Background(), tenantA, first.DeviceID); d.HardwareIdentityHash != "hw-original" {
		t.Fatalf("hardware identity = %q, want hw-original", d.HardwareIdentityHash)
	}
	reimaged, err := r.enrol(attested(t, key, "hw-original", att(intuneB)))
	if err != nil {
		t.Fatal(err)
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
	r.store.RevokeDevice(tenantA, first.DeviceID, r.now)
	if _, err := r.enrol(attested(t, key, "hw-new", att(intuneB))); !isCode(err, apierr.CodeRevokedDevice) {
		t.Fatalf("revoked device via its Intune id: err = %v", err)
	}
}

// Graph refusing the app, a tenant with no active Entra connection, or no checker at all is the
// service's problem: 503, never a pass.
func TestIntuneServiceFailuresAreRetryable(t *testing.T) {
	att := &protocol.DeviceAttestation{IntuneDeviceID: intuneA, SerialNumber: serialA}

	graphDown := newRig(t, nil)
	graphDown.requireIntune()
	graphDown.intune.status = http.StatusForbidden
	noConn := newRig(t, nil)
	noConn.store.SetVerification(tenantA, store.VerificationIntune)
	noChecker := newRig(t, func(c *enrol.Config) { c.Intune = nil })
	noChecker.requireIntune()

	for name, r := range map[string]*rig{"graph 403": graphDown, "no entra connection": noConn, "no checker": noChecker} {
		key, _ := r.addKey(t, tenantA, nil)
		if _, err := r.enrol(attested(t, key, "hw", att)); !isCode(err, apierr.CodeUnavailable) {
			t.Errorf("%s: err = %v, want %s", name, err, apierr.CodeUnavailable)
		}
		if len(r.store.Devices()) != 0 {
			t.Errorf("%s: a device was written although the check could not run", name)
		}
	}
}

func TestNoneVerificationDoesNotAskIntune(t *testing.T) {
	r := newRig(t, nil)
	key, _ := r.addKey(t, tenantA, nil)
	if _, err := r.enrol(attested(t, key, "hw-none", &protocol.DeviceAttestation{IntuneDeviceID: intuneA})); err != nil {
		t.Fatal(err)
	}
	if r.intune.calls != 0 {
		t.Fatalf("Graph was called %d times for a key-only tenant", r.intune.calls)
	}
}

func TestAFailingUserRefKeySourceIssuesNothing(t *testing.T) {
	r := newRig(t, func(c *enrol.Config) { c.UserRefKeys = userRefKeys{err: errors.New("down")} })
	key, _ := r.addKey(t, tenantA, nil)
	if _, err := r.enrol(request(t, key, "hw-nokey")); !isCode(err, apierr.CodeUnavailable) {
		t.Fatalf("err = %v, want %s", err, apierr.CodeUnavailable)
	}
	if len(r.store.Credentials()) != 0 {
		t.Fatal("a certificate was issued without the user_ref key")
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
