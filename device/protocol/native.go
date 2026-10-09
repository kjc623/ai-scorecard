package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Native messaging between the browser extension and capture-core.
//
// Chromium enforces a message size ceiling and the extension cannot read the spool, so
// observations flow one way, attachment bytes flow chunked behind a manifest that lets
// capture-core refuse an oversized upload before transfer, and anything undeliverable is held in
// extension memory only, bounded, dropped oldest-first with a counter.
//
// Every message is one JSON object with a `type` discriminator. A message whose type is
// unknown is counted and dropped; it is never guessed at.

// NativeMessage is the envelope every native-messaging message shares.
type NativeMessage struct {
	Type    string          `json:"type"`
	Version byte            `json:"version"`
	ID      string          `json:"id,omitempty"` // correlation id for request/response pairs
	Body    json.RawMessage `json:"body,omitempty"`
}

// Native message types, extension -> capture-core.
const (
	TypeObservation        = "observation"         // one observed submission, already shape-classified
	TypeAttachmentManifest = "attachment_manifest" // descriptor first: refuse before transfer
	TypeAttachmentChunk    = "attachment_chunk"
	TypeAttachmentComplete = "attachment_complete"
	TypeHealth             = "health"          // the extension's own coverage row
	TypePolicySync         = "policy_sync"     // ask for the current bundle / version
	TypeModeQuery          = "mode_query"      // ask for a destination's effective mode
	TypeDecisionRecord     = "decision_record" // a locally decided warn/block, including blocked requests
)

// Native message types, capture-core -> extension.
const (
	TypeAck            = "ack"
	TypeRefusal        = "refusal" // typed, with a reason from the closed set
	TypePolicyBundle   = "policy_bundle"
	TypeModeAnswer     = "mode_answer"
	TypeHealthSnapshot = "health_snapshot"
)

// Native message types between capture-core and its user-session helper, a capture-core process
// the service starts in each signed-in session. They travel on the same endpoint as the browser
// relay; the extension never sends or receives them.
const (
	TypeHelperHello  = "helper_hello"  // helper -> capture-core, the connection's first frame
	TypeNotify       = "notify"        // capture-core -> helper: show one notification
	TypeNotifyResult = "notify_result" // helper -> capture-core: whether it was shown
)

// Native message types between a tool's hook command (capture-core --hook) and capture-core. The
// hook sends one hook_evaluate on its own connection and waits for the hook_decision; the
// extension never sends or receives them.
const (
	TypeHookEvaluate = "hook_evaluate" // hook -> capture-core: a prompt the tool is about to send
	TypeHookDecision = "hook_decision" // capture-core -> hook: what the tool does with it
)

// RefusalReason is the closed set of reasons capture-core refuses an extension message. A
// refusal is never silent and never a partial acceptance.
type RefusalReason string

const (
	RefusalAttachmentTooLarge RefusalReason = "attachment_too_large"
	RefusalModeForbidsRead    RefusalReason = "mode_forbids_read"
	RefusalUnknownType        RefusalReason = "unknown_type"
	RefusalVersionMismatch    RefusalReason = "version_mismatch"
	RefusalMalformed          RefusalReason = "malformed"
	RefusalChannelClosing     RefusalReason = "channel_closing"
	RefusalQueueFull          RefusalReason = "queue_full"
)

// ObservationMessage is one observation delivered by the extension. It carries the fields the
// extension can see and nothing it cannot: capture-core mints the envelope, because the
// envelope needs the device's identity, the effective mode and the spool sequence.
type ObservationMessage struct {
	// ClientID correlates this observation with its later attachment chunks and decision.
	ClientID string `json:"client_id"`

	// Route is which extension mechanism observed it: web_request, page_context or dom.
	Route Route `json:"route"`

	// ToolFingerprint is behaviour-derived, computed by the shape predicate, never a brand name.
	ToolFingerprint string `json:"tool_fingerprint"`

	OccurredAt        time.Time `json:"occurred_at"`
	MonotonicOffsetMS int64     `json:"monotonic_offset_ms"`
	SizeBytes         int64     `json:"size_bytes"`
	ContentDigest     string    `json:"content_digest,omitempty"`

	// HasContent is true when the extension is entitled to hand over the payload at this mode.
	// At M0 it is false and no body field may be populated; capture-core rejects the message if
	// the two disagree, so a defect that read content at M0 is caught rather than stored.
	HasContent bool `json:"has_content"`

	// Content is the strict-UTF-8-decoded payload, or the raw bytes when the decode failed.
	// Deliberately absent at M0.
	Content []byte `json:"content,omitempty"`

	// ContentIsBinary records that the payload was treated as binary rather than lossily
	// decoded: a lossy decode would corrupt the digest and break dedup across routes.
	ContentIsBinary bool `json:"content_is_binary,omitempty"`

	// OverCap records that the body exceeded the cap, so it was hashed and sized and the
	// classification is `confidence: degraded` rather than absent.
	OverCap bool `json:"over_cap,omitempty"`

	// Attachments carries descriptors the extension already knows. Bytes follow separately.
	Attachments []AttachmentDescriptor `json:"attachments,omitempty"`

	// Decision is present when the extension decided inline. A blocked request is still an
	// event: otherwise the product could not answer "what did we stop".
	Decision *Decision `json:"decision,omitempty"`

	// DegradedReason is set when the extension's inline evaluation failed or exceeded budget
	// and the request was allowed through (fail open).
	DegradedReason Detail `json:"degraded_reason,omitempty"`

	// PageContextAttachments reports whether a File handle was actually reachable. A filename
	// alone is not attachment capture, so this is recorded as `content_no_attachments`.
	PageContextAttachments bool `json:"page_context_attachments,omitempty"`
}

// Validate rejects an observation that contradicts itself. The M0 rule is the important one:
// content present at a mode that forbids reading it is a defect, not data.
func (o ObservationMessage) Validate() error {
	switch o.Route {
	case RouteExtWebRequest, RouteExtPageContext, RouteExtDOM:
	default:
		return errRefusal(RefusalMalformed, "route %q is not an extension route", o.Route)
	}
	if o.ToolFingerprint == "" {
		return errRefusal(RefusalMalformed, "observation has no tool fingerprint")
	}
	if !o.HasContent && len(o.Content) > 0 {
		return errRefusal(RefusalMalformed, "observation carries %d content bytes while has_content is false", len(o.Content))
	}
	if o.Decision != nil {
		switch o.Decision.Action {
		case ActionBlocked, ActionWarned, ActionLogged:
		default:
			return errRefusal(RefusalMalformed, "decision action %q outside the closed set", o.Decision.Action)
		}
	}
	for _, a := range o.Attachments {
		if a.Name == "" {
			return errRefusal(RefusalMalformed, "attachment descriptor without a name")
		}
		if a.SizeBytes > MaxAttachmentBytes {
			return errRefusal(RefusalAttachmentTooLarge, "attachment %q is %d bytes, cap is %d", a.Name, a.SizeBytes, MaxAttachmentBytes)
		}
	}
	return nil
}

// Refusal is the typed rejection of an extension message.
type Refusal struct {
	Reason  RefusalReason `json:"reason"`
	Message string        `json:"message"`
}

func errRefusal(r RefusalReason, format string, args ...any) error {
	return &RefusalError{Reason: r, Message: fmt.Sprintf(format, args...)}
}

// RefusalError carries the closed reason code alongside the prose.
type RefusalError struct {
	Reason  RefusalReason
	Message string
}

func (e *RefusalError) Error() string { return string(e.Reason) + ": " + e.Message }

// ModeQuery asks for a destination's effective mode so the inline decision can use policy the
// extension already holds instead of a round trip.
type ModeQuery struct {
	ToolFingerprint string `json:"tool_fingerprint"`
	Host            string `json:"host,omitempty"`
	MediaType       string `json:"media_type,omitempty"`
	SizeBytes       int64  `json:"size_bytes,omitempty"`
}

// ModeAnswer is the resolved effective mode, with the reason it resolved that way, so an
// operator can explain a decision without reading a policy bundle by hand.
type ModeAnswer struct {
	Mode          CollectionMode `json:"mode"`
	PolicyVersion string         `json:"policy_version"`
	Reason        string         `json:"reason,omitempty"`
}

// PolicySyncRequest asks for the bundle in force. The extension holds no durable state, so this is
// how it gets policy after a restart.
type PolicySyncRequest struct {
	KnownVersion string `json:"known_version,omitempty"` // 304-aware: the server answers "unchanged"
}

// PolicyBundleMessage carries the bundle in force to the extension. capture-core verifies the
// signed envelope; Bundle is its decoded payload, exactly the JSON object the signature covered,
// never the envelope. The extension trusts capture-core and does not verify it again.
type PolicyBundleMessage struct {
	PolicyVersion string          `json:"policy_version"`
	Bundle        json.RawMessage `json:"bundle"`
	Unchanged     bool            `json:"unchanged,omitempty"`
}

// Ack confirms a message was accepted. Acceptance is not delivery to the server: the spool is
// what makes that distinction, and it is why an ack never claims an event was ingested.
type Ack struct {
	ID     string `json:"id,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// MaxNativeMessageBytes is the effective ceiling the extension must assume for one native
// message. Chromium enforces its own limit; this constant is the one the extension queues
// against, and attachment bytes never travel whole because of it.
const MaxNativeMessageBytes = 1 << 20

// HelperHello opens a helper connection. capture-core accepts it only from the user signed in to
// SessionID, so one user's helper never receives another user's notifications.
type HelperHello struct {
	SessionID uint32 `json:"session_id"`
	PID       uint32 `json:"pid"`
}

// Notification limits, in characters (the link in bytes).
const (
	MaxNotifyTitle = 64
	MaxNotifyBody  = 280
	MaxNotifyLink  = 2048
)

// Notify asks the helper to show one notification to its session's user. Link, when present, is
// an https URL the notification offers to open.
type Notify struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	Link  string `json:"link,omitempty"`
}

// Validate refuses a notification the helper could not show as written: an empty or overlong
// title or body, a control character, or a link that is not an absolute https URL.
func (n Notify) Validate() error {
	if err := notifyText("title", n.Title, MaxNotifyTitle, false); err != nil {
		return err
	}
	if err := notifyText("body", n.Body, MaxNotifyBody, true); err != nil {
		return err
	}
	if n.Link == "" {
		return nil
	}
	if len(n.Link) > MaxNotifyLink {
		return fmt.Errorf("notify: the link is %d bytes, over the %d limit", len(n.Link), MaxNotifyLink)
	}
	u, err := url.Parse(n.Link)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return errors.New("notify: the link must be an absolute https URL")
	}
	return nil
}

func notifyText(field, s string, limit int, newlines bool) error {
	if !utf8.ValidString(s) {
		return fmt.Errorf("notify: the %s is not UTF-8", field)
	}
	if strings.TrimSpace(s) == "" {
		return fmt.Errorf("notify: the %s is empty", field)
	}
	if n := utf8.RuneCountInString(s); n > limit {
		return fmt.Errorf("notify: the %s is %d characters, over the %d limit", field, n, limit)
	}
	for _, r := range s {
		if unicode.IsControl(r) && !(newlines && r == '\n') {
			return fmt.Errorf("notify: the %s carries a control character", field)
		}
	}
	return nil
}

// NotifyResult answers one Notify, correlated by the message id. Error says why it was not shown.
type NotifyResult struct {
	Shown bool   `json:"shown"`
	Error string `json:"error,omitempty"`
}

// MaxHookPromptBytes is the most prompt text one hook_evaluate carries. A longer prompt, or one
// whose frame would exceed the endpoint's frame limit once encoded, is sent as its length only,
// with OverCap set.
const MaxHookPromptBytes = 256 << 10

// Hook field limits, in bytes.
const (
	maxHookEvent         = 64
	maxHookSessionID     = 256
	maxHookToolName      = 128
	maxHookCwd           = 4096
	maxHookClientVersion = 64
	maxHookRuleID        = 128
)

var hookToolKey = regexp.MustCompile(`^[a-z][a-z0-9_]{1,63}$`)

// HookEvaluate is one prompt a tool's hook hands capture-core before the tool sends it. Tool is
// the tool key (claude_code, cursor, ...) and Event the tool's own name for the hook event.
type HookEvaluate struct {
	Tool       string `json:"tool"`
	Event      string `json:"event"`
	SessionID  string `json:"session_id"`
	PromptText string `json:"prompt_text"`
	// PromptBytes is the prompt's length in bytes: the length of PromptText or, when OverCap is
	// set, of the prompt that was not sent.
	PromptBytes   int64  `json:"prompt_bytes"`
	OverCap       bool   `json:"over_cap,omitempty"`
	ToolName      string `json:"tool_name,omitempty"`
	Cwd           string `json:"cwd,omitempty"`
	ClientVersion string `json:"client_version,omitempty"`
}

// CapPrompt sets PromptBytes from PromptText and, when the text is longer than
// MaxHookPromptBytes, drops it and sets OverCap.
func (h *HookEvaluate) CapPrompt() {
	h.PromptBytes = int64(len(h.PromptText))
	h.OverCap = h.PromptBytes > MaxHookPromptBytes
	if h.OverCap {
		h.PromptText = ""
	}
}

// Validate refuses a hook_evaluate whose fields are missing, overlong or contradict each other.
// Its errors never quote the prompt.
func (h HookEvaluate) Validate() error {
	if !hookToolKey.MatchString(h.Tool) {
		return errors.New("hook_evaluate: the tool is not a tool key")
	}
	for _, f := range []struct {
		name, value string
		limit       int
		required    bool
	}{
		{"event", h.Event, maxHookEvent, true},
		{"session_id", h.SessionID, maxHookSessionID, true},
		{"tool_name", h.ToolName, maxHookToolName, false},
		{"cwd", h.Cwd, maxHookCwd, false},
		{"client_version", h.ClientVersion, maxHookClientVersion, false},
	} {
		if f.required && f.value == "" {
			return fmt.Errorf("hook_evaluate: the %s is empty", f.name)
		}
		if len(f.value) > f.limit {
			return fmt.Errorf("hook_evaluate: the %s is %d bytes, over the %d limit", f.name, len(f.value), f.limit)
		}
	}
	switch {
	case h.OverCap && (h.PromptText != "" || h.PromptBytes <= 0):
		return errors.New("hook_evaluate: an over-cap prompt carries only its length")
	case !h.OverCap && len(h.PromptText) > MaxHookPromptBytes:
		return fmt.Errorf("hook_evaluate: the prompt is %d bytes, over the %d limit", len(h.PromptText), MaxHookPromptBytes)
	case !h.OverCap && h.PromptBytes != int64(len(h.PromptText)):
		return errors.New("hook_evaluate: prompt_bytes is not the prompt's length")
	}
	return nil
}

// HookAction is what the tool does with the prompt.
type HookAction string

// The closed set of hook actions.
const (
	HookAllow HookAction = "allow"
	HookWarn  HookAction = "warn"
	HookBlock HookAction = "block"
)

// HookDecision answers one hook_evaluate. Message and Link are the matching rule's, for the tool
// to show; RuleID names the rule, policy.default when none matched.
type HookDecision struct {
	Action  HookAction `json:"action"`
	Message string     `json:"message"`
	Link    string     `json:"link"`
	RuleID  string     `json:"rule_id"`
}

// Validate refuses a decision outside the closed actions, or with an overlong message, link or
// rule id.
func (d HookDecision) Validate() error {
	switch d.Action {
	case HookAllow, HookWarn, HookBlock:
	default:
		return fmt.Errorf("hook_decision: action %q outside the closed set", d.Action)
	}
	if n := utf8.RuneCountInString(d.Message); n > MaxNotifyBody {
		return fmt.Errorf("hook_decision: the message is %d characters, over the %d limit", n, MaxNotifyBody)
	}
	if len(d.Link) > MaxNotifyLink {
		return fmt.Errorf("hook_decision: the link is %d bytes, over the %d limit", len(d.Link), MaxNotifyLink)
	}
	if len(d.RuleID) > maxHookRuleID {
		return fmt.Errorf("hook_decision: the rule id is %d bytes, over the %d limit", len(d.RuleID), maxHookRuleID)
	}
	return nil
}
