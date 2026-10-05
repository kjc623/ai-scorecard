package scim

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/shadow-ai-capture/device/protocol"
)

func TestTokenFormat(t *testing.T) {
	plaintext, hash, err := MintToken(strings.ToUpper(tenantA))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(plaintext, TokenPrefix+tenantA+".") {
		t.Fatalf("token = %s", plaintext)
	}
	if hash != HashToken(plaintext) || !strings.HasPrefix(hash, "sha256:") || len(hash) != len("sha256:")+64 {
		t.Fatalf("hash = %s", hash)
	}
	if tenant, err := ParseToken(plaintext); err != nil || tenant != tenantA {
		t.Fatalf("ParseToken = %s, %v", tenant, err)
	}
	other, _, _ := MintToken(tenantA)
	if other == plaintext {
		t.Fatal("two mints produced one token")
	}
	for _, bad := range []string{"", "sac1." + tenantA + ".x", TokenPrefix + "nope.abc", TokenPrefix + tenantA, TokenPrefix + tenantA + ".short"} {
		if _, err := ParseToken(bad); err == nil {
			t.Errorf("ParseToken(%q) accepted", bad)
		}
	}
	if _, _, err := MintToken("not-a-tenant"); err == nil {
		t.Fatal("a token was minted for a non-uuid tenant")
	}
}

func TestTokenAdministration(t *testing.T) {
	h := newHarness(t, Config{})
	ctx := context.Background()
	id, token, err := h.tokens.Create(ctx, tenantA, "  Entra provisioning  ", "admin@contoso.com")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(token, TokenPrefix+tenantA+".") || !isUUID(id) {
		t.Fatalf("created = %s %s", id, token)
	}
	if _, _, err := h.tokens.Create(ctx, tenantA, "x", " "); err == nil {
		t.Fatal("a token was created with no actor")
	}
	if _, _, err := h.tokens.Create(ctx, tenantA, strings.Repeat("l", MaxLabelLength+1), "admin@contoso.com"); err == nil {
		t.Fatal("an over-long label was accepted")
	}
	if _, _, err := h.tokens.Create(ctx, "cccccccc-0000-4000-8000-00000000000c", "x", "admin@contoso.com"); err == nil {
		t.Fatal("a token was created for an unknown tenant")
	}
	list, err := h.tokens.List(ctx, tenantA)
	if err != nil || len(list) != 1 || list[0].Label != "Entra provisioning" || list[0].RevokedAt != nil ||
		list[0].TokenID != id || list[0].CreatedBy != "admin@contoso.com" {
		t.Fatalf("List = %+v, %v", list, err)
	}
	raw, _ := json.Marshal(list)
	if strings.Contains(string(raw), token) {
		t.Fatalf("the listing carries the plaintext: %s", raw)
	}
	// Only the hash is stored.
	for _, e := range h.mem.tenants[tenantA].tokens {
		if e.Hash != HashToken(token) || strings.Contains(e.Hash, token) {
			t.Fatalf("stored token = %+v", e)
		}
	}
	h.mustDo(200, "GET", "/scim/v2/Users", token, nil)

	if err := h.tokens.Revoke(ctx, tenantA, id, "admin@contoso.com"); err != nil {
		t.Fatal(err)
	}
	if err := h.tokens.Revoke(ctx, tenantA, id, "admin@contoso.com"); err != nil {
		t.Fatalf("a second revoke = %v, want a no-op", err)
	}
	if err := h.tokens.Revoke(ctx, tenantA, "0f0f0f0f-0000-4000-8000-000000000000", "admin@contoso.com"); !IsNotFound(err) {
		t.Fatalf("revoking an unknown token = %v, want not found", err)
	}
	if err := h.tokens.Revoke(ctx, tenantA, "not-a-uuid", "admin@contoso.com"); !IsNotFound(err) {
		t.Fatalf("revoking a malformed id = %v, want not found", err)
	}
	// A token id from another tenant is unknown here, and stays live there.
	tokB, otherID := h.token(tenantB)
	if err := h.tokens.Revoke(ctx, tenantA, otherID, "admin@contoso.com"); !IsNotFound(err) {
		t.Fatalf("revoking another tenant's token = %v, want not found", err)
	}
	h.mustDo(200, "GET", "/scim/v2/Users", tokB, nil)
	h.mustDo(401, "GET", "/scim/v2/Users", token, nil)
	if list, _ := h.tokens.List(ctx, tenantA); list[0].RevokedAt == nil {
		t.Fatal("the listing does not show the revocation")
	}
	// The admin routes audit token writes (internal/deploy); this store does not, so there is one row.
	for _, e := range h.mem.AuditLog(tenantA) {
		if e.ObjectType == "scim_token" {
			t.Fatalf("the token store audited %s itself", e.Action)
		}
	}
}

func TestPersonLookupForTheIdentityService(t *testing.T) {
	h := newHarness(t, Config{})
	tok, _ := h.token(tenantA)
	id := str(h.mustDo(201, "POST", "/scim/v2/Users", tok, entraCreateUser)["id"])
	ctx := context.Background()

	p, found, err := h.svc.Person(ctx, tenantA, "ADA.LOVELACE@contoso.com", "")
	if err != nil || !found || p.UserRef != vectorUPN || !p.Active {
		t.Fatalf("by userName = %+v %v %v", p, found, err)
	}
	p, found, err = h.svc.Person(ctx, tenantA, "someone.else@contoso.com", strings.ToLower(vectorOIDID))
	if err != nil || !found || p.UserRef != vectorUPN {
		t.Fatalf("by object id = %+v %v %v", p, found, err)
	}
	h.mustDo(200, "PATCH", "/scim/v2/Users/"+id, tok, `{"Operations":[{"op":"Replace","path":"active","value":"False"}]}`)
	if p, _, _ = h.svc.Person(ctx, tenantA, "ada.lovelace@contoso.com", ""); p.Active {
		t.Fatal("a deactivated person reads as active; their sessions would outlive the deactivation")
	}
	if _, found, _ = h.svc.Person(ctx, tenantA, "nobody@contoso.com", ""); found {
		t.Fatal("an unprovisioned person was found")
	}
	if _, found, _ = h.svc.Person(ctx, tenantB, "ada.lovelace@contoso.com", ""); found {
		t.Fatal("tenant B found tenant A's person")
	}
}

// Many first writes for a tenant that has no user_ref key yet: the key is minted once, and every
// canonical ref is the one that key derives.
func TestConcurrentFirstWritesShareOneTenantKey(t *testing.T) {
	h := newHarness(t, Config{})
	tok, _ := h.token(tenantB)
	const n = 8
	var wg sync.WaitGroup
	ids := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, out := h.do("POST", "/scim/v2/Users", tok, map[string]any{"userName": fmt.Sprintf("c%d@fabrikam.com", i)})
			ids[i] = str(out["id"])
		}(i)
	}
	wg.Wait()
	for i, id := range ids {
		if id == "" {
			t.Fatalf("write %d failed", i)
		}
		want := h.ref(tenantB, protocol.UserRefUPN, fmt.Sprintf("c%d@fabrikam.com", i))
		if got := h.storedUser(tenantB, id).UserRef; got != want {
			t.Fatalf("write %d derived %s under a key that is not the tenant's (%s)", i, got, want)
		}
	}
}
