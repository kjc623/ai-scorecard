package directory

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Syncer is one sync pass over one tenant. It is deliberately not itself a Store or a Source: the
// three pieces are injected, so the same logic runs against the lab's file and against a fake store
// in tests. A customer's people arrive through SCIM instead (internal/scim), which writes the same
// ops.user_dim columns one person at a time; RetireMissing leaves those rows alone.
type Syncer struct {
	Source Source
	Store  Store
	Cipher *Cipher
	// Now defaults to time.Now. It is a field so a test can pin synced_at.
	Now func() time.Time
}

func (s *Syncer) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// Sync reads the directory once and reconciles ops.user_dim for one tenant.
//
// It is idempotent: running it twice writes the same rows with the same values. It is incremental in
// the only sense the data supports: rows are upserted per user_ref and never truncated and
// reinserted, so a person's row and its history survive every run. A user the source omits is
// retired rather than deleted. A read that returns no users at all is refused rather than acted on,
// because retiring every row from an empty read would turn a wrong file path or an empty export into
// a silent data loss; for the same reason a Source must fail on a partial read, never shorten it.
func (s *Syncer) Sync(ctx context.Context, tenantID string) (Result, error) {
	if s.Source == nil || s.Store == nil || s.Cipher == nil {
		return Result{}, fmt.Errorf("directory: syncer needs a source, a store and a cipher")
	}
	users, err := s.Source.List(ctx)
	if err != nil {
		return Result{}, err
	}
	if len(users) == 0 {
		return Result{}, fmt.Errorf("directory: source %q returned no users; refusing to retire every row from an empty read", s.Source.Name())
	}
	if err := validate(users); err != nil {
		return Result{}, err
	}
	identity, err := s.Store.DeviceIdentity(ctx, tenantID)
	if err != nil {
		return Result{}, err
	}
	storeNames := identity == "clear"

	at := s.now()
	out := Result{TenantID: tenantID}
	present := make([]string, 0, len(users))
	for _, u := range users {
		ref := strings.TrimSpace(u.UserRef)
		if ref == "" {
			// A directory user the mapping attribute does not name cannot be joined to the wire.
			// It is counted and left alone; it is not written under a blank key.
			out.Skipped++
			continue
		}
		var display *string
		if storeNames {
			display = nullable(u.DisplayName)
		}
		sealed, err := s.Cipher.Seal(tenantID, u.DirectoryID)
		if err != nil {
			return Result{}, err
		}
		department := nullable(u.Department)
		row := Row{
			UserRef:              ref,
			DirectoryObjectIDEnc: sealed,
			Department:           department,
			Population:           nullable(u.Population),
			ManagerRef:           nullable(u.ManagerRef),
			DisplayName:          display,
			Status:               normalizeStatus(u.Status),
			SyncedAt:             at,
		}
		if err := s.Store.UpsertUser(ctx, tenantID, row); err != nil {
			return Result{}, err
		}
		present = append(present, ref)
		out.Synced++
		if department == nil {
			out.Unmapped++
		}
	}
	out.Read = len(users)
	retired, err := s.Store.RetireMissing(ctx, tenantID, present)
	if err != nil {
		return Result{}, err
	}
	out.Retired = retired
	return out, nil
}
