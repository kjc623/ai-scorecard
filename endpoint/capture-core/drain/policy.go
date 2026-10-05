package drain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/dpop"
	"github.com/shadow-ai-capture/device/protocol"
)

// PolicyStatus is what GET /v1/policy answered (docs/02 §5.2).
type PolicyStatus int

const (
	// PolicyServed: 200, a signed bundle, not yet verified.
	PolicyServed PolicyStatus = iota + 1
	// PolicyNotModified: 304, the bundle named by If-None-Match is still current.
	PolicyNotModified
	// PolicyNone: 404 with the §5 error envelope, the tenant has no bundle.
	PolicyNone
)

// PolicyFetch is one answer from GET /v1/policy. The drain does not verify the bundle — it holds no
// policy key — and hands Envelope, exactly as served, to the caller's policy.Store.
type PolicyFetch struct {
	Status   PolicyStatus
	Envelope []byte
	// ETag is the server's version tag for the next If-None-Match, as received.
	ETag string
	// NextPoll is the cadence the server asked for: Cache-Control max-age on an answer, Retry-After
	// (or retry_after_s) on a 429/503. Zero means it stated none.
	NextPoll time.Duration
}

// FetchPolicy performs GET /v1/policy with the device credential, the same authenticator as
// /v1/health, sending etag as If-None-Match when it is non-empty. A refusal or an outage is an
// error and never a PolicyNone: only the server's explicit statement that no bundle exists may move
// the device to M0, and an error leaves the bundle in force where it is (docs/01 §13.3).
func (d *Drainer) FetchPolicy(ctx context.Context, etag string) (PolicyFetch, error) {
	if !d.ready(ctx) {
		return PolicyFetch{}, errors.New("drain: not enrolled; policy not fetched")
	}
	d.mu.Lock()
	cred, key := d.cred, d.key
	d.mu.Unlock()
	if cred == nil {
		return PolicyFetch{}, errors.New("drain: no credential; policy not fetched")
	}

	headers := map[string]string{"Accept": "application/json"}
	if etag != "" {
		headers[protocol.HeaderIfNoneMatch] = etag
	}
	hc := d.client.http
	switch d.cfg.AuthMode {
	case protocol.AuthModeDPoP:
		token, err := d.tokenFor(ctx)
		if err != nil {
			return PolicyFetch{}, err
		}
		proof, err := dpop.DPoPProof(key, http.MethodGet, d.client.base+"/v1/policy", dpop.AthOf(token))
		if err != nil {
			return PolicyFetch{}, err
		}
		headers[protocol.HeaderDPoP] = proof
		headers[protocol.HeaderAuthorization] = "DPoP " + token
	case protocol.AuthModeX509:
		pair, err := cred.KeyPair()
		if err != nil {
			return PolicyFetch{}, fmt.Errorf("drain: building client certificate: %w", err)
		}
		hc = d.client.withCert(pair)
	default:
		return PolicyFetch{}, fmt.Errorf("drain: auth mode %q is not implemented", d.cfg.AuthMode)
	}

	resp, body, err := d.client.get(ctx, "/v1/policy", headers, hc)
	if err != nil {
		return PolicyFetch{}, &apiError{transport: true}
	}
	out := PolicyFetch{ETag: resp.Header.Get(protocol.HeaderETag), NextPoll: maxAge(resp.Header)}
	switch resp.StatusCode {
	case http.StatusOK:
		out.Status, out.Envelope = PolicyServed, signedEnvelope(body)
		return out, nil
	case http.StatusNotModified:
		out.Status = PolicyNotModified
		if out.ETag == "" {
			out.ETag = etag
		}
		return out, nil
	case http.StatusNotFound:
		// A 404 without the §5 error envelope is a route the server (or the edge in front of it)
		// does not have, not a statement about the tenant's policy, so it changes nothing.
		if ae := classify(resp.StatusCode, body); ae.code != "" {
			out.Status = PolicyNone
			return out, nil
		}
		return PolicyFetch{}, fmt.Errorf("drain: GET /v1/policy answered 404 without an error envelope; the route is not served")
	default:
		ae := classify(resp.StatusCode, body)
		if ae.retryAfterS > 0 {
			out.NextPoll = time.Duration(ae.retryAfterS) * time.Second
		} else if s, err := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After"))); err == nil && s > 0 {
			out.NextPoll = time.Duration(s) * time.Second
		}
		return PolicyFetch{NextPoll: out.NextPoll}, ae
	}
}

// signedEnvelope extracts the bytes to verify from a 200 body: protocol.PolicyResponse's
// signed_bundle, kept as raw JSON so the bytes the signature covers are the bytes verified. A body
// that is the signed envelope itself (the contract's "exact signed bytes") is taken as it is, and
// anything else is passed through unchanged, so the verifier, not this function, names what is
// wrong with it.
func signedEnvelope(body []byte) []byte {
	var resp protocol.PolicyResponse
	if json.Unmarshal(body, &resp) == nil && len(resp.SignedBundle) > 0 {
		return resp.SignedBundle
	}
	return body
}

// maxAge reads Cache-Control max-age, the server's statement of how long its answer stays current.
func maxAge(h http.Header) time.Duration {
	for _, part := range strings.Split(h.Get("Cache-Control"), ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok || !strings.EqualFold(k, "max-age") {
			continue
		}
		if s, err := strconv.Atoi(strings.Trim(v, `"`)); err == nil && s > 0 {
			return time.Duration(s) * time.Second
		}
	}
	return 0
}
