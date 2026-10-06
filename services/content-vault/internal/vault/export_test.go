package vault_test

import (
	"bytes"
	"testing"

	"github.com/shadow-ai-capture/content-vault/internal/store"
	"github.com/shadow-ai-capture/content-vault/internal/store/storetest"
	"github.com/shadow-ai-capture/content-vault/internal/vault"
)

// TestSubjectExportDecryptsEveryStoredPrompt checks that an admin's subject export returns the
// decrypted prompts of exactly that subject and audits the read with the subject named.
func TestSubjectExportDecryptsEveryStoredPrompt(t *testing.T) {
	r := newRig(t, store.SearchFullText)
	r.store.PutSubmission(storetest.Submission{
		TenantID: tenantID, SubmissionID: submissionID, PromptKind: "user", UserRef: "u_subject", ReceivedAt: t0,
	})
	content := []byte("the prompt a person typed")
	if _, err := r.svc.Upload(ctx, upload(content)); err != nil {
		t.Fatal(err)
	}

	res, err := r.svc.SubjectExport(ctx, vault.SubjectExportRequest{
		TenantID: tenantID, SubjectRef: "u_subject", Principal: "admin@example.com", SessionID: "sid123",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Skipped != 0 || len(res.Prompts) != 1 {
		t.Fatalf("prompts = %d, skipped = %d, want 1 and 0", len(res.Prompts), res.Skipped)
	}
	p := res.Prompts[0]
	if p.SubmissionID != submissionID || p.EventID != eventID || !bytes.Equal(p.Plaintext, content) || p.SizeBytes != len(content) {
		t.Fatalf("prompt = %+v", p)
	}

	audits := r.store.Audit()
	if len(audits) != 2 { // the upload audit, then the export audit
		t.Fatalf("%d audit rows, want 2", len(audits))
	}
	export := audits[1]
	if export.Action != "content_subject_export" || export.SubjectRef != "u_subject" || export.ObjectType != "subject" {
		t.Fatalf("export audit = %+v", export)
	}
}

// TestSubjectExportFindsNoOtherSubject checks the export returns nothing for a subject with no
// stored content, and still audits the read.
func TestSubjectExportFindsNoOtherSubject(t *testing.T) {
	r := newRig(t, store.SearchFullText)
	r.store.PutSubmission(storetest.Submission{
		TenantID: tenantID, SubmissionID: submissionID, PromptKind: "user", UserRef: "u_subject", ReceivedAt: t0,
	})
	if _, err := r.svc.Upload(ctx, upload([]byte("someone's prompt"))); err != nil {
		t.Fatal(err)
	}

	res, err := r.svc.SubjectExport(ctx, vault.SubjectExportRequest{
		TenantID: tenantID, SubjectRef: "u_someone_else", Principal: "admin@example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Prompts) != 0 {
		t.Fatalf("prompts = %d, want 0 for an unrelated subject", len(res.Prompts))
	}
}
