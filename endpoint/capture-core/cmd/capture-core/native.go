package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/core"
	"github.com/shadow-ai-capture/device/capture-core/dedup"
	"github.com/shadow-ai-capture/device/protocol"
)

// The native-messaging transport is Chromium's, not this project's: a 4-byte **little-endian**
// length prefix followed by one JSON document, on the child process's stdin/stdout.
//
// That is deliberately NOT device/protocol's framing, which is a 1-byte version plus a big-endian
// 32-bit length on a local socket (§3.4). The two are different transports with different peers,
// and conflating them would put a browser's transport rule inside the classifier wire format. So
// the adapter lives here, in the binary, and device/protocol is untouched.
const (
	// maxNativeFrameBytes is Chromium's own limit for one host message. A frame larger than this
	// cannot arrive, and a length that claims to be is refused before allocating.
	maxNativeFrameBytes = 1024 * 1024
	nativeHeaderBytes   = 4
)

// writeNativeFrame writes one Chromium native-messaging frame.
func writeNativeFrame(w io.Writer, payload []byte) error {
	if len(payload) > maxNativeFrameBytes {
		return fmt.Errorf("native message is %d bytes, over Chromium's %d limit", len(payload), maxNativeFrameBytes)
	}
	var hdr [nativeHeaderBytes]byte
	binary.LittleEndian.PutUint32(hdr[:], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

// readNativeFrame reads one Chromium native-messaging frame.
func readNativeFrame(r io.Reader) ([]byte, error) {
	var hdr [nativeHeaderBytes]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.LittleEndian.Uint32(hdr[:])
	if n > maxNativeFrameBytes {
		return nil, fmt.Errorf("native frame declares %d bytes, over the %d limit", n, maxNativeFrameBytes)
	}
	if n == 0 {
		// An empty frame is not a message: it is a defect on the other side.
		return nil, errors.New("native frame carries no payload")
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, err
	}
	return payload, nil
}

// nativeHost turns frames into pipeline calls. It is the seam device/integration simulates: a
// decoded ObservationMessage becomes a core.Observation, and the pipeline decides the mode — this
// type never decides anything about content.
type nativeHost struct {
	pipe     *core.Pipeline
	store    *policyStoreView
	deviceID string
	log      *slog.Logger
	health   *healthChannel
	cfg      Config

	mu          sync.Mutex
	transfers   map[string]*attachmentTransfer
	lastVersion string
}

// policyStoreView is the narrow view of the policy store the native host needs, so the host cannot
// reach into verification or force a bundle.
type policyStoreView struct {
	currentVersion func() string
	currentBytes   func() []byte
}

type attachmentTransfer struct {
	descriptor protocol.AttachmentDescriptor
	chunks     int
	bytes      int64
	openedAt   time.Time
}

func newNativeHost(pipe *core.Pipeline, store *policyStoreView, cfg Config, health *healthChannel, log *slog.Logger) *nativeHost {
	return &nativeHost{
		pipe:      pipe,
		store:     store,
		deviceID:  cfg.DeviceID,
		log:       log,
		health:    health,
		cfg:       cfg,
		transfers: map[string]*attachmentTransfer{},
	}
}

// Handle takes one frame payload and returns the frame payload to write back. It never returns nil
// and never panics on hostile input: every failure path answers with a typed refusal, which is what
// "the extension is never left hanging" means in practice.
func (h *nativeHost) Handle(ctx context.Context, payload []byte) []byte {
	var msg protocol.NativeMessage
	if err := json.Unmarshal(payload, &msg); err != nil {
		return h.refuse(protocol.RefusalMalformed, "frame is not JSON: %v", err)
	}
	if msg.Type == "" {
		return h.refuse(protocol.RefusalMalformed, "frame carries no type discriminator")
	}

	// Payloads flow one way and responses are correlated by the frame's id, so the answer always
	// carries the id it is answering.
	switch msg.Type {
	case protocol.TypeObservation:
		return h.handleObservation(ctx, msg)
	case protocol.TypeDecisionRecord:
		// A locally decided warn/block is still an observation: "what did we stop" must be
		// answerable, so a decision record is processed on the observation path.
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
	default:
		return h.refuse(protocol.RefusalUnknownType, "unknown message type %q", msg.Type)
	}
}

func (h *nativeHost) handleObservation(ctx context.Context, msg protocol.NativeMessage) []byte {
	var obs protocol.ObservationMessage
	if err := json.Unmarshal(msg.Body, &obs); err != nil {
		return h.refuse(protocol.RefusalMalformed, "observation body is not an ObservationMessage: %v", err)
	}
	if err := obs.Validate(); err != nil {
		var refusal *protocol.RefusalError
		if errors.As(err, &refusal) {
			return h.refuse(refusal.Reason, "%s", refusal.Message)
		}
		return h.refuse(protocol.RefusalMalformed, "%v", err)
	}

	// A manifest that was already refused must not be followed by bytes, and a manifest that names
	// an oversized attachment is refused here even when the observation carries it inline.
	for _, a := range obs.Attachments {
		if a.SizeBytes > h.cfg.AttachmentCap {
			return h.refuse(protocol.RefusalAttachmentTooLarge, "attachment %q is %d bytes, over the %d cap", a.Name, a.SizeBytes, h.cfg.AttachmentCap)
		}
	}

	observation := toCoreObservation(obs)
	outcome, err := h.pipe.Process(ctx, observation)
	if err != nil {
		// The pipeline returns an error only when the observation was refused before it could be
		// stored (a mode violation, a spool failure, an invalid envelope). None of those is
		// "success", so none of them may be answered with an ack.
		return h.refuse(protocol.RefusalQueueFull, "observation refused: %v", err)
	}
	if !outcome.Emitted {
		return h.refuse(protocol.RefusalModeForbidsRead, "observation was not emitted: %s", outcome.Reason)
	}
	return h.ack(msg.ID, fmt.Sprintf("event=%s seq=%d mode=%s confidence=%s degraded=%v reason=%s",
		outcome.EventID, outcome.Seq, outcome.Mode, outcome.Confidence, outcome.Degraded, outcome.Reason))
}

func (h *nativeHost) handleManifest(msg protocol.NativeMessage) []byte {
	var m protocol.AttachmentManifest
	if err := json.Unmarshal(msg.Body, &m); err != nil {
		return h.refuse(protocol.RefusalMalformed, "manifest body is not an AttachmentManifest: %v", err)
	}
	if m.TransferID == "" || m.Descriptor.Name == "" {
		return h.refuse(protocol.RefusalMalformed, "manifest needs a transfer_id and a descriptor name")
	}
	// The descriptor arrives BEFORE any byte moves, which is the whole point: an oversized upload
	// is refused without transferring it (§3.4).
	if m.Descriptor.SizeBytes > h.cfg.AttachmentCap {
		return h.refuse(protocol.RefusalAttachmentTooLarge, "attachment %q is %d bytes, over the %d cap", m.Descriptor.Name, m.Descriptor.SizeBytes, h.cfg.AttachmentCap)
	}
	if m.Descriptor.SizeBytes < 0 {
		return h.refuse(protocol.RefusalMalformed, "attachment %q declares a negative size", m.Descriptor.Name)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, exists := h.transfers[m.TransferID]; exists {
		return h.refuse(protocol.RefusalMalformed, "transfer %q is already open", m.TransferID)
	}
	h.transfers[m.TransferID] = &attachmentTransfer{descriptor: m.Descriptor, openedAt: time.Now()}
	return h.ack(msg.ID, fmt.Sprintf("transfer=%s accepted manifest for %q (%d bytes, media type %q)",
		m.TransferID, m.Descriptor.Name, m.Descriptor.SizeBytes, m.Descriptor.MediaType))
}

func (h *nativeHost) handleChunk(msg protocol.NativeMessage) []byte {
	var c protocol.AttachmentChunk
	if err := json.Unmarshal(msg.Body, &c); err != nil {
		return h.refuse(protocol.RefusalMalformed, "chunk body is not an AttachmentChunk: %v", err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	t, ok := h.transfers[c.TransferID]
	if !ok {
		// Bytes without an accepted manifest are refused rather than assembled into a partial file.
		return h.refuse(protocol.RefusalMalformed, "chunk for unknown transfer %q; the manifest must be accepted first", c.TransferID)
	}
	if c.Seq != t.chunks {
		return h.refuse(protocol.RefusalMalformed, "chunk %d arrived where %d was expected; a gap is refused rather than assembled", c.Seq, t.chunks)
	}
	t.bytes += int64(len(c.Data))
	if t.bytes > h.cfg.AttachmentCap {
		delete(h.transfers, c.TransferID)
		return h.refuse(protocol.RefusalAttachmentTooLarge, "transfer %q exceeded the %d cap", c.TransferID, h.cfg.AttachmentCap)
	}
	t.chunks++
	return h.ack(msg.ID, fmt.Sprintf("transfer=%s chunk=%d bytes=%d", c.TransferID, c.Seq, len(c.Data)))
}

func (h *nativeHost) handleComplete(msg protocol.NativeMessage) []byte {
	var c protocol.AttachmentComplete
	if err := json.Unmarshal(msg.Body, &c); err != nil {
		return h.refuse(protocol.RefusalMalformed, "complete body is not an AttachmentComplete: %v", err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	t, ok := h.transfers[c.TransferID]
	if !ok {
		return h.refuse(protocol.RefusalMalformed, "completion for unknown transfer %q", c.TransferID)
	}
	delete(h.transfers, c.TransferID)
	return h.ack(msg.ID, fmt.Sprintf("transfer=%s closed with %d chunks and %d bytes%s",
		c.TransferID, t.chunks, t.bytes, errorSuffix(c.Err)))
}

func errorSuffix(err string) string {
	if err == "" {
		return ""
	}
	// A failed attachment read never fails the submission; it is counted and reported (§3.4).
	return "; read error reported: " + err
}

func (h *nativeHost) handleExtensionHealth(msg protocol.NativeMessage) []byte {
	var rep protocol.HealthReport
	if err := json.Unmarshal(msg.Body, &rep); err != nil {
		return h.refuse(protocol.RefusalMalformed, "health body is not a HealthReport: %v", err)
	}
	if rep.Collector == "" {
		rep.Collector = "capture-extension"
	}
	// The extension cannot read the spool, so its row carries its own queue counters; the shape is
	// the same closed counter set, which is what lets the coverage report merge them.
	if rep.Counters == nil {
		rep.Counters = map[protocol.Counter]uint64{}
	}
	for _, c := range protocol.AllCounters {
		if _, ok := rep.Counters[c]; !ok {
			rep.Counters[c] = 0
		}
	}
	if err := rep.Validate(); err != nil {
		return h.refuse(protocol.RefusalMalformed, "%v", err)
	}
	if h.health != nil {
		h.health.SetExtensionReport(rep)
	}
	return h.ack(msg.ID, fmt.Sprintf("health recorded for %s state=%s", rep.Collector, rep.State))
}

func (h *nativeHost) handlePolicySync(msg protocol.NativeMessage) []byte {
	var req protocol.PolicySyncRequest
	if len(msg.Body) > 0 {
		if err := json.Unmarshal(msg.Body, &req); err != nil {
			return h.refuse(protocol.RefusalMalformed, "policy_sync body: %v", err)
		}
	}
	current := ""
	var bundle json.RawMessage
	if h.store != nil {
		current = h.store.currentVersion()
		bundle = h.store.currentBytes()
	}
	unchanged := req.KnownVersion != "" && req.KnownVersion == current
	resp := protocol.NativeMessage{
		Type:    protocol.TypePolicyBundle,
		Version: protocol.Version,
		ID:      msg.ID,
	}
	body, err := json.Marshal(protocol.PolicyBundleMessage{
		PolicyVersion: current,
		Bundle:        bundle,
		Unchanged:     unchanged,
	})
	if err != nil {
		return h.refuse(protocol.RefusalMalformed, "encoding policy bundle: %v", err)
	}
	resp.Body = body
	raw, err := json.Marshal(resp)
	if err != nil {
		return h.refuse(protocol.RefusalMalformed, "encoding response: %v", err)
	}
	return raw
}

func (h *nativeHost) handleModeQuery(msg protocol.NativeMessage) []byte {
	var q protocol.ModeQuery
	if err := json.Unmarshal(msg.Body, &q); err != nil {
		return h.refuse(protocol.RefusalMalformed, "mode_query body: %v", err)
	}
	res := h.pipe.ResolveMode(core.ScopeQuery{
		ToolFingerprint: q.ToolFingerprint,
		DeviceID:        h.cfg.DeviceID,
		UserRef:         h.cfg.UserRef,
		Population:      h.cfg.Population,
	})
	answer, err := res.ModeAnswer(q.Host)
	if err != nil {
		return h.refuse(protocol.RefusalMalformed, "%v", err)
	}
	resp := protocol.NativeMessage{Type: protocol.TypeModeAnswer, Version: protocol.Version, ID: msg.ID}
	body, err := json.Marshal(answer)
	if err != nil {
		return h.refuse(protocol.RefusalMalformed, "encoding mode answer: %v", err)
	}
	resp.Body = body
	raw, err := json.Marshal(resp)
	if err != nil {
		return h.refuse(protocol.RefusalMalformed, "encoding response: %v", err)
	}
	return raw
}

func (h *nativeHost) ack(id, detail string) []byte {
	resp := protocol.NativeMessage{Type: protocol.TypeAck, Version: protocol.Version, ID: id}
	body, _ := json.Marshal(protocol.Ack{ID: id, Detail: detail})
	resp.Body = body
	raw, err := json.Marshal(resp)
	if err != nil {
		return []byte(`{"type":"refusal","version":1}`)
	}
	return raw
}

func (h *nativeHost) refuse(reason protocol.RefusalReason, format string, args ...any) []byte {
	resp := protocol.NativeMessage{Type: protocol.TypeRefusal, Version: protocol.Version}
	body, _ := json.Marshal(protocol.Refusal{Reason: reason, Message: fmt.Sprintf(format, args...)})
	resp.Body = body
	raw, err := json.Marshal(resp)
	if err != nil {
		return []byte(`{"type":"refusal","version":1}`)
	}
	return raw
}

// toCoreObservation is the conversion device/integration simulates, now in the binary that must do
// it for real: a decoded frame becomes what the pipeline consumes, with a reader rather than bytes.
func toCoreObservation(o protocol.ObservationMessage) core.Observation {
	decision := o.Decision
	if decision == nil {
		decision = &protocol.Decision{RuleID: "policy.default", Action: protocol.ActionLogged, DecidedLocally: true}
	}
	obs := core.Observation{
		Route:             o.Route,
		Kind:              protocol.KindPrompt,
		ToolFingerprint:   o.ToolFingerprint,
		Population:        "",
		OccurredAt:        o.OccurredAt,
		MonotonicOffsetMS: o.MonotonicOffsetMS,
		SizeBytes:         o.SizeBytes,
		Decision:          decision,
		Extract:           extensionExtractor{},
		ClientID:          o.ClientID,
		OverCap:           o.OverCap,
	}
	if o.HasContent && len(o.Content) > 0 {
		// Hand over a reader, never the bytes: a pipeline given bytes has already read them, and
		// §11.2's ordering cannot be enforced after the fact.
		obs.Content = bytesReader{body: o.Content}
	}
	return obs
}

// extensionExtractor is C1 for the extension routes. The extension sends the user-authored payload
// (it read the compose box or the request body), and it also sends a content digest it computed
// over those bytes; the canonical text is the payload itself, decoded as UTF-8.
//
// The digest the extension computed is deliberately NOT reused as the canonical content digest: the
// pipeline recomputes it over the canonicalised text, so two routes agree on one digest rather than
// each trusting the other's arithmetic (docs/02 §4.2).
type extensionExtractor struct{}

func (extensionExtractor) Extract(payload []byte, _ string) (string, []dedup.Attachment, error) {
	if len(payload) == 0 {
		return "", nil, errors.New("native host: no payload to canonicalise")
	}
	// C2 decode: ill-formed sequences become U+FFFD so the digest is deterministic, never lossy in
	// a way that silently changes identity.
	return dedup.Decode(payload), nil, nil
}

// runNativeHost is Chromium's entry point: read frames from stdin, answer on stdout, and never
// write anything else to stdout.
func runNativeHost(cfg Config, log *slog.Logger) error {
	svc, err := newService(context.Background(), cfg, log)
	if err != nil {
		return err
	}
	ctx, cancel := signalContext()
	defer cancel()
	if err := svc.Start(ctx); err != nil {
		return err
	}
	defer func() { _ = svc.Stop(context.Background()) }()

	host := svc.nativeHost()
	log.Info("native-messaging host ready", "framing", "4-byte little-endian length prefix")
	for {
		payload, err := readNativeFrame(os.Stdin)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil // the browser closed the channel
			}
			return err
		}
		response := host.Handle(ctx, payload)
		if err := writeNativeFrame(os.Stdout, response); err != nil {
			return err
		}
	}
}

// runNativeFrames feeds the golden case files through the real framing and dispatch. It exists so
// the byte-level transport is exercised without a browser: each case's frame is written as a real
// Chromium frame, read back through the real reader, and handled by the real host.
func runNativeFrames(cfg Config, log *slog.Logger, dir string) error {
	svc, err := newService(context.Background(), cfg, log)
	if err != nil {
		return err
	}
	ctx, cancel := signalContext()
	defer cancel()
	if err := svc.Start(ctx); err != nil {
		return err
	}
	host := svc.nativeHost()
	results, err := driveGoldenFrames(ctx, host, dir, os.Stdout)
	if err != nil {
		_ = svc.Stop(context.Background())
		return err
	}
	if err := svc.Stop(context.Background()); err != nil {
		return err
	}
	if results.failed > 0 {
		return fmt.Errorf("%d of %d golden frames failed", results.failed, results.total)
	}
	return nil
}

// frameResults is a small tally, so a failure is a count rather than a paragraph.
type frameResults struct {
	total    int
	acked    int
	refused  int
	failed   int
	spoolSeq map[string]uint64
}

// goldenCase is the shape of the case files under device/integration/testdata/native/.
type goldenCase struct {
	Name  string          `json:"name"`
	Why   string          `json:"why"`
	Frame json.RawMessage `json:"frame"`
}

// driveGoldenFrames runs every case file in dir through the framing and the host, printing one line
// per case and returning the tally. The write/read round trip is deliberate: it is the only part of
// this path that a browser would otherwise be needed to exercise.
func driveGoldenFrames(ctx context.Context, host *nativeHost, dir string, out io.Writer) (frameResults, error) {
	res := frameResults{spoolSeq: map[string]uint64{}}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return res, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return res, err
		}
		var c goldenCase
		if err := json.Unmarshal(raw, &c); err != nil {
			return res, fmt.Errorf("%s: %w", name, err)
		}
		payload := compactJSON(c.Frame)

		// Write it as Chromium would, read it back with the real reader, then dispatch.
		var wire bytes.Buffer
		if err := writeNativeFrame(&wire, payload); err != nil {
			return res, fmt.Errorf("%s: framing: %w", name, err)
		}
		framed, err := readNativeFrame(bytes.NewReader(wire.Bytes()))
		if err != nil {
			return res, fmt.Errorf("%s: %w", name, err)
		}
		response := host.Handle(ctx, framed)

		var resp protocol.NativeMessage
		if err := json.Unmarshal(response, &resp); err != nil {
			return res, fmt.Errorf("%s: response is not a NativeMessage: %w", name, err)
		}
		body := summarizeResponse(resp)
		res.total++
		switch resp.Type {
		case protocol.TypeAck:
			res.acked++
		case protocol.TypeRefusal:
			res.refused++
		default:
			// A response that is neither an ack nor a refusal is a protocol defect.
			res.failed++
		}
		fmt.Fprintf(out, "  %-32s -> %-9s %s\n", c.Name, resp.Type, body)
		fmt.Fprintf(out, "      why: %s\n", c.Why)
	}
	return res, nil
}

// summarizeResponsePayload decodes a response payload and summarizes it, for call sites that hold
// the raw bytes.
func summarizeResponsePayload(payload []byte) string {
	var resp protocol.NativeMessage
	if err := json.Unmarshal(payload, &resp); err != nil {
		return "unparseable response: " + err.Error()
	}
	return summarizeResponse(resp)
}

func summarizeResponse(resp protocol.NativeMessage) string {
	switch resp.Type {
	case protocol.TypeAck:
		var ack protocol.Ack
		if err := json.Unmarshal(resp.Body, &ack); err == nil {
			return ack.Detail
		}
	case protocol.TypeRefusal:
		var ref protocol.Refusal
		if err := json.Unmarshal(resp.Body, &ref); err == nil {
			return string(ref.Reason) + ": " + ref.Message
		}
	case protocol.TypePolicyBundle:
		var pb protocol.PolicyBundleMessage
		if err := json.Unmarshal(resp.Body, &pb); err == nil {
			return fmt.Sprintf("policy version %q unchanged=%v bundle_bytes=%d", pb.PolicyVersion, pb.Unchanged, len(pb.Bundle))
		}
	case protocol.TypeModeAnswer:
		var ma protocol.ModeAnswer
		if err := json.Unmarshal(resp.Body, &ma); err == nil {
			return fmt.Sprintf("mode %s policy %s reason %s", ma.Mode, ma.PolicyVersion, ma.Reason)
		}
	}
	return ""
}
