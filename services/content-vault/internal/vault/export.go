package vault

import (
	"context"
	"errors"

	"github.com/shadow-ai-capture/device/protocol"

	"github.com/shadow-ai-capture/content-vault/internal/keyring"
	"github.com/shadow-ai-capture/content-vault/internal/store"
)

// SubjectExportRequest is an admin's request to read every stored prompt of one subject.
type SubjectExportRequest struct {
	TenantID   string
	SubjectRef string
	// Principal is the admin, from their verified token.
	Principal string
	SessionID string
}

// SubjectPrompt is one decrypted stored prompt.
type SubjectPrompt struct {
	EventID      string
	SubmissionID string
	PromptKind   string
	RawDigest    string
	SizeBytes    int
	Plaintext    []byte
}

// SubjectExportResult is the decrypted content of one subject.
type SubjectExportResult struct {
	// Prompts lists the decrypted objects, oldest first. Objects sealed under a key version the
	// keyring no longer holds are left out, and `Skipped` states how many.
	Prompts []SubjectPrompt
	Skipped int
}

// SubjectExport decrypts every stored prompt of one subject. The outcome is audited in the same
// transaction that serves it.
func (s *Service) SubjectExport(ctx context.Context, req SubjectExportRequest) (SubjectExportResult, error) {
	now := s.now().UTC()
	var res SubjectExportResult
	err := s.store.InTenant(ctx, req.TenantID, func(tx store.Tx) error {
		t, err := tenant(ctx, tx)
		if err != nil {
			return err
		}
		if t.Status == "closed" || !t.ReadEnabled {
			return deny(ReasonRetrievalDisabled, "content reads are disabled for tenant %s", t.TenantID)
		}
		contents, err := tx.ContentForSubject(ctx, req.SubjectRef)
		if err != nil {
			return err
		}
		prompts := make([]SubjectPrompt, 0, len(contents))
		skipped := 0
		for _, c := range contents {
			plaintext, err := s.keys.Open(c.TenantID, c.ObjectID, c.KeyVersion, c.Ciphertext)
			if errors.Is(err, keyring.ErrUnknownVersion) {
				skipped++
				continue
			}
			if err != nil {
				return err
			}
			if protocol.RawDigest(plaintext) != c.RawDigest {
				return errors.New("vault: decrypted content does not match its recorded digest")
			}
			prompts = append(prompts, SubjectPrompt{
				EventID: c.EventID, SubmissionID: c.SubmissionID, PromptKind: c.PromptKind,
				RawDigest: c.RawDigest, SizeBytes: len(plaintext), Plaintext: plaintext,
			})
		}
		detail := map[string]any{"prompts": len(prompts), "stored_objects": len(contents)}
		if skipped > 0 {
			detail["undecryptable"] = skipped
		}
		if req.SessionID != "" {
			detail["sid"] = req.SessionID
		}
		if err := tx.AppendAudit(ctx, store.AuditEntry{
			ActorType: "user", ActorID: req.Principal, Action: ActionSubjectExport, ObjectType: "subject",
			SubjectRef: req.SubjectRef, Detail: detail, OccurredAt: now,
		}); err != nil {
			return err
		}
		res = SubjectExportResult{Prompts: prompts, Skipped: skipped}
		return nil
	})
	return res, err
}
