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
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/credential"
	"github.com/shadow-ai-capture/device/protocol"
)

// maxResponseBytes bounds how much of a response the drain reads. An events response carries one
// result per event; 4 MiB covers a full 500-event batch with margin.
const maxResponseBytes = 4 << 20

// requestTimeout bounds one request to the edge.
const requestTimeout = 20 * time.Second

// client is the device's HTTPS transport to the edge: TLS 1.3, the system roots plus an optional
// extra CA, and the device certificate presented whenever the device holds one.
type client struct {
	base string // scheme://host[:port], no trailing slash
	http *http.Client
	tr   *http.Transport
	now  func() time.Time

	mu   sync.Mutex
	cert *tls.Certificate

	// reach, when set, is told what each request found: true when the edge answered it with a
	// success, false when the request did not reach the edge. A refusal tells it nothing: whether
	// one is a fault is the caller's to judge.
	reach func(reached bool)

	// unverifiedSince is when the edge's certificate first failed verification after the last
	// handshake that verified it, and unverifiedCount how many requests have failed verification
	// since; both are zero while the last verification passed or none has failed.
	trustMu         sync.Mutex
	unverifiedSince time.Time
	unverifiedCount int
}

// edgeURL is the endpoint as the client addresses it: an https URL with no trailing slash.
func edgeURL(base string) (string, error) {
	u := strings.TrimRight(strings.TrimSpace(base), "/")
	if !strings.HasPrefix(u, "https://") || len(u) == len("https://") {
		return "", fmt.Errorf("drain: endpoint %q must be an https URL", base)
	}
	return u, nil
}

// edgeRoots is the pool the edge's certificate is verified under: the system roots, plus the PEM
// CA set in caFile when it is set (a private CA in front of the device edge). On Windows and macOS
// the system roots are the platform's store, verified through the platform, whatever SSL_CERT_FILE
// or SSL_CERT_DIR names in the environment (go.mod's x509sslcertoverrideplatform=0): the CLI trust
// shim sets those for command-line runtimes, and the agent must not inherit its own bundle as the
// edge's trust.
func edgeRoots(caFile string) (*x509.CertPool, error) {
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if strings.TrimSpace(caFile) != "" {
		pemBytes, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("drain: reading CA file: %w", err)
		}
		if !pool.AppendCertsFromPEM(pemBytes) {
			return nil, fmt.Errorf("drain: CA file %q carries no certificates", caFile)
		}
	}
	return pool, nil
}

// VerifyEdge connects to the edge and reports whether its certificate verifies under the roots
// this process sees: the system roots plus caFile, as the client verifies it. It sends no request,
// and a refusal after the certificate verified (the edge wanting a device certificate) still
// counts as verified.
func VerifyEdge(ctx context.Context, endpoint, caFile string) error {
	base, err := edgeURL(endpoint)
	if err != nil {
		return err
	}
	u, err := url.Parse(base)
	if err != nil {
		return fmt.Errorf("drain: endpoint %q: %w", endpoint, err)
	}
	pool, err := edgeRoots(caFile)
	if err != nil {
		return err
	}
	addr := u.Host
	if u.Port() == "" {
		addr = net.JoinHostPort(u.Hostname(), "443")
	}
	var verified atomic.Bool
	dialer := tls.Dialer{Config: &tls.Config{
		MinVersion: tls.VersionTLS13,
		RootCAs:    pool,
		ServerName: u.Hostname(),
		VerifyConnection: func(tls.ConnectionState) error {
			verified.Store(true)
			return nil
		},
	}}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if conn != nil {
		_ = conn.Close()
	}
	if verified.Load() {
		return nil
	}
	if err == nil {
		err = errors.New("the handshake completed without verifying the certificate")
	}
	return fmt.Errorf("drain: %s: %w", u.Host, err)
}

// newClient builds the transport. caFile, when set, names a PEM CA set trusted in addition to the
// system roots.
func newClient(base, caFile string) (*client, error) {
	u, err := edgeURL(base)
	if err != nil {
		return nil, err
	}
	pool, err := edgeRoots(caFile)
	if err != nil {
		return nil, err
	}
	c := &client{base: u, now: time.Now}
	c.tr = &http.Transport{
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS13,
			RootCAs:    pool,
			GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
				c.mu.Lock()
				defer c.mu.Unlock()
				if c.cert == nil {
					return &tls.Certificate{}, nil // no certificate yet: first enrolment
				}
				return c.cert, nil
			},
			// Runs only once the edge's certificate has verified.
			VerifyConnection: func(tls.ConnectionState) error {
				c.trustMu.Lock()
				c.unverifiedSince, c.unverifiedCount = time.Time{}, 0
				c.trustMu.Unlock()
				return nil
			},
		},
		ForceAttemptHTTP2:   true,
		MaxIdleConnsPerHost: 2,
		IdleConnTimeout:     90 * time.Second,
	}
	c.http = &http.Client{Timeout: requestTimeout, Transport: c.tr}
	return c, nil
}

// setCertificate makes cert the device certificate presented from the next connection on. Idle
// connections authenticated with the previous certificate are closed.
func (c *client) setCertificate(cert *tls.Certificate) {
	c.mu.Lock()
	c.cert = cert
	c.mu.Unlock()
	c.tr.CloseIdleConnections()
}

// do sends one request to path on the edge and returns the response with its body read, bounded.
// A transport-level failure (connection, TLS, timeout) is returned as a retryable *apiError.
func (c *client) do(ctx context.Context, method, path string, body []byte, headers map[string]string) (*http.Response, []byte, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return nil, nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		c.noteVerification(err)
		c.noteReach(false)
		return nil, nil, &apiError{transport: true, cause: err}
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		c.noteReach(false)
		return resp, nil, &apiError{transport: true, cause: err}
	}
	if succeeded(resp.StatusCode) {
		c.noteReach(true)
	}
	return resp, b, nil
}

// transfer sends req on the transport alone, without the client's timeout, which is sized for API
// calls; the caller bounds it with its context, and reads and closes the body. A transport-level
// failure is returned as a retryable *apiError.
func (c *client) transfer(req *http.Request) (*http.Response, error) {
	resp, err := c.tr.RoundTrip(req)
	if err != nil {
		c.noteReach(false)
		return nil, &apiError{transport: true, cause: err}
	}
	if succeeded(resp.StatusCode) {
		c.noteReach(true)
	}
	return resp, nil
}

// succeeded reports whether status is the edge taking the request: a 2xx, or a 304 confirming
// what the device holds.
func succeeded(status int) bool {
	return status >= 200 && status < 300 || status == http.StatusNotModified
}

func (c *client) noteReach(reached bool) {
	if c.reach != nil {
		c.reach(reached)
	}
}

// noteVerification records a request that failed because the edge's certificate did not verify.
func (c *client) noteVerification(err error) {
	var cv *tls.CertificateVerificationError
	if !errors.As(err, &cv) {
		return
	}
	c.trustMu.Lock()
	if c.unverifiedCount == 0 {
		c.unverifiedSince = c.now()
	}
	c.unverifiedCount++
	c.trustMu.Unlock()
}

// unverified reports when the edge's certificate first failed verification after the last
// handshake that verified it, and how many requests have failed verification since; 0 while the
// last verification passed or none has failed.
func (c *client) unverified() (time.Time, int) {
	c.trustMu.Lock()
	defer c.trustMu.Unlock()
	return c.unverifiedSince, c.unverifiedCount
}

// apiError is a refusal from the edge (a status with the error envelope's reason code, when it
// carried one) or a transport-level failure.
type apiError struct {
	status      int
	code        protocol.ReasonCode
	retryAfterS int
	transport   bool
	cause       error
}

func (e *apiError) Error() string {
	if e.transport {
		return fmt.Sprintf("drain: transport error: %v", e.cause)
	}
	if e.code != "" {
		return fmt.Sprintf("drain: status %d code %q", e.status, e.code)
	}
	return fmt.Sprintf("drain: status %d", e.status)
}

// Retryable reports whether the request should be retried as it is: a transport failure, 429 or
// 503. Every other refusal is terminal for the request.
func (e *apiError) Retryable() bool {
	return e.transport || e.status == http.StatusTooManyRequests || e.status == http.StatusServiceUnavailable
}

// errorEnvelope is the edge's common error body.
type errorEnvelope struct {
	Error struct {
		Code        protocol.ReasonCode `json:"code"`
		RetryAfterS int                 `json:"retry_after_s,omitempty"`
		Message     string              `json:"message,omitempty"`
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

// enrol performs POST /v1/enrol with a freshly generated key that never leaves the device. A
// rotation presents the current certificate on the connection and no deployment key; a first
// enrolment (or one after the certificate expired) presents the tenant's deployment key.
func (d *Drainer) enrol(ctx context.Context, hwid string, rotation bool) (*credential.Credential, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("drain: generating device key: %w", err)
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: "shadow-ai-capture-device"},
	}, key)
	if err != nil {
		return nil, fmt.Errorf("drain: building CSR: %w", err)
	}
	// The attestation is read at each enrolment, so an MDM enrolment or a directory join that
	// completes after the agent started is stated on the next attempt.
	var attestation *protocol.DeviceAttestation
	if d.cfg.Attestation != nil {
		attestation = d.cfg.Attestation()
	}
	body := protocol.EnrolmentRequest{
		SchemaVersion: protocol.EnrolmentSchemaVersion,
		CSR:           string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})),
		Device: protocol.DeviceInfo{
			OS:                   enrolmentOS(runtime.GOOS),
			AgentVersion:         d.cfg.AgentVersion,
			Hostname:             d.cfg.Hostname,
			HostnameHash:         d.cfg.HostnameHash,
			ManagedState:         d.cfg.ManagedState,
			HardwareIdentityHash: hwid,
		},
		Attestation: attestation,
	}
	if !rotation {
		body.DeploymentKey = d.cfg.DeploymentKey
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("drain: encoding enrol request: %w", err)
	}
	resp, respBody, err := d.client.do(ctx, http.MethodPost, "/v1/enrol", raw, map[string]string{"Content-Type": "application/json"})
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, classify(resp.StatusCode, respBody)
	}
	var er protocol.EnrolmentResponse
	if err := json.Unmarshal(respBody, &er); err != nil {
		return nil, fmt.Errorf("drain: decoding enrol response: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("drain: marshalling device key: %w", err)
	}
	c := &credential.Credential{
		DeviceID:             er.DeviceID,
		TenantID:             er.TenantID,
		Region:               er.Region,
		HardwareIdentityHash: hwid,
		DeviceIdentity:       er.DeviceIdentity,
		PrivateKey:           string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})),
		CertPEM:              er.Credential.CertPEM,
		ChainPEM:             er.Credential.ChainPEM,
		NotAfter:             er.Credential.NotAfter,
	}
	if er.UserRefKey != "" {
		// A malformed key is dropped rather than failing the enrolment: the credential is still
		// good, and observations stay unattributed until a later enrolment issues a usable key.
		if _, err := protocol.DecodeUserRefKey(er.UserRefKey); err != nil {
			d.log.Printf("drain: enrolment response carries an unusable user_ref_key: %v", err)
		} else {
			c.UserRefKey = er.UserRefKey
		}
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("drain: issued credential is unusable: %w", err)
	}
	if _, err := c.KeyPair(); err != nil {
		return nil, fmt.Errorf("drain: issued certificate does not match the device key: %w", err)
	}
	return c, nil
}

// enrolmentOS maps the Go platform name onto the enrolment vocabulary (windows, macos, linux).
func enrolmentOS(goos string) string {
	if goos == "darwin" {
		return "macos"
	}
	return goos
}

// sendBatch POSTs a built batch to /v1/events and returns the parsed response, or an *apiError for
// a non-200 status. The response is validated against the events sent before it is trusted,
// because the spool settles records on the strength of it.
func (d *Drainer) sendBatch(ctx context.Context, bb *builtBatch) (*protocol.EventBatchResponse, error) {
	resp, body, err := d.client.do(ctx, http.MethodPost, "/v1/events", bb.body, map[string]string{
		"Content-Type":     "application/json",
		"Content-Encoding": "gzip",
	})
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, classify(resp.StatusCode, body)
	}
	var out protocol.EventBatchResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("drain: decoding events response: %w", err)
	}
	if err := out.Validate(eventIDs(bb.entries)); err != nil {
		return nil, fmt.Errorf("drain: unusable events response: %w", err)
	}
	return &out, nil
}

// eventIDs extracts the event_id from each envelope, so the response can be validated in request
// order.
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

func isAPIError(err error) (*apiError, bool) {
	var ae *apiError
	ok := errors.As(err, &ae)
	return ae, ok
}
