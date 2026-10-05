package scim

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/shadow-ai-capture/device/protocol"

	"github.com/shadow-ai-capture/control-api/internal/directory"
)

// The request bodies below are the shapes Microsoft Entra ID's provisioning service sends (Microsoft's
// "Develop and plan provisioning for a SCIM endpoint" reference requests, and its documented
// non-compliant defaults: capitalised ops, string booleans, path-less replace with path keys,
// members removal with a value), with the identifiers swapped for the protocol test vectors.

const entraCreateUser = `{
  "schemas": [
    "urn:ietf:params:scim:schemas:core:2.0:User",
    "urn:ietf:params:scim:schemas:extension:enterprise:2.0:User"],
  "externalId": "6F9619FF-8B86-D011-B42D-00C04FC964FF",
  "userName": "Ada.Lovelace@Contoso.com",
  "active": true,
  "emails": [{"primary": true, "type": "work", "value": "ada.lovelace@contoso.com"}],
  "meta": {"resourceType": "User"},
  "name": {"formatted": "Ada Lovelace", "familyName": "Lovelace", "givenName": "Ada"},
  "roles": [],
  "urn:ietf:params:scim:schemas:extension:enterprise:2.0:User": {
    "department": "Engineering",
    "employeeNumber": "1001"
  }
}`

func TestEntraProvisioningLifecycle(t *testing.T) {
	h := newHarness(t, Config{})
	tok, tokenID := h.token(tenantA)

	// "Test Connection": a userName no one has. 200 and an empty list, never 404.
	list := h.mustDo(200, "GET", filterPath("Users", `userName eq "4a1c6b9e-test-connection"`), tok, nil)
	if total(list) != 0 || len(resources(t, list)) != 0 {
		t.Fatalf("test connection list = %v", list)
	}

	created := h.mustDo(201, "POST", "/scim/v2/Users", tok, entraCreateUser)
	id := str(created["id"])
	if !isUUID(id) {
		t.Fatalf("created id = %q", id)
	}
	meta, _ := created["meta"].(map[string]any)
	if str(meta["resourceType"]) != "User" || str(meta["location"]) != "https://app.example.test/scim/v2/Users/"+id {
		t.Fatalf("meta = %v", meta)
	}

	// The canonical ref is the userName's upn-ref: the protocol vector, through the mapping.
	row := h.storedUser(tenantA, id)
	if row.UserRef != vectorUPN {
		t.Fatalf("canonical user_ref = %s, want the upn vector %s", row.UserRef, vectorUPN)
	}
	aliases := h.mem.Aliases(tenantA)
	if aliases[vectorUPN] != vectorUPN || aliases[vectorOID] != vectorUPN {
		t.Fatalf("aliases = %v; want the upn and oid vectors -> the canonical ref", aliases)
	}
	if _, ok := aliases[vectorAcct]; ok {
		t.Fatal("an account-name ref was aliased; it must stay the unmapped series")
	}
	// The stored row holds no clear identifier: the lookups are HMACs under the tenant key.
	if string(row.UserNameHash) == "Ada.Lovelace@Contoso.com" || len(row.UserNameHash) != 32 || len(row.ExternalIDHash) != 32 {
		t.Fatalf("lookup hashes = %x / %x", row.UserNameHash, row.ExternalIDHash)
	}

	dim := h.dim(tenantA, vectorUPN)
	if deref(dim.Department) != "Engineering" || deref(dim.DisplayName) != "Ada Lovelace" || dim.Status != directory.StatusActive {
		t.Fatalf("user_dim = dept %s, name %s, status %s", deref(dim.Department), deref(dim.DisplayName), dim.Status)
	}
	if got, err := h.cipher.Open(tenantA, dim.DirectoryObjectIDEnc); err != nil || got != vectorOIDID {
		t.Fatalf("directory_object_id_enc opens to %q, %v; want the externalId", got, err)
	}

	// GET returns what Entra sent.
	got := h.mustDo(200, "GET", "/scim/v2/Users/"+id, tok, nil)
	if str(got["userName"]) != "Ada.Lovelace@Contoso.com" || str(got["externalId"]) != vectorOIDID {
		t.Fatalf("GET = %v", got)
	}
	ext, _ := got[SchemaEnterpriseUser].(map[string]any)
	if str(ext["employeeNumber"]) != "1001" {
		t.Fatalf("the enterprise extension did not round-trip: %v", got)
	}

	// Lookups by either matching attribute, in any case.
	for _, f := range []string{`userName eq "ada.lovelace@contoso.com"`, `externalId eq "6f9619ff-8b86-d011-b42d-00c04fc964ff"`} {
		list := h.mustDo(200, "GET", filterPath("Users", f), tok, nil)
		if total(list) != 1 || str(resources(t, list)[0]["id"]) != id {
			t.Fatalf("filter %s = %v", f, list)
		}
	}

	// PATCH with Entra's capitalised op and a filtered multi-valued path.
	h.mustDo(200, "PATCH", "/scim/v2/Users/"+id, tok, `{
	  "schemas": ["urn:ietf:params:scim:api:messages:2.0:PatchOp"],
	  "Operations": [
	    {"op": "Replace", "path": "emails[type eq \"work\"].value", "value": "ada@contoso.com"},
	    {"op": "Replace", "path": "name.familyName", "value": "King"}
	  ]}`)
	got = h.mustDo(200, "GET", "/scim/v2/Users/"+id, tok, nil)
	emails, _ := got["emails"].([]any)
	if len(emails) != 1 || str(emails[0].(map[string]any)["value"]) != "ada@contoso.com" {
		t.Fatalf("emails after replace = %v", got["emails"])
	}
	if str(got["name"].(map[string]any)["familyName"]) != "King" {
		t.Fatalf("name after replace = %v", got["name"])
	}

	// The compliant path-less form: keys that are themselves paths, including the extension.
	h.mustDo(200, "PATCH", "/scim/v2/Users/"+id, tok, `{
	  "schemas": ["urn:ietf:params:scim:api:messages:2.0:PatchOp"],
	  "Operations": [{"op": "replace", "value": {
	    "displayName": "Ada King",
	    "name.givenName": "Augusta",
	    "emails[type eq \"other\"].value": "aking@example.test",
	    "urn:ietf:params:scim:schemas:extension:enterprise:2.0:User:department": "Research"
	  }}]}`)
	dim = h.dim(tenantA, vectorUPN)
	if deref(dim.Department) != "Research" || deref(dim.DisplayName) != "Ada King" {
		t.Fatalf("user_dim after path-less replace = dept %s, name %s", deref(dim.Department), deref(dim.DisplayName))
	}
	got = h.mustDo(200, "GET", "/scim/v2/Users/"+id, tok, nil)
	if emails, _ := got["emails"].([]any); len(emails) != 2 {
		t.Fatalf("a filtered path that matched nothing did not create the value: %v", got["emails"])
	}

	// An extension attribute by its own path, in both of Entra's spellings.
	h.mustDo(200, "PATCH", "/scim/v2/Users/"+id, tok, `{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],
	  "Operations":[{"op":"Replace","path":"urn:ietf:params:scim:schemas:extension:enterprise:2.0:User:department","value":"Finance"}]}`)
	if d := h.dim(tenantA, vectorUPN); deref(d.Department) != "Finance" {
		t.Fatalf("department after the extension path = %s", deref(d.Department))
	}
	h.mustDo(200, "PATCH", "/scim/v2/Users/"+id, tok, `{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],
	  "Operations":[{"op":"Add","path":"urn:ietf:params:scim:schemas:extension:enterprise:2.0:User.department","value":"Legal"}]}`)
	if d := h.dim(tenantA, vectorUPN); deref(d.Department) != "Legal" {
		t.Fatalf("department after the dotted extension path = %s", deref(d.Department))
	}

	// A manager set, then removed with a value-less path; a department cleared the same way is the
	// explicit unmapped series.
	h.mustDo(200, "PATCH", "/scim/v2/Users/"+id, tok, `{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],
	  "Operations":[{"op":"Add","path":"urn:ietf:params:scim:schemas:extension:enterprise:2.0:User:manager","value":"26118915-6090-4610-87a4-49d0767e9fde"}]}`)
	h.mustDo(200, "PATCH", "/scim/v2/Users/"+id, tok, `{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],
	  "Operations":[
	    {"op":"Remove","path":"urn:ietf:params:scim:schemas:extension:enterprise:2.0:User:manager"},
	    {"op":"Replace","path":"urn:ietf:params:scim:schemas:extension:enterprise:2.0:User:department"}]}`)
	got = h.mustDo(200, "GET", "/scim/v2/Users/"+id, tok, nil)
	if ext, _ := got[SchemaEnterpriseUser].(map[string]any); ext["manager"] != nil || ext["department"] != nil {
		t.Fatalf("value-less removes left %v", ext)
	}
	if d := h.dim(tenantA, vectorUPN); d.Department != nil || d.ManagerRef != nil {
		t.Fatalf("user_dim after clearing = dept %s, manager %s", deref(d.Department), deref(d.ManagerRef))
	}

	// Entra's soft delete: "Replace" active with the string "False".
	h.mustDo(200, "PATCH", "/scim/v2/Users/"+id, tok, `{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],
	  "Operations":[{"op":"Replace","path":"active","value":"False"}]}`)
	got = h.mustDo(200, "GET", "/scim/v2/Users/"+id, tok, nil)
	if got["active"] != false {
		t.Fatalf("active after \"False\" = %#v, want the boolean false", got["active"])
	}
	if h.storedUser(tenantA, id).Active || h.dim(tenantA, vectorUPN).Status != directory.StatusInactive {
		t.Fatal("a deactivated user is still active in scim_user or user_dim")
	}
	// ...and back, the compliant way.
	h.mustDo(200, "PATCH", "/scim/v2/Users/"+id, tok, `{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],
	  "Operations":[{"op":"replace","value":{"active":true}}]}`)
	if !h.storedUser(tenantA, id).Active {
		t.Fatal("reactivation did not take")
	}

	// A userName change: the canonical ref is fixed; the new UPN's ref becomes an alias of it and the
	// old one stays, so both the device's old and new derivations land on the same person.
	h.mustDo(200, "PATCH", "/scim/v2/Users/"+id, tok, `{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],
	  "Operations":[{"op":"Replace","path":"userName","value":"Ada.King@Contoso.com"}]}`)
	if r := h.storedUser(tenantA, id); r.UserRef != vectorUPN {
		t.Fatalf("a rename changed the canonical ref to %s", r.UserRef)
	}
	newRef := h.ref(tenantA, protocol.UserRefUPN, "ada.king@contoso.com")
	aliases = h.mem.Aliases(tenantA)
	if aliases[newRef] != vectorUPN || aliases[vectorUPN] != vectorUPN || aliases[vectorOID] != vectorUPN {
		t.Fatalf("aliases after rename = %v", aliases)
	}
	if l := h.mustDo(200, "GET", filterPath("Users", `userName eq "Ada.Lovelace@Contoso.com"`), tok, nil); total(l) != 0 {
		t.Fatal("the old userName still finds the user")
	}
	if l := h.mustDo(200, "GET", filterPath("Users", `userName eq "ada.king@contoso.com"`), tok, nil); total(l) != 1 {
		t.Fatal("the new userName does not find the user")
	}

	// DELETE retires: 204, the row stays, inactive everywhere, still readable.
	if code, _ := h.do("DELETE", "/scim/v2/Users/"+id, tok, nil); code != 204 {
		t.Fatalf("DELETE = %d, want 204", code)
	}
	if h.storedUser(tenantA, id).Active || h.dim(tenantA, vectorUPN).Status != directory.StatusInactive {
		t.Fatal("DELETE left the person active")
	}
	if d := h.dim(tenantA, vectorUPN); deref(d.DisplayName) != "Ada King" {
		t.Fatal("retirement rewrote the person's attributes; history must stay attributable")
	}
	got = h.mustDo(200, "GET", "/scim/v2/Users/"+id, tok, nil)
	if got["active"] != false {
		t.Fatalf("a retired user reads as %v", got["active"])
	}

	// Every provisioning write is audited as the token, and no audit detail carries a person's data.
	log := h.mem.AuditLog(tenantA)
	actions := map[string]int{}
	for _, e := range log {
		actions[e.Action]++
		if e.ActorType != "service" || e.ActorID != "scim:"+tokenID {
			t.Fatalf("audit actor = %s %s", e.ActorType, e.ActorID)
		}
		raw, _ := json.Marshal(e.Detail)
		for _, personal := range []string{"ada", "lovelace", "king", "augusta", "contoso", "6f9619ff", "research", "finance"} {
			if strings.Contains(strings.ToLower(string(raw)), personal) {
				t.Fatalf("audit detail %s carries %q", raw, personal)
			}
		}
		if e.ObjectID != id || e.SubjectRef != vectorUPN {
			t.Fatalf("audit row names %s / %s", e.ObjectID, e.SubjectRef)
		}
	}
	for _, want := range []string{"scim.user.create", "scim.user.update", "scim.user.deactivate", "scim.user.reactivate", "scim.user.retire"} {
		if actions[want] == 0 {
			t.Errorf("no %s audit row; got %v", want, actions)
		}
	}
}

func TestEntraGroupMembership(t *testing.T) {
	h := newHarness(t, Config{})
	tok, _ := h.token(tenantA)
	u1 := str(h.mustDo(201, "POST", "/scim/v2/Users", tok, map[string]any{"userName": "one@contoso.com"})["id"])
	u2 := str(h.mustDo(201, "POST", "/scim/v2/Users", tok, map[string]any{"userName": "two@contoso.com"})["id"])

	created := h.mustDo(201, "POST", "/scim/v2/Groups", tok, `{
	  "schemas": ["urn:ietf:params:scim:schemas:core:2.0:Group",
	              "http://schemas.microsoft.com/2006/11/ResourceManagement/ADSCIM/Group"],
	  "externalId": "8aa1a0c0-c4c3-4bc0-b4a5-2ef676900159",
	  "displayName": "Security Analysts",
	  "meta": {"resourceType": "Group"}}`)
	gid := str(created["id"])
	if members, ok := created["members"].([]any); !ok || len(members) != 0 {
		t.Fatalf("a new group's members = %v, want []", created["members"])
	}

	// Entra's existence check: by displayName, without members.
	list := h.mustDo(200, "GET", filterPath("Groups", `displayName eq "security analysts"`, "excludedAttributes", "members"), tok, nil)
	if total(list) != 1 {
		t.Fatalf("group by displayName = %v", list)
	}
	if _, has := resources(t, list)[0]["members"]; has {
		t.Fatal("excludedAttributes=members still returned members")
	}

	patch := func(body string) {
		t.Helper()
		if code, out := h.do("PATCH", "/scim/v2/Groups/"+gid, tok, body); code != 204 {
			t.Fatalf("group PATCH = %d: %v", code, out)
		}
	}
	members := func() []string {
		t.Helper()
		g := h.mustDo(200, "GET", "/scim/v2/Groups/"+gid, tok, nil)
		var out []string
		for _, m := range g["members"].([]any) {
			out = append(out, str(m.(map[string]any)["value"]))
		}
		return out
	}

	patch(`{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],"Operations":[
	  {"op":"Add","path":"members","value":[{"$ref":null,"value":"` + u1 + `"},{"$ref":null,"value":"` + u2 + `"}]}]}`)
	if got := members(); len(got) != 2 {
		t.Fatalf("members after add = %v", got)
	}
	// Adding again is not a duplicate.
	patch(`{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],"Operations":[
	  {"op":"Add","path":"members","value":[{"$ref":null,"value":"` + u1 + `"}]}]}`)
	if got := members(); len(got) != 2 {
		t.Fatalf("a repeated add duplicated a member: %v", got)
	}
	// Entra's default removal: path members with a value.
	patch(`{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],"Operations":[
	  {"op":"Remove","path":"members","value":[{"$ref":null,"value":"` + u1 + `"}]}]}`)
	if got := members(); len(got) != 1 || got[0] != u2 {
		t.Fatalf("members after the value removal = %v", got)
	}
	// The compliant removal: a filter path and no value.
	patch(`{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],"Operations":[
	  {"op":"remove","path":"members[value eq \"` + u2 + `\"]"}]}`)
	if got := members(); len(got) != 0 {
		t.Fatalf("members after the filtered removal = %v", got)
	}
	// A member this tenant does not have is dropped, not an error.
	patch(`{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],"Operations":[
	  {"op":"Add","path":"members","value":[{"value":"0f0f0f0f-0000-4000-8000-000000000000"},{"value":"not-an-id"}]}]}`)
	if got := members(); len(got) != 0 {
		t.Fatalf("an unknown member was added: %v", got)
	}
	patch(`{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],"Operations":[
	  {"op":"Replace","path":"displayName","value":"SOC"}]}`)
	g := h.mustDo(200, "GET", "/scim/v2/Groups/"+gid+"?excludedAttributes=members", tok, nil)
	if str(g["displayName"]) != "SOC" || g["members"] != nil || str(g["externalId"]) != "8aa1a0c0-c4c3-4bc0-b4a5-2ef676900159" {
		t.Fatalf("group after rename = %v", g)
	}

	if code, _ := h.do("DELETE", "/scim/v2/Groups/"+gid, tok, nil); code != 204 {
		t.Fatalf("group DELETE = %d", code)
	}
	h.mustDo(404, "GET", "/scim/v2/Groups/"+gid, tok, nil)

	var actions []string
	for _, e := range h.mem.AuditLog(tenantA) {
		if e.ObjectType == "scim_group" {
			actions = append(actions, e.Action)
			raw, _ := json.Marshal(e.Detail)
			if strings.Contains(string(raw), "Security") || strings.Contains(string(raw), "SOC") {
				t.Fatalf("group audit detail carries the name: %s", raw)
			}
		}
	}
	if len(actions) == 0 || actions[0] != "scim.group.create" || actions[len(actions)-1] != "scim.group.delete" {
		t.Fatalf("group audit actions = %v", actions)
	}
}
