package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/attachments"
	"github.com/shadow-ai-capture/device/capture-core/core"
	"github.com/shadow-ai-capture/device/capture-core/dedup"
	"github.com/shadow-ai-capture/device/protocol"
)

// nativeSession handles the frames of one browser connection. Observations are attributed to the
// person the connection belongs to, and the pipeline decides the mode: the session never decides
// anything about content. Attachment bytes the extension transfers ahead of an observation are
// held in memory until that observation claims them for classification.
type nativeSession struct {
	svc         *service
	person      core.Person
	attachments *attachments.Holder
}

func newNativeSession(svc *service, person core.Person) *nativeSession {
	return &nativeSession{svc: svc, person: person, attachments: attachments.New(time.Now)}
}

// Handle takes one frame payload and returns the payload to answer with. Every failure is a typed
// refusal, so the extension is never left without an answer.
func (h *nativeSession) Handle(ctx context.Context, payload []byte) []byte {
	var msg protocol.NativeMessage
	if err := json.Unmarshal(payload, &msg); err != nil {
		return refusal(protocol.RefusalMalformed, "frame is not JSON: %v", err)
	}
	if msg.Version != protocol.Version {
		return refusal(protocol.RefusalVersionMismatch, "frame version %d, this agent speaks %d", msg.Version, protocol.Version)
	}
	switch msg.Type {
	case protocol.TypeObservation, protocol.TypeDecisionRecord:
		// A locally decided warn or block is still an observation: "what did we stop" must be
		// answerable.
		return h.handleObservation(ctx, msg)
	case protocol.TypeAttachmentManifest:
		return h.handleManifest(msg)
	case protocol.TypeAttachmentChunk:
		return h.handleChunk(msg)
	case protocol.TypeAttachmentComplete:
		return h.handleComplete(msg)
	case protocol.TypeHealth:
		return h.handleExtensionHealth(msg)
	case protocol.TypePolicySync:
		return h.handlePolicySync(msg)
	case protocol.TypeModeQuery:
		return h.handleModeQuery(msg)
	case "":
		return refusal(protocol.RefusalMalformed, "frame carries no type")
	default:
		return refusal(protocol.RefusalUnknownType, "unknown message type %q", msg.Type)
	}
}

func (h *nativeSession) handleObservation(ctx context.Context, msg protocol.NativeMessage) []byte {
	var obs protocol.ObservationMessage
	if err := json.Unmarshal(msg.Body, &obs); err != nil {
		return refusal(protocol.RefusalMalformed, "observation body is not an ObservationMessage: %v", err)
	}
	if err := obs.Validate(); err != nil {
		var r *protocol.RefusalError
		if errors.As(err, &r) {
			return refusal(r.Reason, "%s", r.Message)
		}
		return refusal(protocol.RefusalMalformed, "%v", err)
	}
	observation := toCoreObservation(obs)
	person := h.person
	observation.Person = &person
	// The bytes transferred for this observation are classified under its mode and then dropped.
	for _, held := range h.attachments.Claim(obs.ClientID, obs.Attachments) {
		observation.AttachmentContent = append(observation.AttachmentContent,
			core.AttachmentContent{MediaType: held.Descriptor.MediaType, Content: bytesReader(held.Bytes)})
	}
	outcome, err := h.svc.pipe.Process(ctx, observation)
	if err != nil {
		// The pipeline errs only when the observation could not be stored (a mode violation, a
		// spool failure, an unresolved identity); none of those may be answered with an ack.
		return refusal(protocol.RefusalQueueFull, "observation refused: %v", err)
	}
	return ack(msg.ID, fmt.Sprintf("event=%s mode=%s confidence=%s degraded=%v", outcome.EventID, outcome.Mode, outcome.Confidence, outcome.Degraded))
}

// handleManifest accepts or refuses an attachment before any byte moves. A device whose policy
// reads no content refuses it: the tenant default bounds every scope entry, so no observation
// could read the bytes.
func (h *nativeSession) handleManifest(msg protocol.NativeMessage) []byte {
	var m protocol.AttachmentManifest
	if err := json.Unmarshal(msg.Body, &m); err != nil {
		return refusal(protocol.RefusalMalformed, "manifest body is not an AttachmentManifest: %v", err)
	}
	if !h.svc.pipe.ResolveMode(core.ScopeQuery{UserRef: h.person.UserRef}).ReadsContent() {
		return refusal(protocol.RefusalModeForbidsRead, "the collection mode reads no content, so attachment %q is not transferred", m.Descriptor.Name)
	}
	if err := h.attachments.Open(m); err != nil {
		return attachmentRefusal(err)
	}
	return ack(msg.ID, fmt.Sprintf("transfer=%s accepted", m.TransferID))
}

func (h *nativeSession) handleChunk(msg protocol.NativeMessage) []byte {
	var c protocol.AttachmentChunk
	if err := json.Unmarshal(msg.Body, &c); err != nil {
		return refusal(protocol.RefusalMalformed, "chunk body is not an AttachmentChunk: %v", err)
	}
	if err := h.attachments.Append(c); err != nil {
		return attachmentRefusal(err)
	}
	return ack(msg.ID, fmt.Sprintf("transfer=%s chunk=%d", c.TransferID, c.Seq))
}

func (h *nativeSession) handleComplete(msg protocol.NativeMessage) []byte {
	var c protocol.AttachmentComplete
	if err := json.Unmarshal(msg.Body, &c); err != nil {
		return refusal(protocol.RefusalMalformed, "complete body is not an AttachmentComplete: %v", err)
	}
	held, err := h.attachments.Complete(c)
	if err != nil {
		return attachmentRefusal(err)
	}
	return ack(msg.ID, fmt.Sprintf("transfer=%s held %d bytes for classification (%s)", c.TransferID, len(held.Bytes), held.Descriptor.ContentDigest))
}

// attachmentRefusal maps a holder error onto the closed refusal reasons.
func attachmentRefusal(err error) []byte {
	switch {
	case errors.Is(err, attachments.ErrTooLarge):
		return refusal(protocol.RefusalAttachmentTooLarge, "%v", err)
	case errors.Is(err, attachments.ErrFull):
		return refusal(protocol.RefusalQueueFull, "%v", err)
	default:
		return refusal(protocol.RefusalMalformed, "%v", err)
	}
}

func (h *nativeSession) handleExtensionHealth(msg protocol.NativeMessage) []byte {
	var rep protocol.HealthReport
	if err := json.Unmarshal(msg.Body, &rep); err != nil {
		return refusal(protocol.RefusalMalformed, "health body is not a HealthReport: %v", err)
	}
	if rep.Collector == "" {
		rep.Collector = "capture-extension"
	}
	if rep.Counters == nil {
		rep.Counters = map[protocol.Counter]uint64{}
	}
	for _, c := range protocol.AllCounters {
		if _, ok := rep.Counters[c]; !ok {
			rep.Counters[c] = 0
		}
	}
	if err := rep.Validate(); err != nil {
		return refusal(protocol.RefusalMalformed, "%v", err)
	}
	h.svc.health.SetExtensionReport(rep)
	return ack(msg.ID, fmt.Sprintf("health recorded for %s state=%s", rep.Collector, rep.State))
}

// handlePolicySync hands the extension the verified bundle in force; the extension holds no
// durable state, so this is how it gets policy after a restart.
func (h *nativeSession) handlePolicySync(msg protocol.NativeMessage) []byte {
	var req protocol.PolicySyncRequest
	if len(msg.Body) > 0 {
		if err := json.Unmarshal(msg.Body, &req); err != nil {
			return refusal(protocol.RefusalMalformed, "policy_sync body: %v", err)
		}
	}
	answer := protocol.PolicyBundleMessage{}
	if b := h.svc.currentBundle(); b != nil {
		answer.PolicyVersion = b.Version
		answer.Unchanged = req.KnownVersion != "" && req.KnownVersion == b.Version
		if !answer.Unchanged {
			answer.Bundle = h.svc.store.InForceRaw()
		}
	}
	return reply(protocol.TypePolicyBundle, msg.ID, answer)
}

func (h *nativeSession) handleModeQuery(msg protocol.NativeMessage) []byte {
	var q protocol.ModeQuery
	if err := json.Unmarshal(msg.Body, &q); err != nil {
		return refusal(protocol.RefusalMalformed, "mode_query body: %v", err)
	}
	res := h.svc.pipe.ResolveMode(core.ScopeQuery{ToolFingerprint: q.ToolFingerprint, UserRef: h.person.UserRef})
	answer, err := res.ModeAnswer(q.Host)
	if err != nil {
		return refusal(protocol.RefusalMalformed, "%v", err)
	}
	return reply(protocol.TypeModeAnswer, msg.ID, answer)
}

func reply(typ, id string, body any) []byte {
	raw, err := json.Marshal(body)
	if err != nil {
		return refusal(protocol.RefusalMalformed, "encoding %s: %v", typ, err)
	}
	out, err := json.Marshal(protocol.NativeMessage{Type: typ, Version: protocol.Version, ID: id, Body: raw})
	if err != nil {
		return refusal(protocol.RefusalMalformed, "encoding %s: %v", typ, err)
	}
	return out
}

func ack(id, detail string) []byte {
	return reply(protocol.TypeAck, id, protocol.Ack{ID: id, Detail: detail})
}

func refusal(reason protocol.RefusalReason, format string, args ...any) []byte {
	body, _ := json.Marshal(protocol.Refusal{Reason: reason, Message: fmt.Sprintf(format, args...)})
	out, err := json.Marshal(protocol.NativeMessage{Type: protocol.TypeRefusal, Version: protocol.Version, Body: body})
	if err != nil {
		return []byte(`{"type":"refusal","version":1}`)
	}
	return out
}

// toCoreObservation turns a decoded observation into what the pipeline consumes, with the content
// behind a reader so the mode is applied before it is read.
func toCoreObservation(o protocol.ObservationMessage) core.Observation {
	decision := o.Decision
	if decision == nil {
		decision = &protocol.Decision{RuleID: "policy.default", Action: protocol.ActionLogged, DecidedLocally: true}
	}
	atts := make([]dedup.Attachment, 0, len(o.Attachments))
	for _, a := range o.Attachments {
		atts = append(atts, dedup.Attachment{Name: a.Name, MediaType: a.MediaType, SizeBytes: a.SizeBytes, ContentDigest: a.ContentDigest})
	}
	obs := core.Observation{
		Route:             o.Route,
		Kind:              protocol.KindPrompt,
		ToolFingerprint:   o.ToolFingerprint,
		OccurredAt:        o.OccurredAt,
		MonotonicOffsetMS: o.MonotonicOffsetMS,
		SizeBytes:         o.SizeBytes,
		Decision:          decision,
		Extract:           extensionExtractor{},
		Attachments:       atts,
		ClientID:          o.ClientID,
		OverCap:           o.OverCap,
	}
	if o.HasContent && len(o.Content) > 0 {
		obs.Content = bytesReader(o.Content)
	}
	return obs
}

// bytesReader is the ContentReader for bytes that arrived over native messaging.
type bytesReader []byte

func (r bytesReader) Read(context.Context) ([]byte, error) { return r, nil }

// extensionExtractor extracts the text of an extension observation: the extension sends the
// user-authored payload (the compose box or the request body), decoded here as UTF-8. The digest
// the extension computed is not reused; the pipeline recomputes it over the canonical text, so two
// routes agree on one digest rather than each trusting the other's arithmetic.
type extensionExtractor struct{}

func (extensionExtractor) Extract(payload []byte, _ string) (string, []dedup.Attachment, error) {
	if len(payload) == 0 {
		return "", nil, errors.New("native host: no payload to canonicalise")
	}
	return dedup.Decode(payload), nil, nil
}
