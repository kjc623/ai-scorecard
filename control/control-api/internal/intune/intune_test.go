package intune_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/shadow-ai-capture/device/protocol"

	"github.com/shadow-ai-capture/control-api/internal/intune"
)

const (
	customerTenant = "9b1c2d3e-4f50-4a6b-8c7d-0e1f2a3b4c5d"
	intuneID       = "0f6a2b1c-3d4e-4f5a-8b6c-7d8e9f0a1b2c"
	entraDeviceID  = "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d"
)

type tokens struct {
	tenant, scope string
	err           error
}

func (t *tokens) Token(_ context.Context, customerTenantID, scope string) (string, error) {
	t.tenant, t.scope = customerTenantID, scope
	return "graph-token", t.err
}

// graph is a stand-in for Microsoft Graph's managedDevices endpoint.
func graph(t *testing.T, status int, body string) (*httptest.Server, *http.Request) {
	t.Helper()
	var seen http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = *r.Clone(context.Background())
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func checker(t *testing.T, srv *httptest.Server, tok *tokens) *intune.GraphChecker {
	t.Helper()
	c, err := intune.NewGraphChecker(tok, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	c.BaseURL = srv.URL
	return c
}

func managed(serial, entra, state string) string {
	return `{"id":"` + intuneID + `","serialNumber":"` + serial + `","azureADDeviceId":"` + entra +
		`","managementState":"` + state + `","deviceName":"LAPTOP-1"}`
}

func reason(t *testing.T, err error) string {
	t.Helper()
	r, ok := intune.IsRefusal(err)
	if !ok {
		t.Fatalf("expected a refusal, got %v", err)
	}
	return r.Reason
}

// TestGraphCheckerAsksTheCustomersIntune pins the request: the customer's tenant and Graph's
// default scope for the token, the managed device by id with the contract's $select, and a bearer
// token.
func TestGraphCheckerAsksTheCustomersIntune(t *testing.T) {
	srv, seen := graph(t, 200, managed("PF3ABC12", entraDeviceID, "managed"))
	tok := &tokens{}
	d, err := checker(t, srv, tok).Check(context.Background(), customerTenant, &protocol.DeviceAttestation{
		IntuneDeviceID: "{" + strings.ToUpper(intuneID) + "}", EntraDeviceID: entraDeviceID, SerialNumber: "pf3abc12",
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if d.ID != intuneID || d.DeviceName != "LAPTOP-1" {
		t.Errorf("device = %+v", d)
	}
	if tok.tenant != customerTenant || tok.scope != intune.GraphScope {
		t.Errorf("token asked for tenant %q scope %q", tok.tenant, tok.scope)
	}
	if got, want := seen.URL.Path, "/v1.0/deviceManagement/managedDevices/"+intuneID; got != want {
		t.Errorf("path = %s, want %s", got, want)
	}
	if got := seen.URL.Query().Get("$select"); got != "id,serialNumber,azureADDeviceId,managementState,deviceName" {
		t.Errorf("$select = %q", got)
	}
	if got := seen.Header.Get("Authorization"); got != "Bearer graph-token" {
		t.Errorf("Authorization = %q", got)
	}
}

func TestGraphCheckerRefusals(t *testing.T) {
	att := func(serial, entra string) *protocol.DeviceAttestation {
		return &protocol.DeviceAttestation{IntuneDeviceID: intuneID, SerialNumber: serial, EntraDeviceID: entra}
	}
	cases := []struct {
		name   string
		status int
		body   string
		att    *protocol.DeviceAttestation
		want   string
	}{
		{"no attestation", 200, managed("S1", "", "managed"), nil, intune.ReasonAttestationMissing},
		{"no intune id", 200, managed("S1", "", "managed"), &protocol.DeviceAttestation{SerialNumber: "S1"}, intune.ReasonAttestationMissing},
		{"id is not a guid", 200, managed("S1", "", "managed"), &protocol.DeviceAttestation{IntuneDeviceID: "../../users", SerialNumber: "S1"}, intune.ReasonAttestationInvalid},
		{"not found", 404, `{"error":{"code":"ResourceNotFound"}}`, att("S1", ""), intune.ReasonNotFound},
		{"unmanaged", 200, managed("S1", "", "retirePending"), att("S1", ""), intune.ReasonNotManaged},
		{"serial mismatch", 200, managed("S1", "", "managed"), att("S2", ""), intune.ReasonSerialMismatch},
		{"no serial sent", 200, managed("S1", "", "managed"), att("", ""), intune.ReasonSerialMismatch},
		{"entra device mismatch", 200, managed("S1", entraDeviceID, "managed"), att("S1", "11111111-2222-4333-8444-555555555555"), intune.ReasonEntraDeviceMismatch},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, _ := graph(t, c.status, c.body)
			_, err := checker(t, srv, &tokens{}).Check(context.Background(), customerTenant, c.att)
			if got := reason(t, err); got != c.want {
				t.Errorf("reason = %s, want %s", got, c.want)
			}
		})
	}
}

// TestEntraDeviceIDIsComparedOnlyWhenBothSidesKnowIt: a device with no Entra join, or Graph's zero
// GUID for one, is not refused for the absence.
func TestEntraDeviceIDIsComparedOnlyWhenBothSidesKnowIt(t *testing.T) {
	for _, c := range []struct{ graphEntra, deviceEntra string }{
		{"", entraDeviceID},
		{"00000000-0000-0000-0000-000000000000", entraDeviceID},
		{entraDeviceID, ""},
		{strings.ToUpper(entraDeviceID), entraDeviceID},
	} {
		srv, _ := graph(t, 200, managed("S1", c.graphEntra, "managed"))
		if _, err := checker(t, srv, &tokens{}).Check(context.Background(), customerTenant,
			&protocol.DeviceAttestation{IntuneDeviceID: intuneID, SerialNumber: "S1", EntraDeviceID: c.deviceEntra}); err != nil {
			t.Errorf("graph %q device %q: %v", c.graphEntra, c.deviceEntra, err)
		}
	}
}

// TestServiceFailuresAreNotRefusals: consent missing, Graph throttling or a token failure are the
// service's problem, never reported to the device as "not managed".
func TestServiceFailuresAreNotRefusals(t *testing.T) {
	for _, status := range []int{401, 403, 429, 500, 503} {
		srv, _ := graph(t, status, `{"error":{"code":"Authorization_RequestDenied","message":"secret detail"}}`)
		_, err := checker(t, srv, &tokens{}).Check(context.Background(), customerTenant,
			&protocol.DeviceAttestation{IntuneDeviceID: intuneID, SerialNumber: "S1"})
		if err == nil {
			t.Fatalf("status %d: no error", status)
		}
		if _, ok := intune.IsRefusal(err); ok {
			t.Errorf("status %d was reported as a device refusal", status)
		}
		if strings.Contains(err.Error(), "secret detail") {
			t.Errorf("status %d: Graph's error message leaked into the error: %v", status, err)
		}
	}
	srv, _ := graph(t, 200, managed("S1", "", "managed"))
	_, err := checker(t, srv, &tokens{err: errors.New("no consent")}).Check(context.Background(), customerTenant,
		&protocol.DeviceAttestation{IntuneDeviceID: intuneID, SerialNumber: "S1"})
	if _, ok := intune.IsRefusal(err); err == nil || ok {
		t.Errorf("a token failure must be a retryable error, got %v", err)
	}
}
