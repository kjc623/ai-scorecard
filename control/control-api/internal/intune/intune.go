// Package intune is the Intune managed-device check a deployment-key enrolment passes when its
// tenant sets device_verification='intune' (contract §5).
//
// A deployment key alone is a shared secret: every device a customer's package reaches holds it, so
// anyone who copies the package can enrol. For a tenant whose devices are managed by Intune, the
// device also states the identifiers its MDM enrolment gave it (protocol.DeviceAttestation), and
// this package looks the device up in the customer's own Intune, through Microsoft Graph, with the
// only application permission the product asks for: DeviceManagementManagedDevices.Read.All.
//
// What it establishes, stated plainly: that a managed device with this id exists in the customer's
// tenant, is managed, has this serial number and (when both sides know it) this Entra device id.
// It does not prove the caller IS that device; the identifiers are not secrets. The check turns "has
// the package" into "has the package and names a real, managed device of this customer", and the
// one-Intune-device-one-product-device binding in enrol stops a copied package from minting a
// second identity behind a real device's identifiers.
package intune

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/shadow-ai-capture/device/protocol"
)

// GraphScope is the scope an app-only Graph token is requested for.
const GraphScope = "https://graph.microsoft.com/.default"

// DefaultGraphBaseURL is Microsoft Graph's global endpoint.
const DefaultGraphBaseURL = "https://graph.microsoft.com"

// TokenSource obtains an app-only access token for the multi-tenant app in a customer's Entra
// tenant. The identity side implements it (client secret, certificate or managed-identity federated
// credential); this package only consumes it, so it is declared here rather than imported.
type TokenSource interface {
	Token(ctx context.Context, customerTenantID, scope string) (string, error)
}

// Refusal reasons: the closed set enrol reports as detail.reason of device_not_managed.
const (
	ReasonAttestationMissing  = "attestation_missing"
	ReasonAttestationInvalid  = "attestation_invalid"
	ReasonNotFound            = "not_found"
	ReasonNotManaged          = "not_managed"
	ReasonSerialMismatch      = "serial_mismatch"
	ReasonEntraDeviceMismatch = "entra_device_mismatch"
)

// Refusal is a device Intune does not vouch for. It is the device's problem, reported to it; every
// other error from Check is the service's problem (Graph unreachable, consent missing) and is
// retryable.
type Refusal struct {
	Reason string
}

func (r *Refusal) Error() string { return "intune: device refused: " + r.Reason }

// IsRefusal reports whether err is a Refusal, and returns it.
func IsRefusal(err error) (*Refusal, bool) {
	var r *Refusal
	if errors.As(err, &r) {
		return r, true
	}
	return nil, false
}

// ManagedDevice is the slice of Graph's managedDevice the check reads.
type ManagedDevice struct {
	ID              string `json:"id"`
	SerialNumber    string `json:"serialNumber"`
	AzureADDeviceID string `json:"azureADDeviceId"`
	ManagementState string `json:"managementState"`
	DeviceName      string `json:"deviceName"`
}

// Checker verifies a device's attestation against the customer's Intune.
type Checker interface {
	// Check returns the verified managed device, a *Refusal, or a retryable error.
	Check(ctx context.Context, entraTenantID string, att *protocol.DeviceAttestation) (ManagedDevice, error)
}

// GraphChecker is the Checker that asks Microsoft Graph.
type GraphChecker struct {
	Tokens  TokenSource
	HTTP    *http.Client
	BaseURL string
}

// NewGraphChecker builds the Graph checker. A nil HTTP client gets a bounded default: an enrolment
// waits on this call, so it must not hang on a slow Graph.
func NewGraphChecker(tokens TokenSource, client *http.Client) (*GraphChecker, error) {
	if tokens == nil {
		return nil, errors.New("intune: a token source is required")
	}
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	return &GraphChecker{Tokens: tokens, HTTP: client, BaseURL: DefaultGraphBaseURL}, nil
}

// Validate applies the checks that need no network: the attestation names an Intune device id in
// Graph's GUID shape. It runs before a token is requested, so a device that sends nothing costs
// nothing.
func Validate(att *protocol.DeviceAttestation) (string, error) {
	if att == nil || strings.TrimSpace(att.IntuneDeviceID) == "" {
		return "", &Refusal{Reason: ReasonAttestationMissing}
	}
	id := strings.ToLower(strings.Trim(strings.TrimSpace(att.IntuneDeviceID), "{}"))
	if !isGUID(id) {
		// An id that is not a GUID is never a managed device, and refusing it here also keeps any
		// other shape out of the Graph URL path.
		return "", &Refusal{Reason: ReasonAttestationInvalid}
	}
	return id, nil
}

// Check implements Checker.
func (g *GraphChecker) Check(ctx context.Context, entraTenantID string, att *protocol.DeviceAttestation) (ManagedDevice, error) {
	id, err := Validate(att)
	if err != nil {
		return ManagedDevice{}, err
	}
	if !isGUID(strings.ToLower(entraTenantID)) {
		return ManagedDevice{}, fmt.Errorf("intune: the tenant's Entra tenant id %q is not a GUID", entraTenantID)
	}
	token, err := g.Tokens.Token(ctx, entraTenantID, GraphScope)
	if err != nil {
		return ManagedDevice{}, fmt.Errorf("intune: graph token: %w", err)
	}
	base := strings.TrimRight(g.BaseURL, "/")
	if base == "" {
		base = DefaultGraphBaseURL
	}
	q := url.Values{"$select": {"id,serialNumber,azureADDeviceId,managementState,deviceName"}}
	endpoint := base + "/v1.0/deviceManagement/managedDevices/" + url.PathEscape(id) + "?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return ManagedDevice{}, fmt.Errorf("intune: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	resp, err := g.HTTP.Do(req)
	if err != nil {
		return ManagedDevice{}, fmt.Errorf("intune: graph request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return ManagedDevice{}, fmt.Errorf("intune: read graph response: %w", err)
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return ManagedDevice{}, &Refusal{Reason: ReasonNotFound}
	case resp.StatusCode == http.StatusBadRequest:
		// Graph answers 400 for an id it cannot resolve as a managed device; to the caller that is
		// the same fact as 404.
		return ManagedDevice{}, &Refusal{Reason: ReasonNotFound}
	case resp.StatusCode != http.StatusOK:
		// 401/403 mean consent or the permission is missing in the customer's tenant, 429/5xx mean
		// Graph is busy: the service's problem in every case, never the device's, so it is retryable
		// and logged with Graph's error code but not its body.
		return ManagedDevice{}, fmt.Errorf("intune: graph answered %d (%s)", resp.StatusCode, graphErrorCode(body))
	}
	var d ManagedDevice
	if err := json.Unmarshal(body, &d); err != nil {
		return ManagedDevice{}, fmt.Errorf("intune: graph response is not a managedDevice: %w", err)
	}
	if err := Decide(id, d, att); err != nil {
		return ManagedDevice{}, err
	}
	return d, nil
}

// Decide applies the contract's checks to a managed device Graph returned. It is separate from
// Check so the rules are testable without a network.
func Decide(id string, d ManagedDevice, att *protocol.DeviceAttestation) error {
	if !strings.EqualFold(strings.Trim(d.ID, "{}"), id) {
		return &Refusal{Reason: ReasonNotFound}
	}
	if !strings.EqualFold(d.ManagementState, "managed") {
		return &Refusal{Reason: ReasonNotManaged}
	}
	serial := strings.TrimSpace(att.SerialNumber)
	if serial == "" || !strings.EqualFold(serial, strings.TrimSpace(d.SerialNumber)) {
		return &Refusal{Reason: ReasonSerialMismatch}
	}
	entra := strings.Trim(strings.TrimSpace(att.EntraDeviceID), "{}")
	graphEntra := strings.Trim(strings.TrimSpace(d.AzureADDeviceID), "{}")
	if entra != "" && graphEntra != "" && graphEntra != zeroGUID && !strings.EqualFold(entra, graphEntra) {
		return &Refusal{Reason: ReasonEntraDeviceMismatch}
	}
	return nil
}

// zeroGUID is what Graph reports as azureADDeviceId for a device with no Entra registration; it is
// an absent value, not one to compare against.
const zeroGUID = "00000000-0000-0000-0000-000000000000"

// graphErrorCode extracts Graph's machine-readable error code, the only part of an error body safe
// and useful to log.
func graphErrorCode(body []byte) string {
	var e struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil && e.Error.Code != "" {
		return e.Error.Code
	}
	return "no error code"
}

func isGUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range []byte(s) {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
				return false
			}
		}
	}
	return true
}
