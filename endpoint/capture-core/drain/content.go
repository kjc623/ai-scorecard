package drain

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/contentstore"
	"github.com/shadow-ai-capture/device/capture-core/dpop"
	"github.com/shadow-ai-capture/device/protocol"
)

// ContentSource is the grant-side view of the M3 local content store (docs/02 §3, §10). The
// drainer owns the device's credential and its transport to the edge, so it is the component that
// asks for a grant and performs the one upload a grant permits; the store owns the content.
type ContentSource interface {
	MarkDelivered(eventID string, req contentstore.Request) (bool, error)
	Ready() []contentstore.Item
	Get(eventID string) ([]byte, error)
	Settle(eventID string, state contentstore.State, detail string) error
	Retry(eventID, detail string, at time.Time) error
	Expire(now time.Time) (int, error)
}

// contentPassLimit bounds how many grants one pass asks for, so a backlog of held content cannot
// starve event delivery, which shares the pass.
const contentPassLimit = 8

// noteDelivered tells the content store that an event reached the server. A grant can only be
// requested for an event the server has, so this is what makes held content requestable.
func (d *Drainer) noteDelivered(payload []byte) {
	if d.cfg.Content == nil {
		return
	}
	var env struct {
		EventID        string                  `json:"event_id"`
		Kind           protocol.Kind           `json:"kind"`
		CollectionMode protocol.CollectionMode `json:"collection_mode"`
		ContentDigest  string                  `json:"content_digest"`
		Decision       *struct {
			RuleID string `json:"rule_id"`
		} `json:"policy_decision"`
	}
	if json.Unmarshal(payload, &env) != nil || env.Kind != protocol.KindPrompt || env.CollectionMode != protocol.ModeM3 {
		return
	}
	req := contentstore.Request{CollectionMode: string(env.CollectionMode), ContentDigest: env.ContentDigest}
	if env.Decision != nil {
		req.PolicyRuleID = env.Decision.RuleID
	}
	if _, err := d.cfg.Content.MarkDelivered(env.EventID, req); err != nil {
		d.log.Printf("drain: content: recording delivery of %s: %v", env.EventID, err)
	}
}

// contentPass asks for a grant for each held object whose event has been delivered, and uploads
// the ones that are granted. A denial is terminal and the content stays on the device; anything
// else is retried with backoff. Nothing here can fail the event drain.
func (d *Drainer) contentPass(ctx context.Context, deadline time.Time) {
	src := d.cfg.Content
	if src == nil {
		return
	}
	if n, err := src.Expire(d.clock()); err != nil {
		d.log.Printf("drain: content: retention sweep failed: %v", err)
	} else if n > 0 {
		d.log.Printf("drain: content: retention removed %d held object(s)", n)
	}
	items := src.Ready()
	if len(items) > contentPassLimit {
		items = items[:contentPassLimit]
	}
	for _, it := range items {
		if ctx.Err() != nil || !d.clock().Before(deadline) {
			return
		}
		state, detail, err := d.uploadOne(ctx, it)
		switch {
		case err != nil:
			at := d.clock().Add(d.backoff.Delay(it.Attempts))
			_ = src.Retry(it.EventID, err.Error(), at)
			d.log.Printf("drain: content: event %s not uploaded, retrying: %v", it.EventID, err)
		default:
			if serr := src.Settle(it.EventID, state, detail); serr != nil {
				d.log.Printf("drain: content: settling %s: %v", it.EventID, serr)
			}
			d.log.Printf("drain: content: event %s %s %s", it.EventID, state, detail)
		}
	}
}

// uploadOne runs the grant-and-upload exchange for one event. A returned error means "try again";
// a returned state is terminal.
func (d *Drainer) uploadOne(ctx context.Context, it contentstore.Item) (contentstore.State, string, error) {
	plaintext, err := d.cfg.Content.Get(it.EventID)
	if err != nil {
		return "", "", fmt.Errorf("reading held content: %w", err)
	}
	req := protocol.ContentGrantRequest{
		SchemaVersion:  protocol.ContentGrantSchemaVersion,
		EventID:        it.EventID,
		CollectionMode: protocol.CollectionMode(it.Request.CollectionMode),
		ContentDigest:  it.Request.ContentDigest,
		SizeBytes:      int64(len(plaintext)),
		RawSizeBytes:   protocol.SealedContentSize(int64(len(plaintext))),
		PolicyRuleID:   it.Request.PolicyRuleID,
	}
	body, err := json.Marshal(req)
	if err != nil {
		return "", "", err
	}
	status, respBody, err := d.authed(ctx, http.MethodPost, d.client.base+"/v1/content/grant", body, map[string]string{"Content-Type": "application/json"})
	if err != nil {
		return "", "", err
	}
	switch status {
	case http.StatusOK:
	case http.StatusConflict:
		// The grant for this event was already consumed: an earlier upload landed and its
		// acknowledgement was lost.
		return contentstore.StateUploaded, string(protocol.ReasonGrantConsumed), nil
	case http.StatusUnprocessableEntity:
		return contentstore.StateDenied, string(protocol.ReasonModeViolation), nil
	default:
		return "", "", classify(status, respBody)
	}
	var grant protocol.ContentGrantResponse
	if err := json.Unmarshal(respBody, &grant); err != nil {
		return "", "", fmt.Errorf("decoding grant response: %w", err)
	}
	if grant.State == protocol.ContentGrantDenied {
		return contentstore.StateDenied, grant.Reason, nil
	}
	if grant.State != protocol.ContentGrantGranted || grant.Upload == nil || grant.Key == nil {
		return "", "", fmt.Errorf("grant response for %s is not a usable decision (state %q)", it.EventID, grant.State)
	}
	if grant.Key.Alg != protocol.ContentKeyAlg {
		return "", "", fmt.Errorf("grant names object-key algorithm %q, want %s", grant.Key.Alg, protocol.ContentKeyAlg)
	}
	key, err := base64.StdEncoding.DecodeString(grant.Key.ObjectKeyB64)
	if err != nil {
		return "", "", fmt.Errorf("grant carries an undecodable object key: %w", err)
	}
	sealed, err := protocol.SealContent(key, it.EventID, plaintext)
	if err != nil {
		return "", "", err
	}
	if grant.MaxBytes > 0 && int64(len(sealed)) > grant.MaxBytes {
		return contentstore.StateDenied, "over_grant_size", nil
	}
	headers := map[string]string{"Content-Type": "application/octet-stream", protocol.HeaderContentRawDigest: protocol.RawDigest(sealed)}
	for k, v := range grant.Upload.Headers {
		headers[k] = v
	}
	method := grant.Upload.Method
	if method == "" {
		method = http.MethodPut
	}
	status, respBody, err = d.authed(ctx, method, grant.Upload.URL, sealed, headers)
	if err != nil {
		return "", "", err
	}
	switch status {
	case http.StatusOK, http.StatusCreated, http.StatusNoContent:
		return contentstore.StateUploaded, grant.GrantID, nil
	case http.StatusConflict:
		// Single use: the object is already there.
		return contentstore.StateUploaded, grant.GrantID, nil
	default:
		return "", "", fmt.Errorf("upload refused: %w", classify(status, respBody))
	}
}

// authed sends one request to the edge with the device credential, in whichever mode the device
// enrolled with.
func (d *Drainer) authed(ctx context.Context, method, url string, body []byte, headers map[string]string) (int, []byte, error) {
	d.mu.Lock()
	cred, key := d.cred, d.key
	d.mu.Unlock()
	if cred == nil {
		return 0, nil, fmt.Errorf("not enrolled")
	}
	hc := d.client.http
	h := map[string]string{}
	for k, v := range headers {
		h[k] = v
	}
	switch d.cfg.AuthMode {
	case protocol.AuthModeX509:
		pair, err := cred.KeyPair()
		if err != nil {
			return 0, nil, fmt.Errorf("building client certificate: %w", err)
		}
		hc = d.client.withCert(pair)
	case protocol.AuthModeDPoP:
		if err := d.ensureToken(ctx); err != nil {
			return 0, nil, err
		}
		d.mu.Lock()
		token := d.token
		d.mu.Unlock()
		proof, err := dpop.DPoPProof(key, method, url, dpop.AthOf(token))
		if err != nil {
			return 0, nil, err
		}
		h[protocol.HeaderDPoP] = proof
		h[protocol.HeaderAuthorization] = "DPoP " + token
	default:
		return 0, nil, fmt.Errorf("auth mode %q is not implemented", d.cfg.AuthMode)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	for k, v := range h {
		req.Header.Set(k, v)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, nil, &apiError{transport: true}
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, b, nil
}
