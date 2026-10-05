package scim

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/shadow-ai-capture/control-api/internal/store"
)

// Tokens administers a tenant's SCIM tokens for the admin API (Settings → Deployment,
// internal/deploy). It owns the rows; the admin routes own the actor's authentication and the audit
// row, so a token write is audited once, by the component that knows who asked.
type Tokens struct {
	store Store
	// Now defaults to time.Now; a test pins it.
	Now func() time.Time
}

// NewTokens wires token administration over the SCIM store.
func NewTokens(st Store) *Tokens { return &Tokens{store: st, Now: time.Now} }

// TokenInfo is a token as the admin page lists it. The plaintext is never listed: it exists once,
// in Create's return.
type TokenInfo struct {
	TokenID   string
	Label     string
	CreatedBy string
	CreatedAt time.Time
	RevokedAt *time.Time
}

// MaxLabelLength bounds a token label; it is a name for the admin's own reference.
const MaxLabelLength = 120

func (t *Tokens) now() time.Time {
	if t.Now != nil {
		return t.Now().UTC()
	}
	return time.Now().UTC()
}

// Create mints a token for the tenant and returns its id and its plaintext, which is shown once.
func (t *Tokens) Create(ctx context.Context, tenantID, label, createdBy string) (tokenID, token string, err error) {
	createdBy = strings.TrimSpace(createdBy)
	if createdBy == "" {
		return "", "", fmt.Errorf("scim: a token is created by a named actor")
	}
	label = strings.TrimSpace(label)
	if utf8.RuneCountInString(label) > MaxLabelLength {
		return "", "", fmt.Errorf("scim: a token label is at most %d characters", MaxLabelLength)
	}
	plaintext, hash, err := MintToken(tenantID)
	if err != nil {
		return "", "", err
	}
	id, err := store.NewUUID()
	if err != nil {
		return "", "", err
	}
	err = t.store.InTenant(ctx, strings.ToLower(tenantID), func(tx Tx) error {
		if _, err := tx.DeviceIdentity(); err != nil {
			return err
		}
		return tx.InsertToken(TokenRow{ID: id, Hash: hash, Label: label, CreatedBy: createdBy, CreatedAt: t.now()})
	})
	if err != nil {
		return "", "", err
	}
	return id, plaintext, nil
}

// Revoke ends a token: from the next request on, it authenticates nothing. Revoking a revoked token
// is not an error; a token the tenant does not have is ErrNotFound.
func (t *Tokens) Revoke(ctx context.Context, tenantID, tokenID, revokedBy string) error {
	if strings.TrimSpace(revokedBy) == "" {
		return fmt.Errorf("scim: a token is revoked by a named actor")
	}
	if !store.IsUUID(tokenID) {
		return ErrNotFound
	}
	tokenID = strings.ToLower(tokenID)
	return t.store.InTenant(ctx, strings.ToLower(tenantID), func(tx Tx) error {
		revoked, err := tx.RevokeToken(tokenID, t.now())
		if err != nil || revoked {
			return err
		}
		_, err = tx.Token(tokenID)
		return err
	})
}

// List returns the tenant's tokens, newest first.
func (t *Tokens) List(ctx context.Context, tenantID string) ([]TokenInfo, error) {
	var out []TokenInfo
	err := t.store.InTenant(ctx, strings.ToLower(tenantID), func(tx Tx) error {
		rows, err := tx.ListTokens()
		if err != nil {
			return err
		}
		out = make([]TokenInfo, 0, len(rows))
		for _, r := range rows {
			out = append(out, TokenInfo{TokenID: r.ID, Label: r.Label, CreatedBy: r.CreatedBy, CreatedAt: r.CreatedAt, RevokedAt: r.RevokedAt})
		}
		return nil
	})
	return out, err
}

// IsNotFound lets a caller outside this package map Revoke's unknown token to its own not-found.
func IsNotFound(err error) bool { return errors.Is(err, ErrNotFound) }
