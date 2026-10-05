package scim

import (
	"strings"
	"testing"

	"github.com/shadow-ai-capture/device/protocol"

	"github.com/shadow-ai-capture/control-api/internal/directory"
)

// Okta's SCIM 2.0 client shapes (Okta's "SCIM 2.0 protocol reference" requests): a paged probe,
// a filter with startIndex/count, a create that carries a password and an empty groups list, a PUT
// with the whole profile, path-less PATCH for active and password, and Push Groups' rename with the id
// in the value and filter-path member removal.

const oktaCreateUser = `{
  "schemas": ["urn:ietf:params:scim:schemas:core:2.0:User"],
  "userName": "test.user@okta.local",
  "name": {"givenName": "Test", "familyName": "User"},
  "emails": [{"primary": true, "value": "test.user@okta.local", "type": "work"}],
  "displayName": "Test User",
  "locale": "en-US",
  "externalId": "00ujl29u0le5T6Aj10h7",
  "groups": [],
  "password": "1mz050nq",
  "active": true
}`

func TestOktaProvisioningLifecycle(t *testing.T) {
	h := newHarness(t, Config{})
	tok, _ := h.token(tenantA)

	if l := h.mustDo(200, "GET", "/scim/v2/Users?startIndex=1&count=1", tok, nil); total(l) != 0 {
		t.Fatalf("probe = %v", l)
	}
	if l := h.mustDo(200, "GET", filterPath("Users", `userName eq "test.user@okta.local"`, "startIndex", "1", "count", "100"), tok, nil); total(l) != 0 {
		t.Fatalf("pre-create lookup = %v", l)
	}

	created := h.mustDo(201, "POST", "/scim/v2/Users", tok, oktaCreateUser)
	id := str(created["id"])
	if _, has := created["password"]; has {
		t.Fatal("the password was echoed")
	}
	if _, has := created["groups"]; has {
		t.Fatal("the read-only groups attribute was stored")
	}
	row := h.storedUser(tenantA, id)
	opened, err := h.cipher.Open(tenantA, row.ResourceEnc)
	if err != nil || strings.Contains(opened, "1mz050nq") {
		t.Fatalf("the sealed resource holds the password (%v)", err)
	}
	// An Okta id is not a GUID: the person is keyed by userName alone, with no oid alias.
	upnRef := h.ref(tenantA, protocol.UserRefUPN, "test.user@okta.local")
	if row.UserRef != upnRef {
		t.Fatalf("canonical = %s, want %s", row.UserRef, upnRef)
	}
	if a := h.mem.Aliases(tenantA); len(a) != 1 || a[upnRef] != upnRef {
		t.Fatalf("aliases = %v, want only the upn-ref", a)
	}
	if got, _ := h.cipher.Open(tenantA, h.dim(tenantA, upnRef).DirectoryObjectIDEnc); got != "00ujl29u0le5T6Aj10h7" {
		t.Fatalf("directory id = %q, want the Okta externalId", got)
	}

	// Okta's profile update is a PUT of the whole user, id and password included; active is kept
	// when it is left out.
	h.mustDo(200, "PUT", "/scim/v2/Users/"+id, tok, `{
	  "schemas": ["urn:ietf:params:scim:schemas:core:2.0:User",
	              "urn:ietf:params:scim:schemas:extension:enterprise:2.0:User"],
	  "id": "`+id+`",
	  "userName": "test.user@okta.local",
	  "name": {"givenName": "Another", "familyName": "User"},
	  "emails": [{"primary": true, "value": "test.user@okta.local", "type": "work"}],
	  "displayName": "Another User",
	  "externalId": "00ujl29u0le5T6Aj10h7",
	  "password": "changed",
	  "urn:ietf:params:scim:schemas:extension:enterprise:2.0:User": {"department": "Sales"},
	  "meta": {"resourceType": "User"}}`)
	dim := h.dim(tenantA, upnRef)
	if deref(dim.Department) != "Sales" || deref(dim.DisplayName) != "Another User" || dim.Status != directory.StatusActive {
		t.Fatalf("user_dim after PUT = %s / %s / %s", deref(dim.Department), deref(dim.DisplayName), dim.Status)
	}

	// Deactivation and a password push, both path-less.
	h.mustDo(200, "PATCH", "/scim/v2/Users/"+id, tok, `{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],
	  "Operations":[{"op":"replace","value":{"active":false}}]}`)
	h.mustDo(200, "PATCH", "/scim/v2/Users/"+id, tok, `{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],
	  "Operations":[{"op":"replace","value":{"password":"another"}}]}`)
	got := h.mustDo(200, "GET", "/scim/v2/Users/"+id, tok, nil)
	if got["active"] != false || got["password"] != nil {
		t.Fatalf("after deactivate + password = %v", got)
	}
	if h.dim(tenantA, upnRef).Status != directory.StatusInactive {
		t.Fatal("deactivation did not reach user_dim")
	}

	// Push Groups.
	g := h.mustDo(201, "POST", "/scim/v2/Groups", tok, `{"schemas":["urn:ietf:params:scim:schemas:core:2.0:Group"],"displayName":"Test SCIMv2","members":[]}`)
	gid := str(g["id"])
	l := h.mustDo(200, "GET", filterPath("Groups", `displayName eq "Test SCIMv2"`, "startIndex", "1", "count", "100"), tok, nil)
	if total(l) != 1 || str(resources(t, l)[0]["id"]) != gid {
		t.Fatalf("group lookup = %v", l)
	}
	if code, out := h.do("PATCH", "/scim/v2/Groups/"+gid, tok, `{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],
	  "Operations":[{"op":"replace","value":{"id":"`+gid+`","displayName":"Renamed SCIMv2"}}]}`); code != 204 {
		t.Fatalf("rename = %d %v", code, out)
	}
	if code, out := h.do("PATCH", "/scim/v2/Groups/"+gid, tok, `{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],
	  "Operations":[{"op":"add","path":"members","value":[{"value":"`+id+`","display":"test.user@okta.local"}]}]}`); code != 204 {
		t.Fatalf("add member = %d %v", code, out)
	}
	g = h.mustDo(200, "GET", "/scim/v2/Groups/"+gid, tok, nil)
	if str(g["displayName"]) != "Renamed SCIMv2" || len(g["members"].([]any)) != 1 {
		t.Fatalf("group = %v", g)
	}
	if code, _ := h.do("PATCH", "/scim/v2/Groups/"+gid, tok, `{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],
	  "Operations":[{"op":"remove","path":"members[value eq \"`+id+`\"]"}]}`); code != 204 {
		t.Fatal("filter-path removal failed")
	}
	// Okta's group PUT replaces the name and the members.
	g = h.mustDo(200, "PUT", "/scim/v2/Groups/"+gid, tok, `{"schemas":["urn:ietf:params:scim:schemas:core:2.0:Group"],
	  "id":"`+gid+`","displayName":"Put SCIMv2","members":[{"value":"`+id+`"}]}`)
	if str(g["displayName"]) != "Put SCIMv2" || len(g["members"].([]any)) != 1 {
		t.Fatalf("group after PUT = %v", g)
	}
	if l := h.mustDo(200, "GET", "/scim/v2/Groups?startIndex=1&count=100", tok, nil); total(l) != 1 {
		t.Fatalf("group import = %v", l)
	}
	if code, _ := h.do("DELETE", "/scim/v2/Groups/"+gid, tok, nil); code != 204 {
		t.Fatal("group delete failed")
	}
}

func TestUserNameIsUniqueRegardlessOfCase(t *testing.T) {
	h := newHarness(t, Config{})
	tok, _ := h.token(tenantA)
	first := str(h.mustDo(201, "POST", "/scim/v2/Users", tok, map[string]any{"userName": "Dup@Contoso.com"})["id"])
	_, out := h.do("POST", "/scim/v2/Users", tok, map[string]any{"userName": "dup@contoso.com"})
	if str(out["status"]) != "409" || str(out["scimType"]) != "uniqueness" {
		t.Fatalf("duplicate create = %v, want a SCIM 409 uniqueness error", out)
	}
	schemas, _ := out["schemas"].([]any)
	if len(schemas) != 1 || schemas[0] != SchemaError {
		t.Fatalf("error schemas = %v", out["schemas"])
	}
	second := str(h.mustDo(201, "POST", "/scim/v2/Users", tok, map[string]any{"userName": "other@contoso.com"})["id"])
	h.mustDo(409, "PATCH", "/scim/v2/Users/"+second, tok, `{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],
	  "Operations":[{"op":"replace","path":"userName","value":"DUP@contoso.com"}]}`)
	// The refused rename left the user and the audit trail as they were.
	if got := h.mustDo(200, "GET", "/scim/v2/Users/"+second, tok, nil); str(got["userName"]) != "other@contoso.com" {
		t.Fatalf("a refused rename changed the user: %v", got)
	}
	if len(h.mem.StoredUsers(tenantA)) != 2 || first == second {
		t.Fatal("unexpected users")
	}
	for _, e := range h.mem.AuditLog(tenantA) {
		if e.Action == "scim.user.update" {
			t.Fatal("a refused write was audited as done")
		}
	}
}

func TestPaging(t *testing.T) {
	h := newHarness(t, Config{})
	tok, _ := h.token(tenantA)
	var ids []string
	for _, name := range []string{"p1", "p2", "p3", "p4", "p5"} {
		ids = append(ids, str(h.mustDo(201, "POST", "/scim/v2/Users", tok, map[string]any{"userName": name + "@contoso.com"})["id"]))
	}
	l := h.mustDo(200, "GET", "/scim/v2/Users?startIndex=2&count=2", tok, nil)
	page := resources(t, l)
	if total(l) != 5 || len(page) != 2 || str(page[0]["id"]) != ids[1] || str(page[1]["id"]) != ids[2] {
		t.Fatalf("page 2 = %v", l)
	}
	if n, _ := l["startIndex"].(interface{ Int64() (int64, error) }).Int64(); n != 2 {
		t.Fatalf("startIndex = %v", l["startIndex"])
	}
	if n, _ := l["itemsPerPage"].(interface{ Int64() (int64, error) }).Int64(); n != 2 {
		t.Fatalf("itemsPerPage = %v", l["itemsPerPage"])
	}
	if l := h.mustDo(200, "GET", "/scim/v2/Users?count=0", tok, nil); total(l) != 5 || len(resources(t, l)) != 0 {
		t.Fatalf("count=0 = %v", l)
	}
	if l := h.mustDo(200, "GET", "/scim/v2/Users?startIndex=9&count=5", tok, nil); total(l) != 5 || len(resources(t, l)) != 0 {
		t.Fatalf("past the end = %v", l)
	}
	if l := h.mustDo(200, "GET", "/scim/v2/Users?startIndex=0&count=100000", tok, nil); len(resources(t, l)) != 5 {
		t.Fatalf("clamped page = %v", l)
	}
	// A filtered result pages too.
	l = h.mustDo(200, "GET", filterPath("Users", `userName eq "p3@contoso.com"`, "startIndex", "2"), tok, nil)
	if total(l) != 1 || len(resources(t, l)) != 0 {
		t.Fatalf("filtered page 2 = %v", l)
	}
}

func TestFilters(t *testing.T) {
	h := newHarness(t, Config{})
	tok, _ := h.token(tenantA)
	id := str(h.mustDo(201, "POST", "/scim/v2/Users", tok, entraCreateUser)["id"])
	for f, want := range map[string]int{
		`userName eq "ada.lovelace@contoso.com"`:                                            1,
		`UserName EQ "ADA.LOVELACE@CONTOSO.COM"`:                                            1,
		`urn:ietf:params:scim:schemas:core:2.0:User:userName eq "ada.lovelace@contoso.com"`: 1,
		`userName eq "ada.lovelace@contoso.com" and active eq true`:                         1,
		`userName eq "ada.lovelace@contoso.com" and active eq false`:                        0,
		`externalId eq "{6F9619FF-8B86-D011-B42D-00C04FC964FF}"`:                            0,
		`id eq "` + id + `"`: 1,
		`id eq "not-a-uuid"`: 0,
		`(userName eq "ada.lovelace@contoso.com") and emails[type eq "work"].value co "@contoso"`: 1,
	} {
		code, l := h.do("GET", filterPath("Users", f), tok, nil)
		if code != 200 || total(l) != want {
			t.Errorf("filter %s = %d %v, want %d results", f, code, l, want)
		}
	}
	for _, bad := range []string{`displayName eq "Ada Lovelace"`, `userName eq`, `userName xx "a"`, `userName eq "a" or externalId eq "b"`} {
		code, out := h.do("GET", filterPath("Users", bad), tok, nil)
		if code != 400 || str(out["scimType"]) != "invalidFilter" {
			t.Errorf("filter %s = %d %v, want 400 invalidFilter", bad, code, out)
		}
	}
}

func TestTokenAuthentication(t *testing.T) {
	h := newHarness(t, Config{})
	tok, tokenID := h.token(tenantA)
	tokB, _ := h.token(tenantB)
	h.mustDo(200, "GET", "/scim/v2/Users", tok, nil)

	_, secretA, _ := strings.Cut(strings.TrimPrefix(tok, TokenPrefix), ".")
	for name, header := range map[string]string{
		"missing":          "",
		"basic scheme":     "Basic " + tok,
		"not a SCIM token": "Bearer sac1." + tenantA + ".abc",
		"wrong secret":     "Bearer " + TokenPrefix + tenantA + ".AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		// Tenant A's secret presented with tenant B's id in the clear: the database's answer
		// (tenant A) does not match the claim, so the session is never opened for B.
		"forged tenant": "Bearer " + TokenPrefix + tenantB + "." + secretA,
	} {
		req := "/scim/v2/Users"
		code, out := h.doHeader("GET", req, header)
		if code != 401 || str(out["status"]) != "401" {
			t.Errorf("%s: %d %v, want 401", name, code, out)
		}
	}
	// Discovery is behind the token too.
	if code, _ := h.doHeader("GET", "/scim/v2/ServiceProviderConfig", ""); code != 401 {
		t.Errorf("unauthenticated discovery = %d", code)
	}

	if err := h.tokens.Revoke(t.Context(), tenantA, tokenID, "admin@example.test"); err != nil {
		t.Fatal(err)
	}
	h.mustDo(401, "GET", "/scim/v2/Users", tok, nil)
	// The other tenant's token still works, for its own tenant.
	h.mustDo(200, "GET", "/scim/v2/Users", tokB, nil)
}

func TestTenantIsolation(t *testing.T) {
	h := newHarness(t, Config{})
	tokA, _ := h.token(tenantA)
	tokB, _ := h.token(tenantB)
	idA := str(h.mustDo(201, "POST", "/scim/v2/Users", tokA, entraCreateUser)["id"])
	gA := str(h.mustDo(201, "POST", "/scim/v2/Groups", tokA, map[string]any{"displayName": "Team"})["id"])

	// B sees none of A's people or groups, by id or by filter, and cannot change them.
	h.mustDo(404, "GET", "/scim/v2/Users/"+idA, tokB, nil)
	h.mustDo(404, "PATCH", "/scim/v2/Users/"+idA, tokB, `{"Operations":[{"op":"replace","path":"active","value":false}]}`)
	if code, _ := h.do("DELETE", "/scim/v2/Users/"+idA, tokB, nil); code != 404 {
		t.Fatalf("B deleting A's user = %d", code)
	}
	h.mustDo(404, "GET", "/scim/v2/Groups/"+gA, tokB, nil)
	if l := h.mustDo(200, "GET", filterPath("Users", `userName eq "Ada.Lovelace@Contoso.com"`), tokB, nil); total(l) != 0 {
		t.Fatal("B's filter found A's user")
	}
	if l := h.mustDo(200, "GET", "/scim/v2/Users", tokB, nil); total(l) != 0 {
		t.Fatal("B's list holds A's user")
	}
	if !h.storedUser(tenantA, idA).Active {
		t.Fatal("B's refused write changed A's user")
	}
	// B may provision the same person independently; the tenants' keys differ, so do the refs.
	idB := str(h.mustDo(201, "POST", "/scim/v2/Users", tokB, entraCreateUser)["id"])
	refA, refB := h.storedUser(tenantA, idA).UserRef, h.storedUser(tenantB, idB).UserRef
	if refA == refB {
		t.Fatal("two tenants derived the same user_ref for one UPN")
	}
	// Tenant B is 'hashed': no clear directory name is stored there.
	if d := h.dim(tenantB, refB); d.DisplayName != nil {
		t.Fatalf("a hashed tenant stored display name %q", *d.DisplayName)
	}
	if len(h.mem.Aliases(tenantB)) != 2 || len(h.mem.AuditLog(tenantB)) != 1 {
		t.Fatalf("tenant B holds %d aliases, %d audit rows", len(h.mem.Aliases(tenantB)), len(h.mem.AuditLog(tenantB)))
	}
}

// A userName reused after its first holder was renamed: the newcomer must not share the first
// holder's canonical ref (their user_dim rows would merge), and the upn-ref moves to whoever holds
// the userName now.
func TestReusedUserNameMovesItsAliasAndKeepsPeopleApart(t *testing.T) {
	h := newHarness(t, Config{})
	tok, _ := h.token(tenantA)
	first := str(h.mustDo(201, "POST", "/scim/v2/Users", tok, map[string]any{"userName": "x@contoso.com",
		SchemaEnterpriseUser: map[string]any{"department": "Engineering"}})["id"])
	h.mustDo(200, "PATCH", "/scim/v2/Users/"+first, tok, `{"Operations":[{"op":"replace","path":"userName","value":"y@contoso.com"}]}`)
	second := str(h.mustDo(201, "POST", "/scim/v2/Users", tok, map[string]any{"userName": "x@contoso.com",
		SchemaEnterpriseUser: map[string]any{"department": "Legal"}})["id"])

	refX := h.ref(tenantA, protocol.UserRefUPN, "x@contoso.com")
	refY := h.ref(tenantA, protocol.UserRefUPN, "y@contoso.com")
	a, b := h.storedUser(tenantA, first).UserRef, h.storedUser(tenantA, second).UserRef
	if a != refX || b == refX || b == a {
		t.Fatalf("canonical refs: first %s, second %s (x-ref %s)", a, b, refX)
	}
	aliases := h.mem.Aliases(tenantA)
	if aliases[refX] != b || aliases[refY] != a {
		t.Fatalf("aliases = %v; x-ref should name the second person, y-ref the first", aliases)
	}
	if d := h.dim(tenantA, a); deref(d.Department) != "Engineering" {
		t.Fatalf("the first person's row was overwritten: %s", deref(d.Department))
	}
	if d := h.dim(tenantA, b); deref(d.Department) != "Legal" {
		t.Fatalf("the second person's row = %s", deref(d.Department))
	}
}

func TestPopulationAttributeAndBadRequests(t *testing.T) {
	h := newHarness(t, Config{PopulationAttribute: "urn:ietf:params:scim:schemas:extension:enterprise:2.0:User:division"})
	tok, _ := h.token(tenantA)
	id := str(h.mustDo(201, "POST", "/scim/v2/Users", tok, map[string]any{"userName": "pop@contoso.com",
		SchemaEnterpriseUser: map[string]any{"division": "Contractors"}})["id"])
	ref := h.storedUser(tenantA, id).UserRef
	if d := h.dim(tenantA, ref); deref(d.Population) != "Contractors" || d.Department != nil {
		t.Fatalf("population = %s, department = %s", deref(d.Population), deref(d.Department))
	}

	for name, body := range map[string]string{
		"no userName":      `{"schemas":["urn:ietf:params:scim:schemas:core:2.0:User"],"displayName":"x"}`,
		"not JSON":         `{"userName":`,
		"array body":       `[1,2]`,
		"bad active":       `{"userName":"a@b.c","active":"maybe"}`,
		"numeric userName": `{"userName":42}`,
	} {
		if code, out := h.do("POST", "/scim/v2/Users", tok, body); code != 400 {
			t.Errorf("%s: %d %v, want 400", name, code, out)
		}
	}
	for name, body := range map[string]string{
		"no Operations":          `{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"]}`,
		"unknown op":             `{"Operations":[{"op":"move","path":"userName","value":"x"}]}`,
		"remove without path":    `{"Operations":[{"op":"remove"}]}`,
		"removes userName":       `{"Operations":[{"op":"remove","path":"userName"}]}`,
		"bad path":               `{"Operations":[{"op":"replace","path":"emails[type eq","value":"x"}]}`,
		"no target for a filter": `{"Operations":[{"op":"replace","path":"emails[type co \"w\"].value","value":"x"}]}`,
	} {
		if code, out := h.do("PATCH", "/scim/v2/Users/"+id, tok, body); code != 400 {
			t.Errorf("%s: %d %v, want 400", name, code, out)
		}
	}
	h.mustDo(404, "GET", "/scim/v2/Users/not-a-uuid", tok, nil)
	h.mustDo(404, "GET", "/scim/v2/Users/0f0f0f0f-0000-4000-8000-000000000000", tok, nil)
	h.mustDo(404, "GET", "/scim/v2/Nope", tok, nil)
	h.mustDo(405, "POST", "/scim/v2/ServiceProviderConfig", tok, `{}`)
	h.mustDo(501, "POST", "/scim/v2/Bulk", tok, `{}`)
	// None of that changed the user.
	if got := h.mustDo(200, "GET", "/scim/v2/Users/"+id, tok, nil); str(got["userName"]) != "pop@contoso.com" {
		t.Fatalf("a refused request changed the user: %v", got)
	}
}

func TestDiscoveryDocuments(t *testing.T) {
	h := newHarness(t, Config{})
	tok, _ := h.token(tenantA)
	spc := h.mustDo(200, "GET", "/scim/v2/ServiceProviderConfig", tok, nil)
	if spc["patch"].(map[string]any)["supported"] != true || spc["bulk"].(map[string]any)["supported"] != false {
		t.Fatalf("ServiceProviderConfig = %v", spc)
	}
	if l := h.mustDo(200, "GET", "/scim/v2/Schemas", tok, nil); total(l) != 3 {
		t.Fatalf("Schemas = %v", l)
	}
	ent := h.mustDo(200, "GET", "/scim/v2/Schemas/"+SchemaEnterpriseUser, tok, nil)
	if str(ent["id"]) != SchemaEnterpriseUser {
		t.Fatalf("enterprise schema = %v", ent)
	}
	if l := h.mustDo(200, "GET", "/scim/v2/ResourceTypes", tok, nil); total(l) != 2 {
		t.Fatalf("ResourceTypes = %v", l)
	}
	if u := h.mustDo(200, "GET", "/scim/v2/ResourceTypes/User", tok, nil); str(u["endpoint"]) != "/Users" {
		t.Fatalf("User resource type = %v", u)
	}
	h.mustDo(404, "GET", "/scim/v2/Schemas/urn:nope", tok, nil)
}
