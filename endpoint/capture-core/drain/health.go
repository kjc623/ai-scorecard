package drain

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/shadow-ai-capture/device/capture-core/credential"
	"github.com/shadow-ai-capture/device/capture-core/dpop"
	"github.com/shadow-ai-capture/device/protocol"
)

// ReportHealth sends one health report to POST /v1/health over the same authenticated transport the
// event drain uses. It is the periodic heartbeat docs/02-ingest-and-transport.md §5.4 and §9
// describe: a device that is idle still says it is alive, so an absence of events is never read as
// a dead device (docs/04 §3.7).
//
// It never blocks startup: the caller is the health channel's own goroutine. A failure is returned
// so the channel can report a degraded drain; it is not retried here, because the next tick is the
// retry.
func (d *Drainer) ReportHealth(ctx context.Context, req protocol.HealthRequest) (protocol.HealthResponse, error) {
	if err := req.Validate(); err != nil {
		return protocol.HealthResponse{}, err
	}
	if !d.ready(ctx) {
		return protocol.HealthResponse{}, errors.New("drain: not enrolled; health report not sent")
	}
	d.mu.Lock()
	cred := d.cred
	key := d.key
	d.mu.Unlock()
	if cred == nil {
		return protocol.HealthResponse{}, errors.New("drain: no credential; health report not sent")
	}
	return d.sendHealth(ctx, req, cred, key)
}

func (d *Drainer) sendHealth(ctx context.Context, req protocol.HealthRequest, cred *credential.Credential, key *ecdsa.PrivateKey) (protocol.HealthResponse, error) {
	raw, err := json.Marshal(req)
	if err != nil {
		return protocol.HealthResponse{}, fmt.Errorf("drain: encoding health report: %w", err)
	}
	headers := map[string]string{}
	hc := d.client.http
	switch d.cfg.AuthMode {
	case protocol.AuthModeDPoP:
		token, err := d.tokenFor(ctx)
		if err != nil {
			return protocol.HealthResponse{}, err
		}
		proof, err := dpop.DPoPProof(key, http.MethodPost, d.client.base+"/v1/health", dpop.AthOf(token))
		if err != nil {
			return protocol.HealthResponse{}, err
		}
		headers[protocol.HeaderDPoP] = proof
		headers[protocol.HeaderAuthorization] = "DPoP " + token
	case protocol.AuthModeX509:
		pair, err := cred.KeyPair()
		if err != nil {
			return protocol.HealthResponse{}, fmt.Errorf("drain: building client certificate: %w", err)
		}
		hc = d.client.withCert(pair)
	default:
		return protocol.HealthResponse{}, fmt.Errorf("drain: auth mode %q is not implemented", d.cfg.AuthMode)
	}

	status, body, err := d.client.post(ctx, "/v1/health", raw, "application/json", "", headers, hc)
	if err != nil {
		return protocol.HealthResponse{}, &apiError{transport: true}
	}
	if status != http.StatusOK {
		return protocol.HealthResponse{}, classify(status, body)
	}
	var resp protocol.HealthResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return protocol.HealthResponse{}, fmt.Errorf("drain: decoding health response: %w", err)
	}
	return resp, nil
}

// tokenFor returns a usable DPoP access token, acquiring one when the cached token is close to
// expiry. It mirrors the event drain's token handling but is callable from the health goroutine.
func (d *Drainer) tokenFor(ctx context.Context) (string, error) {
	if err := d.ensureToken(ctx); err != nil {
		return "", err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.token, nil
}
