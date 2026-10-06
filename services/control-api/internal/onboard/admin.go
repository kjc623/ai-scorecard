package onboard

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/shadow-ai-capture/control-api/internal/identity"
	"github.com/shadow-ai-capture/control-api/internal/session"
)

// The vendor operator's two steps: create the product tenant, then issue the one-time
// onboarding link with the email domains the vendor knows belong to the customer. Domains are set
// here and never claimed by the customer, because a domain decides which tenant a work email signs in
// to.
//
// Both write with the RLS tenant set to the tenant being written, so they need no privilege beyond
// sac_control's, and both audit with the operator as the actor.

const (
	sqlAdminSetTenant = `SELECT set_config('app.tenant_id', $1, true)`

	sqlAdminCreateTenant = `
INSERT INTO ops.tenant (tenant_id, name, status, residency_region, ceiling_mode)
VALUES ($1::uuid, $2::text, 'active', $3::text, $4::text)`

	sqlAdminTenantName = `SELECT name FROM ops.tenant WHERE tenant_id = $1::uuid`

	sqlAdminAddDomain = `
INSERT INTO ops.tenant_email_domain (domain, tenant_id, created_by)
VALUES ($1::text, $2::uuid, $3::text)
ON CONFLICT (domain) DO NOTHING`

	// The definer function sees every tenant's domains, which is how a domain held by another tenant
	// is told apart from one this tenant already has.
	sqlAdminDomainOwner = `SELECT ops.tenant_for_email_domain($1::text)::text`

	sqlAdminCreateInvite = `
INSERT INTO ops.onboarding_invite (tenant_id, token_hash, created_by, created_at, expires_at)
VALUES ($1::uuid, $2::text, $3::text, $4::timestamptz, $5::timestamptz)
RETURNING invite_id::text`

	sqlAdminAudit = `
INSERT INTO ops.audit (tenant_id, actor_type, actor_id, action, object_type, object_id, detail, occurred_at)
VALUES ($1::uuid, 'user', $2::text, $3::text, $4::text, $5::text, $6::jsonb, $7::timestamptz)`
)

// AdminStatement pairs a statement with what it is for, for the live test.
type AdminStatement struct {
	Name, Purpose, SQL string
}

// AdminStatements is every statement the operator commands issue.
var AdminStatements = []AdminStatement{
	{"set_tenant", "RLS session tenant (transaction-local)", sqlAdminSetTenant},
	{"create_tenant", "tenant create", sqlAdminCreateTenant},
	{"tenant_name", "tenant invite: the tenant exists", sqlAdminTenantName},
	{"add_domain", "vendor-set SSO discovery domain", sqlAdminAddDomain},
	{"domain_owner", "a domain held by another tenant", sqlAdminDomainOwner},
	{"create_invite", "one-time onboarding invite", sqlAdminCreateInvite},
	{"audit", "operator actions", sqlAdminAudit},
}

// NewTenant is `control-api tenant create`.
type NewTenant struct {
	Name, Region, CeilingMode string
	// Actor is the vendor operator, recorded as vendor:<actor>.
	Actor string
	Now   time.Time
}

// CreateTenant inserts the tenant and returns its new id.
func CreateTenant(ctx context.Context, db *sql.DB, t NewTenant) (string, error) {
	t.Name, t.Region = strings.TrimSpace(t.Name), strings.TrimSpace(t.Region)
	switch {
	case t.Name == "":
		return "", errors.New("onboard: a tenant needs a name")
	case t.Region == "":
		return "", errors.New("onboard: a tenant needs a residency region")
	case t.CeilingMode != "m0" && t.CeilingMode != "m1" && t.CeilingMode != "m2" && t.CeilingMode != "m3":
		return "", fmt.Errorf("onboard: ceiling %q is not m0, m1, m2 or m3", t.CeilingMode)
	case strings.TrimSpace(t.Actor) == "":
		return "", errors.New("onboard: the operator must be named")
	}
	if t.Now.IsZero() {
		t.Now = time.Now()
	}
	id, err := session.NewUUID()
	if err != nil {
		return "", err
	}
	err = adminTx(ctx, db, id, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, sqlAdminCreateTenant, id, t.Name, t.Region, t.CeilingMode); err != nil {
			return fmt.Errorf("insert tenant: %w", err)
		}
		return adminAudit(ctx, tx, id, t.Actor, "tenant.create", "tenant", id, map[string]any{
			"name": t.Name, "region": t.Region, "ceiling_mode": t.CeilingMode,
		}, t.Now)
	})
	if err != nil {
		return "", fmt.Errorf("onboard: create tenant: %w", err)
	}
	return id, nil
}

// NewInvite is `control-api tenant invite`.
type NewInvite struct {
	TenantID  string
	Domains   []string
	TTL       time.Duration
	PublicURL string
	Actor     string
	Now       time.Time
}

// IssuedInvite is the result: the URL is shown once and never stored.
type IssuedInvite struct {
	InviteID  string
	URL       string
	ExpiresAt time.Time
}

// MaxInviteTTL bounds an invite's life: it is a bearer credential to become a tenant's first admin.
const MaxInviteTTL = 30 * 24 * time.Hour

// domainRE is ops.tenant_email_domain's CHECK with label lengths bounded, so a bad domain is refused
// here with its name rather than by the constraint.
var domainRE = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)+$`)

// NormalizeDomain lower-cases and checks an email domain.
func NormalizeDomain(d string) (string, error) {
	d = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(d), "@"))), ".")
	if len(d) > 253 || !domainRE.MatchString(d) {
		return "", fmt.Errorf("onboard: %q is not an email domain", d)
	}
	return d, nil
}

// CreateInvite records the domains and a fresh invite, in one transaction, and returns the URL.
func CreateInvite(ctx context.Context, db *sql.DB, in NewInvite) (IssuedInvite, error) {
	if !session.IsUUID(in.TenantID) {
		return IssuedInvite{}, fmt.Errorf("onboard: tenant %q is not a uuid", in.TenantID)
	}
	tenant := strings.ToLower(in.TenantID)
	u, err := url.Parse(strings.TrimRight(in.PublicURL, "/"))
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return IssuedInvite{}, fmt.Errorf("onboard: public URL %q is not an absolute http(s) URL (SAC_PUBLIC_URL)", in.PublicURL)
	}
	if in.TTL <= 0 {
		in.TTL = 7 * 24 * time.Hour
	}
	if in.TTL > MaxInviteTTL {
		return IssuedInvite{}, fmt.Errorf("onboard: an invite may live at most %s", MaxInviteTTL)
	}
	if strings.TrimSpace(in.Actor) == "" {
		return IssuedInvite{}, errors.New("onboard: the operator must be named")
	}
	var domains []string
	for _, d := range in.Domains {
		n, err := NormalizeDomain(d)
		if err != nil {
			return IssuedInvite{}, err
		}
		domains = append(domains, n)
	}
	if in.Now.IsZero() {
		in.Now = time.Now()
	}
	token, err := identity.MintInviteToken(tenant)
	if err != nil {
		return IssuedInvite{}, err
	}
	out := IssuedInvite{ExpiresAt: in.Now.UTC().Add(in.TTL)}
	err = adminTx(ctx, db, tenant, func(tx *sql.Tx) error {
		var name string
		if err := tx.QueryRowContext(ctx, sqlAdminTenantName, tenant).Scan(&name); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("no tenant %s", tenant)
			}
			return err
		}
		for _, d := range domains {
			res, err := tx.ExecContext(ctx, sqlAdminAddDomain, d, tenant, "vendor:"+in.Actor)
			if err != nil {
				return fmt.Errorf("domain %s: %w", d, err)
			}
			if n, _ := res.RowsAffected(); n == 1 {
				if err := adminAudit(ctx, tx, tenant, in.Actor, "tenant_email_domain.add", "tenant_email_domain", d, nil, in.Now); err != nil {
					return err
				}
				continue
			}
			var owner sql.NullString
			if err := tx.QueryRowContext(ctx, sqlAdminDomainOwner, d).Scan(&owner); err != nil {
				return fmt.Errorf("domain %s: %w", d, err)
			}
			if !strings.EqualFold(owner.String, tenant) {
				return fmt.Errorf("domain %s already belongs to another tenant", d)
			}
		}
		if err := tx.QueryRowContext(ctx, sqlAdminCreateInvite, tenant, identity.HashToken(token), "vendor:"+in.Actor,
			in.Now.UTC(), out.ExpiresAt).Scan(&out.InviteID); err != nil {
			return fmt.Errorf("insert invite: %w", err)
		}
		return adminAudit(ctx, tx, tenant, in.Actor, "onboarding_invite.create", "onboarding_invite", out.InviteID,
			map[string]any{"expires_at": out.ExpiresAt.Format(time.RFC3339), "domains": domains}, in.Now)
	})
	if err != nil {
		return IssuedInvite{}, fmt.Errorf("onboard: create invite: %w", err)
	}
	out.URL = u.String() + PathPrefix + token
	return out, nil
}

func adminTx(ctx context.Context, db *sql.DB, tenantID string, fn func(*sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, sqlAdminSetTenant, tenantID); err != nil {
		return fmt.Errorf("set tenant: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func adminAudit(ctx context.Context, tx *sql.Tx, tenantID, actor, action, objectType, objectID string, detail map[string]any, at time.Time) error {
	if detail == nil {
		detail = map[string]any{}
	}
	b, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, sqlAdminAudit, tenantID, "vendor:"+actor, action, objectType, objectID, string(b), at.UTC()); err != nil {
		return fmt.Errorf("audit %s: %w", action, err)
	}
	return nil
}
