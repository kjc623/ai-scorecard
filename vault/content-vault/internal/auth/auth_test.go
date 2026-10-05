package auth_test

import (
	"net/http"
	"testing"

	"github.com/shadow-ai-capture/content-vault/internal/auth"
)

// TestHeaderAuthenticatorReadsTheSessionRoles is the wire half of the role boundary: the vault
// reads the roles its internal caller (query-api) set from the verified token, and an absent
// header yields no roles rather than a default.
func TestHeaderAuthenticatorReadsTheSessionRoles(t *testing.T) {
	a := auth.NewHeaderAuthenticator("query-api", "control-api", "ops")
	req, _ := http.NewRequest(http.MethodPost, "/v1/content/search", nil)
	req.Header.Set("X-Sac-Service", "query-api")
	req.Header.Set("X-Sac-Subject", "analyst@example.com")
	req.Header.Set("X-Sac-Tenant", "11111111-1111-4111-8111-111111111111")
	req.Header.Set("X-Sac-Roles", " analyst , content_reader ,,")

	p, err := a.Authenticate(req)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if !p.HasAnyRole("content_reader") {
		t.Fatalf("roles %v do not carry content_reader", p.Roles)
	}
	if p.HasAnyRole("admin") {
		t.Fatalf("roles %v wrongly carry admin", p.Roles)
	}
}

// TestNoRoleHeaderIsNoRole: a service caller (control-api on the device path) names no human
// role, and that must stay an empty list rather than an accidental grant.
func TestNoRoleHeaderIsNoRole(t *testing.T) {
	a := auth.NewHeaderAuthenticator("control-api")
	req, _ := http.NewRequest(http.MethodPost, "/v1/content/object", nil)
	req.Header.Set("X-Sac-Service", "control-api")
	req.Header.Set("X-Sac-Subject", "control-api")
	req.Header.Set("X-Sac-Tenant", "11111111-1111-4111-8111-111111111111")

	p, err := a.Authenticate(req)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if len(p.Roles) != 0 {
		t.Fatalf("a caller with no role header got roles %v", p.Roles)
	}
	if p.HasAnyRole("content_reader", "analyst") {
		t.Fatal("an empty role list must carry no role")
	}
}
