package directory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// The statements of the user-reference key store. Each runs in a transaction that sets the
// row-level-security tenant first; ops.tenant is under forced RLS, so a session with no tenant reads
// and writes nothing.
const (
	sqlSetTenant = `SELECT set_config('app.tenant_id', $1, true)`

	// sqlSealedUserRefKey reads the tenant's sealed user-reference key; NULL until first minted.
	sqlSealedUserRefKey = `SELECT user_ref_key_enc FROM ops.tenant WHERE tenant_id = $1::uuid`

	// sqlInitUserRefKey stores a proposed key only where none is stored, and returns the stored one.
	// Under READ COMMITTED a second concurrent writer blocks on the first's row lock, then re-reads
	// the committed row, so COALESCE keeps the first key and both callers get it back: the race is
	// decided by the database, not by the callers.
	sqlInitUserRefKey = `
UPDATE ops.tenant
   SET user_ref_key_enc = COALESCE(user_ref_key_enc, $2::bytea)
 WHERE tenant_id = $1::uuid
RETURNING user_ref_key_enc`
)

// Statements is every statement the key store issues, for the live test.
var Statements = []string{sqlSetTenant, sqlSealedUserRefKey, sqlInitUserRefKey}

// KeyStore is the PostgreSQL UserRefKeyStore over ops.tenant.user_ref_key_enc.
type KeyStore struct {
	db *sql.DB
}

// NewKeyStore wraps an open pool; the caller owns it.
func NewKeyStore(db *sql.DB) *KeyStore { return &KeyStore{db: db} }

// SealedUserRefKey implements UserRefKeyStore.
func (s *KeyStore) SealedUserRefKey(ctx context.Context, tenantID string) ([]byte, error) {
	var sealed []byte
	err := s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, sqlSealedUserRefKey, tenantID).Scan(&sealed)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrUnknownTenant
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return sealed, nil
}

// InitUserRefKey implements UserRefKeyStore.
func (s *KeyStore) InitUserRefKey(ctx context.Context, tenantID string, sealed []byte) ([]byte, error) {
	var stored []byte
	err := s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, sqlInitUserRefKey, tenantID, sealed).Scan(&stored)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrUnknownTenant
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return stored, nil
}

func (s *KeyStore) withTenant(ctx context.Context, tenantID string, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("directory: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, sqlSetTenant, tenantID); err != nil {
		return fmt.Errorf("directory: set tenant: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}
