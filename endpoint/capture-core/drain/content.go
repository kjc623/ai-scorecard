package drain

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/contentstore"
	"github.com/shadow-ai-capture/device/protocol"
)

// ContentSource is the grant-side view of the M3 content store. The drainer owns the device's
// credential and its transport to the edge, so it asks for a grant and performs the one upload a
// grant permits; the store owns the content.
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
		EventID        string                          `json:"event_id"`
		Kind           protocol.Kind                   `json:"kind"`
		CollectionMode protocol.CollectionMode         `json:"collection_mode"`
		ContentDigest  string                          `json:"content_digest"`
		Attachments    []protocol.AttachmentDescriptor `json:"attachments"`
		Decision       *struct {
			RuleID string `json:"rule_id"`
		} `json:"policy_decision"`
	}
	if json.Unmarshal(payload, &env) != nil || env.Kind != protocol.KindPrompt || env.CollectionMode != protocol.ModeM3 {
		return
	}
	req := contentstore.Request{
		CollectionMode:  string(env.CollectionMode),
		ContentDigest:   env.ContentDigest,
		AttachmentCount: len(env.Attachments),
	}
	if env.Decision != nil {
		req.PolicyRuleID = env.Decision.RuleID
	}
	if _, err := d.cfg.Content.MarkDelivered(env.EventID, req); err != nil {
		d.log.Printf("drain: content: recording delivery of %s: %v", env.EventID, err)
	}
}

// contentPass asks for a grant for each held object whose event has been delivered, and uploads
// the granted ones. A denial is terminal and the content stays on the device until retention
// removes it; anything else is retried with backoff. Nothing here can fail the event drain.
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
		if err != nil {
			_ = src.Retry(it.EventID, err.Error(), d.clock().Add(d.backoff.Delay(it.Attempts)))
			d.log.Printf("drain: content: event %s not uploaded, retrying: %v", it.EventID, err)
			continue
		}
		if err := src.Settle(it.EventID, state, detail); err != nil {
			d.log.Printf("drain: content: settling %s: %v", it.EventID, err)
		}
		d.log.Printf("drain: content: event %s %s %s", it.EventID, state, detail)
	}
}

// uploadOne requests the grant for one event and, when it is granted, uploads the content. A
// returned error means try again; a returned state is terminal.
func (d *Drainer) uploadOne(ctx context.Context, it contentstore.Item) (contentstore.State, string, error) {
	body, err := d.cfg.Content.Get(it.EventID)
	if err != nil {
		return "", "", fmt.Errorf("reading held content: %w", err)
	}
	if len(body) > protocol.MaxContentObjectBytes {
		return contentstore.StateDenied, "over_max_object_size", nil
	}
	req, err := json.Marshal(protocol.ContentGrantRequest{
		SchemaVersion:   protocol.ContentGrantSchemaVersion,
		EventID:         it.EventID,
		CollectionMode:  protocol.CollectionMode(it.Request.CollectionMode),
		ContentDigest:   it.Request.ContentDigest,
		SizeBytes:       int64(len(body)),
		AttachmentCount: it.Request.AttachmentCount,
		RawSizeBytes:    int64(len(body)),
		PolicyRuleID:    it.Request.PolicyRuleID,
	})
	if err != nil {
		return "", "", err
	}
	resp, respBody, err := d.client.do(ctx, http.MethodPost, "/v1/content/grant", req, map[string]string{"Content-Type": "application/json"})
	if err != nil {
		return "", "", err
	}
	if resp.StatusCode != http.StatusOK {
		ae := classify(resp.StatusCode, respBody)
		switch {
		case ae.code == protocol.ReasonGrantConsumed:
			// The grant for this event was already used: an earlier upload landed and its
			// acknowledgement was lost.
			return contentstore.StateUploaded, string(protocol.ReasonGrantConsumed), nil
		case ae.code == protocol.ReasonModeViolation:
			return contentstore.StateDenied, string(protocol.ReasonModeViolation), nil
		}
		return "", "", ae
	}
	var grant protocol.ContentGrantResponse
	if err := json.Unmarshal(respBody, &grant); err != nil {
		return "", "", fmt.Errorf("decoding grant response: %w", err)
	}
	switch {
	case grant.State == protocol.ContentGrantDenied:
		return contentstore.StateDenied, grant.Reason, nil
	case grant.State != protocol.ContentGrantGranted || grant.GrantID == "":
		return "", "", fmt.Errorf("grant response for %s is not a usable decision (state %q)", it.EventID, grant.State)
	case grant.MaxBytes > 0 && int64(len(body)) > grant.MaxBytes:
		return contentstore.StateDenied, "over_grant_size", nil
	}

	resp, respBody, err = d.client.do(ctx, http.MethodPost, protocol.ContentUploadPath, body, map[string]string{
		"Content-Type":                  "application/octet-stream",
		protocol.HeaderContentGrantID:   grant.GrantID,
		protocol.HeaderContentEventID:   it.EventID,
		protocol.HeaderContentRawDigest: protocol.RawDigest(body),
	})
	if err != nil {
		return "", "", err
	}
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return contentstore.StateUploaded, grant.GrantID, nil
	default:
		ae := classify(resp.StatusCode, respBody)
		if ae.code == protocol.ReasonGrantConsumed {
			// Already stored under this grant: the upload is idempotent.
			return contentstore.StateUploaded, grant.GrantID, nil
		}
		// An expired grant, an outage or a refusal: ask for a fresh grant on the next attempt.
		return "", "", fmt.Errorf("upload refused: %w", ae)
	}
}
