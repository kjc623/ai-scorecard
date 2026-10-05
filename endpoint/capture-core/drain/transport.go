package drain

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/credential"
	"github.com/shadow-ai-capture/device/capture-core/dpop"
	"github.com/shadow-ai-capture/device/protocol"
)

// maxResponseBytes bounds how much of a response the drain reads. A 200 events response carries one
// result per event; 4 MiB covers a full 500-event batch with margin.
const maxResponseBytes = 4 << 20

// client is the device's HTTPS transport to the edge, built once with the pinned CA set and
// minimum TLS 1.3 (docs/02 §2.1: TLS 1.3 minimum, TLS 1.2 refused).
type client struct {
	base   string // scheme://host[:port], no trailing slash
	caPool *x509.CertPool
	http   *http.Client
}

// newClient builds the transport. When caFile is empty the system root set is used; when it names a
// file, only that CA set is trusted, which is how the device pins the edge's issuing CA.
func newClient(base, caFile string) (*client, error) {
	u := strings.TrimRight(strings.TrimSpace(base), "/")
	if u == "" {
		return nil, errors.New("drain: endpoint is empty")
	}
	if !strings.HasPrefix(u, "https://") {
		return nil, fmt.Errorf("drain: endpoint %q must use https", base)
	}
	pool := x509.NewCertPool()
	if strings.TrimSpace(caFile) != "" {
		pemBytes, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("drain: reading CA file: %w", err)
		}
		if !pool.AppendCertsFromPEM(pemBytes) {
			return nil, fmt.Errorf("drain: CA file %q carries no certificates", caFile)
		}
	} else if sys, err := x509.SystemCertPool(); err == nil && sys != nil {
		pool = sys
	}
	return &client{
		base:   u,
		caPool: pool,
		http: &http.Client{
			Timeout: 20 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: pool},
			},
		},
	}, nil
}

// withCert returns an x509 client presenting the issued leaf.
func (c *client) withCert(pair tls.Certificate) *http.Client {
	return &http.Client{
		Timeout: 20 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				MinVersion:   tls.VersionTLS13,
				RootCAs:      c.caPool,
				Certificates: []tls.Certificate{pair},
				GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
					return &pair, nil
				},
			},
		},
	}
}

// post sends a JSON POST to path. headers carry the auth headers; contentType/contentEncoding
// describe the body. It returns the status and the raw response body; a transport-level error (a
// connection failure or timeout) is returned as err for the caller to classify as retryable.
func (c *client) post(ctx context.Context, path string, body []byte, contentType, contentEncoding string, headers map[string]string, hc *http.Client) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", contentType)
	if contentEncoding != "" {
		req.Header.Set("Content-Encoding", contentEncoding)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, b, nil
}

// get sends a GET to path and returns the response with its body read (bounded), so a caller can
// read the status, the headers and the body together. A transport-level error is returned as err.
func (c *client) get(ctx context.Context, path string, headers map[string]string, hc *http.Client) (*http.Response, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return nil, nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return resp, nil, err
	}
	return resp, b, nil
}

// apiError is a parsed §5 error envelope, a transport-level failure, or a synthetic failure for a
// status with no envelope.
type apiError struct {
	status      int
	code        protocol.ReasonCode
	retryAfterS int
	transport   bool
}

func (e *apiError) Error() string {
	if e == nil {
		return "drain: unknown error"
	}
	if e.transport {
		return "drain: transport error"
	}
	return fmt.Sprintf("drain: status %d code %q", e.status, e.code)
}

// Retryable reports whether the device should retry the whole batch after this failure (§8: 429,
// 503, timeouts and connection failures; duplicate_batch at batch level). Every reason code in §7 is
// terminal for the event except duplicate_batch.
func (e *apiError) Retryable() bool {
	if e == nil {
		return false
	}
	if e.transport {
		return true
	}
	switch {
	case e.status == http.StatusTooManyRequests, e.status == http.StatusServiceUnavailable:
		return true
	case e.status == http.StatusConflict && e.code == protocol.ReasonDuplicateBatch:
		return true
	default:
		return false
	}
}

// errorEnvelope is §5's common error body, parsed with the protocol reason code so the drain never
// invents an outcome the write path did not make.
type errorEnvelope struct {
	Error struct {
		Code        protocol.ReasonCode            `json:"code"`
		Detail      *protocol.BatchRejectionDetail `json:"detail,omitempty"`
		ServerTime  time.Time                      `json:"server_time"`
		RetryAfterS int                            `json:"retry_after_s,omitempty"`
		Message     string                         `json:"message,omitempty"`
	} `json:"error"`
}

func classify(status int, body []byte) *apiError {
	e := &apiError{status: status}
	var env errorEnvelope
	if json.Unmarshal(body, &env) == nil && env.Error.Code != "" {
		e.code = env.Error.Code
		e.retryAfterS = env.Error.RetryAfterS
	}
	return e
}

// enrol performs POST /v1/enrol for the configured mode and returns the issued credential with the
// freshly generated, never-exported device key. x509 submits a PKCS#10 CSR; dpop submits the public
// JWK plus a proof of possession.
func (d *Drainer) enrol(ctx context.Context, hwid string) (*credential.Credential, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("drain: generating device key: %w", err)
	}

	// The attestation is read at each enrolment rather than once at start: an MDM enrolment or a
	// join that completes after the agent started is then stated on the next attempt.
	var attestation *protocol.DeviceAttestation
	if d.cfg.Attestation != nil {
		attestation = d.cfg.Attestation()
	}
	mdmID := d.cfg.MDMID
	if mdmID == "" && attestation != nil {
		mdmID = attestation.IntuneDeviceID
	}
	body := protocol.EnrolmentRequest{
		SchemaVersion:  protocol.EnrolmentSchemaVersion,
		EnrolmentToken: d.cfg.EnrolmentToken,
		DeploymentKey:  d.cfg.DeploymentKey,
		Mode:           d.cfg.AuthMode,
		Device: protocol.DeviceInfo{
			OS:                   enrolmentOS(runtime.GOOS),
			AgentVersion:         d.cfg.AgentVersion,
			Hostname:             d.cfg.Hostname,
			HostnameHash:         d.cfg.HostnameHash,
			ManagedState:         d.cfg.ManagedState,
			MDMID:                mdmID,
			HardwareIdentityHash: hwid,
		},
		Attestation: attestation,
	}

	headers := map[string]string{}
	hc := d.client.http // bootstrap: no client certificate yet; the enrolment token is the credential
	switch d.cfg.AuthMode {
	case protocol.AuthModeX509:
		csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
			Subject: pkix.Name{CommonName: d.cfg.DeviceID},
		}, key)
		if err != nil {
			return nil, fmt.Errorf("drain: building CSR: %w", err)
		}
		body.CSR = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER}))
	case protocol.AuthModeDPoP:
		jwk := dpop.JWKFromPublic(&key.PublicKey)
		body.JWK = &jwk
		proof, err := dpop.DPoPProof(key, http.MethodPost, d.client.base+"/v1/enrol", "")
		if err != nil {
			return nil, err
		}
		headers[protocol.HeaderDPoP] = proof
	default:
		return nil, fmt.Errorf("drain: auth mode %q is not implemented", d.cfg.AuthMode)
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("drain: encoding enrol request: %w", err)
	}
	status, respBody, err := d.client.post(ctx, "/v1/enrol", raw, "application/json", "", headers, hc)
	if err != nil {
		return nil, &apiError{transport: true}
	}
	if status != http.StatusOK {
		return nil, classify(status, respBody)
	}
	var resp protocol.EnrolmentResponse
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return nil, fmt.Errorf("drain: decoding enrol response: %w", err)
	}
	if resp.DeviceID == "" || !resp.Credential.Mode.Valid() {
		return nil, errors.New("drain: enrol response carries no usable credential")
	}

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("drain: marshalling device key: %w", err)
	}
	c := &credential.Credential{
		Mode:                 resp.Credential.Mode,
		DeviceID:             resp.DeviceID,
		TenantID:             resp.TenantID,
		Region:               resp.Region,
		HardwareIdentityHash: hwid,
		DeviceIdentity:       resp.DeviceIdentity,
		PrivateKey:           string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})),
	}
	if resp.UserRefKey != "" {
		// A malformed key is dropped rather than failing the enrolment: the credential is still
		// good, and the device keeps a configured user_ref until a later enrolment issues a key.
		if _, err := protocol.DecodeUserRefKey(resp.UserRefKey); err != nil {
			d.log.Printf("drain: enrolment response carries an unusable user_ref_key: %v", err)
		} else {
			c.UserRefKey = resp.UserRefKey
		}
	}
	switch resp.Credential.Mode {
	case protocol.AuthModeX509:
		c.CertPEM = resp.Credential.CertPEM
		c.ChainPEM = resp.Credential.ChainPEM
		c.NotAfter = resp.Credential.NotAfter
	case protocol.AuthModeDPoP:
		c.JWK = resp.Credential.JWK
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("drain: issued credential is unusable: %w", err)
	}
	return c, nil
}

// enrolmentOS maps the Go platform name onto the enrolment vocabulary (windows, macos, linux), which
// control-api checks against a closed set: Go's "darwin" is the platform the vocabulary calls macos.
func enrolmentOS(goos string) string {
	if goos == "darwin" {
		return "macos"
	}
	return goos
}

// fetchToken performs POST /v1/token for the dpop mode: a signed assertion plus a proof of
// possession is exchanged for a short-lived, sender-constrained access token.
func (d *Drainer) fetchToken(ctx context.Context, key *ecdsa.PrivateKey, deviceID, tenantID string) (string, time.Time, error) {
	assertion, err := dpop.TokenAssertion(key, deviceID, tenantID)
	if err != nil {
		return "", time.Time{}, err
	}
	proof, err := dpop.DPoPProof(key, http.MethodPost, d.client.base+"/v1/token", "")
	if err != nil {
		return "", time.Time{}, err
	}
	body, err := json.Marshal(protocol.TokenRequest{
		GrantType: "urn:ietf:params:oauth:grant-type:jwt-bearer",
		Assertion: assertion,
		DeviceID:  deviceID,
	})
	if err != nil {
		return "", time.Time{}, err
	}
	status, respBody, err := d.client.post(ctx, "/v1/token", body, "application/json", "", map[string]string{
		protocol.HeaderDPoP: proof,
	}, d.client.http)
	if err != nil {
		return "", time.Time{}, &apiError{transport: true}
	}
	if status != http.StatusOK {
		return "", time.Time{}, classify(status, respBody)
	}
	var resp protocol.TokenResponse
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return "", time.Time{}, fmt.Errorf("drain: decoding token response: %w", err)
	}
	if err := resp.Validate(); err != nil {
		return "", time.Time{}, fmt.Errorf("drain: unusable token response: %w", err)
	}
	exp := time.Now().Add(time.Duration(resp.ExpiresIn) * time.Second)
	return resp.AccessToken, exp, nil
}

// sendBatch POSTs a built batch to /v1/events and returns the parsed response, or an *apiError for
// a non-200 status. The response is validated against the events that were sent before it is
// trusted, because the spool settles records on the strength of it.
func (d *Drainer) sendBatch(ctx context.Context, bb *builtBatch, cred *credential.Credential, key *ecdsa.PrivateKey, token string) (*protocol.EventBatchResponse, error) {
	headers := map[string]string{}
	hc := d.client.http
	switch d.cfg.AuthMode {
	case protocol.AuthModeDPoP:
		proof, err := dpop.DPoPProof(key, http.MethodPost, d.client.base+"/v1/events", dpop.AthOf(token))
		if err != nil {
			return nil, err
		}
		headers[protocol.HeaderDPoP] = proof
		headers[protocol.HeaderAuthorization] = "DPoP " + token
	case protocol.AuthModeX509:
		pair, err := cred.KeyPair()
		if err != nil {
			return nil, fmt.Errorf("drain: building client certificate: %w", err)
		}
		hc = d.client.withCert(pair)
	default:
		return nil, fmt.Errorf("drain: auth mode %q is not implemented", d.cfg.AuthMode)
	}

	status, respBody, err := d.client.post(ctx, "/v1/events", bb.body, "application/json", "gzip", headers, hc)
	if err != nil {
		return nil, &apiError{transport: true}
	}
	if status != http.StatusOK {
		return nil, classify(status, respBody)
	}
	var resp protocol.EventBatchResponse
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return nil, fmt.Errorf("drain: decoding events response: %w", err)
	}
	if err := resp.Validate(eventIDs(bb.entries)); err != nil {
		return nil, fmt.Errorf("drain: unusable events response: %w", err)
	}
	return &resp, nil
}

// eventIDs extracts the event_id from each envelope's payload, so the response can be validated in
// request order. The spool stores the minted envelope; the event_id is the field inside it.
func eventIDs(entries []protocol.Entry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		var env struct {
			EventID string `json:"event_id"`
		}
		if json.Unmarshal(e.Payload, &env) == nil {
			out[i] = env.EventID
		}
	}
	return out
}
