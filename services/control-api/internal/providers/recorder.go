package providers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/shadow-ai-capture/device/protocol"
	"github.com/shadow-ai-capture/device/protocol/canon"

	"github.com/shadow-ai-capture/control-api/internal/content"
	"github.com/shadow-ai-capture/control-api/internal/store"
)

// RouteProviderAPI is the route a provider's record arrives by (ref.route_fidelity).
const RouteProviderAPI = "provider.api"

// DefaultRuleID is the rule a decision names when no rule decided it, as a device records it.
const DefaultRuleID = "policy.default"

// envelopeSchemaVersion is the event envelope contract version a record is written to.
const envelopeSchemaVersion = "1.0"

// maxLabels is the envelope's cap on one event's labels.
const maxLabels = 64

// eventNamespace is the UUID namespace a record's event id is derived in, from its connection and
// the provider's id for it.
var eventNamespace = uuid.MustParse("4b0c1a52-7d3e-4f6a-9a8e-2c5d7e9f1b30")

// Policy is the tenant's settings a pass records under, read once per pass.
type Policy struct {
	CeilingMode string
	// Mode is the tenant's collection mode, the ceiling when none is set.
	Mode string
	// ScopeOverrides is the tenant's narrower per-tool modes, by ref.app key.
	ScopeOverrides map[string]string
	// ContentSearch is the tenant's content search setting; at "disabled" no prompt is stored.
	ContentSearch string
	CustomLabels  []store.CustomLabel
}

// ModeFor is the mode a prompt sent with app is collected at: the tool's override, else the
// tenant's mode.
func (p Policy) ModeFor(app string) protocol.CollectionMode {
	if m := p.ScopeOverrides[app]; m != "" {
		return protocol.CollectionMode(m)
	}
	return protocol.CollectionMode(p.Mode)
}

// storesContent reports whether a prompt collected at mode has its text stored in content-vault: at
// M3, under an M3 ceiling, for a tenant that stores prompts at all. These are the conditions on
// which control-api grants a device's upload.
func (p Policy) storesContent(mode protocol.CollectionMode) bool {
	return mode == protocol.ModeM3 && p.CeilingMode == string(protocol.ModeM3) &&
		p.ContentSearch != "" && p.ContentSearch != "disabled"
}

// Classifier classifies a prompt. *classify.Host implements it.
type Classifier interface {
	Classify(ctx context.Context, req protocol.ClassifyRequest) protocol.ClassifyResponse
}

// RecordStore is what recording needs of the database. Every method runs in the tenant's
// row-level-security session.
type RecordStore interface {
	// RecordEvent records an envelope through ingest.record_provider_event and returns its outcome
	// (inserted, merged or duplicate) and its submission.
	RecordEvent(ctx context.Context, tenantID string, envelope map[string]any) (outcome, submissionID string, err error)
	// ContentPending reports whether the submission is the connection's and still waits for its
	// content.
	ContentPending(ctx context.Context, tenantID, submissionID, connectionID string) (bool, error)
	// GrantContent records a granted upload of one event's content.
	GrantContent(ctx context.Context, tenantID string, g Grant) error
	// AddContentUsage adds stored content bytes to the tenant's usage for the day.
	AddContentUsage(ctx context.Context, tenantID string, bytes int64) error
}

// Grant is a granted upload of a provider record's content (ops.grant).
type Grant struct {
	GrantID      string
	EventID      string
	SubmissionID string
	ConnectionID string
	ExpiresAt    time.Time
}

// The outcomes ingest.record_provider_event reports.
const (
	OutcomeInserted  = "inserted"
	OutcomeMerged    = "merged"
	OutcomeDuplicate = "duplicate"
)

// Recorder records a connection's records.
type Recorder struct {
	Store      RecordStore
	Classifier Classifier
	Vault      content.Vault
	Now        func() time.Time
}

// skipped is a record the recorder cannot attribute or that carries nothing: it is counted and
// passed over, and the pass goes on.
type skipped struct{ reason string }

func (e skipped) Error() string { return "providers: record skipped: " + e.reason }

// Record classifies and records one record under p, with the tenant's user-reference key, and
// stores its content when p does. It returns the outcome. A skipped record is an error that
// IsSkipped reports; any other error is the product's failure and ends the pass.
func (r *Recorder) Record(ctx context.Context, c Connection, p Policy, userRefKey []byte, rec Record) (string, error) {
	if rec.ID == "" || rec.OccurredAt.IsZero() || rec.App == "" {
		return "", skipped{"the record has no id, time or tool"}
	}
	if rec.Text == "" && len(rec.Attachments) == 0 {
		return "", skipped{"the record carries no prompt"}
	}
	ref, err := userRef(userRefKey, rec)
	if err != nil {
		return "", skipped{err.Error()}
	}
	mode := p.ModeFor(rec.App)
	eventID := EventID(c.ConnectionID, rec.ID)
	env := map[string]any{
		"schema_version":      envelopeSchemaVersion,
		"event_id":            eventID,
		"tenant_id":           c.TenantID,
		"connection_id":       c.ConnectionID,
		"user_ref":            ref,
		"tool_fingerprint":    "app:" + rec.App,
		"direction":           "egress",
		"kind":                string(protocol.KindPrompt),
		"occurred_at":         rec.OccurredAt.UTC().Format(time.RFC3339Nano),
		"monotonic_offset_ms": 0,
		"source":              RouteProviderAPI,
		"collection_mode":     string(mode),
		"size_bytes":          len(rec.Text),
		"dedup_key":           dedupKey(c.ConnectionID, rec.ID),
		"policy_decision":     protocol.Decision{RuleID: DefaultRuleID, Action: protocol.ActionLogged},
	}
	// record_event keeps the name only for a tenant whose device_identity is clear.
	if rec.UPN != "" {
		env["subject_name"] = rec.UPN
	}
	if mode.ReadsContent() {
		if err := r.classify(ctx, p, mode, rec, env); err != nil {
			return "", err
		}
	}
	outcome, submission, err := r.Store.RecordEvent(ctx, c.TenantID, env)
	if err != nil {
		return "", recordError{err}
	}
	if !p.storesContent(mode) || rec.Text == "" || len(rec.Text) > protocol.MaxContentObjectBytes {
		return outcome, nil
	}
	// A record merged into a device's submission leaves the content to the device. One read again
	// is stored now if its first pass recorded it but did not store it.
	switch outcome {
	case OutcomeInserted:
	case OutcomeDuplicate:
		pending, err := r.Store.ContentPending(ctx, c.TenantID, submission, c.ConnectionID)
		if err != nil {
			return "", recordError{err}
		}
		if !pending {
			return outcome, nil
		}
	default:
		return outcome, nil
	}
	if err := r.storeContent(ctx, c, eventID, submission, []byte(rec.Text)); err != nil {
		return "", err
	}
	return outcome, nil
}

// IsSkipped reports whether err is a record the recorder passed over.
func IsSkipped(err error) bool {
	var s skipped
	return errors.As(err, &s)
}

// classify adds the content-derived fields of a prompt read at mode (M1 and above) to env.
func (r *Recorder) classify(ctx context.Context, p Policy, mode protocol.CollectionMode, rec Record, env map[string]any) error {
	req := protocol.ClassifyRequest{Content: []byte(rec.Text), Mode: mode, CustomLabels: classifyTerms(p.CustomLabels)}
	if err := req.Validate(); err != nil {
		return recordError{fmt.Errorf("classify request: %w", err)}
	}
	resp := r.Classifier.Classify(ctx, req)
	labels := append([]protocol.Label{}, resp.Labels...)
	for _, l := range fileLabels(p.CustomLabels, rec.Attachments) {
		if !slices.ContainsFunc(labels, func(x protocol.Label) bool { return x.Class == l && x.RuleID == "" }) {
			labels = append(labels, protocol.Label{Class: l, Score: protocol.CustomLabelScore})
		}
	}
	if len(labels) > maxLabels {
		labels = labels[:maxLabels]
	}
	env["content_digest"] = canon.Digest(rec.Text, rec.Attachments)
	env["labels"] = labels
	env["classifier_version"] = resp.ClassifierVersion
	env["confidence"] = string(resp.Confidence)
	env["prompt_kind"] = string(protocol.PromptKindUser)
	if mode == protocol.ModeM2 && resp.Excerpt != nil {
		env["content_excerpt"] = resp.Excerpt
	}
	if atts := descriptors(rec.Attachments); len(atts) > 0 {
		env["attachments"] = atts
	}
	return nil
}

// storeContent grants the upload of one record's content and hands it to content-vault, as a
// device's granted upload is handed over.
func (r *Recorder) storeContent(ctx context.Context, c Connection, eventID, submission string, body []byte) error {
	grantID, err := store.NewUUID()
	if err != nil {
		return recordError{err}
	}
	now := time.Now
	if r.Now != nil {
		now = r.Now
	}
	g := Grant{GrantID: grantID, EventID: eventID, SubmissionID: submission, ConnectionID: c.ConnectionID,
		ExpiresAt: now().UTC().Add(content.GrantTTL)}
	if err := r.Store.GrantContent(ctx, c.TenantID, g); err != nil {
		return recordError{err}
	}
	stored, err := r.Vault.Put(ctx, c.TenantID, eventID, grantID, protocol.RawDigest(body), body)
	if err != nil {
		return recordError{fmt.Errorf("content-vault: %w", err)}
	}
	if stored {
		if err := r.Store.AddContentUsage(ctx, c.TenantID, int64(len(body))); err != nil {
			return recordError{err}
		}
	}
	return nil
}

// EventID is the event id of a connection's record: the same record read again is the same event.
func EventID(connectionID, recordID string) string {
	return uuid.NewSHA1(eventNamespace, []byte(connectionID+canon.Sep+recordID)).String()
}

// dedupKey is the exact dedup key of a record no device captured: the record itself. A record
// whose prompt a device captured takes the device's key in record_provider_event instead.
func dedupKey(connectionID, recordID string) string {
	sum := sha256.Sum256([]byte("sac-provider-1" + canon.Sep + connectionID + canon.Sep + recordID))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// userRef derives the person's user_ref as a device does: from the UPN when the record has one,
// else from the Entra object id.
func userRef(key []byte, rec Record) (string, error) {
	if upn := strings.TrimSpace(rec.UPN); upn != "" {
		return protocol.DeriveUserRef(key, protocol.UserRefUPN, upn)
	}
	if oid := strings.TrimSpace(rec.ObjectID); oid != "" {
		return protocol.DeriveUserRef(key, protocol.UserRefOID, oid)
	}
	return "", errors.New("the record names no person")
}

// classifyTerms is the custom labels' words and customer names as the classifier takes them.
func classifyTerms(labels []store.CustomLabel) []protocol.CustomLabelTerms {
	var out []protocol.CustomLabelTerms
	for _, l := range labels {
		terms := append(append([]string{}, l.Words...), l.CustomerNames...)
		if len(terms) > 0 {
			out = append(out, protocol.CustomLabelTerms{Label: l.Label, Terms: terms})
		}
	}
	return out
}

// fileLabels is the custom labels an attached file carries by its extension or by a file name
// pattern that matches its whole name, in any case, as a device applies them.
func fileLabels(labels []store.CustomLabel, atts []canon.Attachment) []string {
	var out []string
	for _, l := range labels {
		if slices.ContainsFunc(atts, func(a canon.Attachment) bool { return matchesFile(l, a.Name) }) {
			out = append(out, l.Label)
		}
	}
	return out
}

func matchesFile(l store.CustomLabel, name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	if n == "" {
		return false
	}
	for _, ext := range l.FileExtensions {
		if strings.HasSuffix(n, "."+strings.ToLower(ext)) {
			return true
		}
	}
	for _, p := range l.FileNames {
		if ok, err := path.Match(strings.ToLower(p), n); err == nil && ok {
			return true
		}
	}
	return false
}

// descriptors is the attachments as the envelope describes them, within its limits.
func descriptors(atts []canon.Attachment) []protocol.AttachmentDescriptor {
	var out []protocol.AttachmentDescriptor
	for _, a := range atts {
		name := strings.TrimSpace(a.Name)
		if name == "" {
			continue
		}
		if utf8.RuneCountInString(name) > protocol.MaxAttachmentNameRunes {
			name = string([]rune(name)[:protocol.MaxAttachmentNameRunes])
		}
		out = append(out, protocol.AttachmentDescriptor{Name: name, MediaType: a.MediaType, SizeBytes: max(a.SizeBytes, 0)})
		if len(out) == protocol.MaxAttachments {
			break
		}
	}
	return out
}
