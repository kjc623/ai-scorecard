package scim

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestParsePath(t *testing.T) {
	for raw, want := range map[string]attrPath{
		"userName":       {attr: "userName"},
		"name.givenName": {attr: "name", sub: "givenName"},
		"urn:ietf:params:scim:schemas:core:2.0:User:userName":                      {attr: "userName"},
		"urn:ietf:params:scim:schemas:extension:enterprise:2.0:User:department":    {schema: SchemaEnterpriseUser, attr: "department"},
		"URN:IETF:PARAMS:SCIM:SCHEMAS:EXTENSION:ENTERPRISE:2.0:USER.department":    {schema: SchemaEnterpriseUser, attr: "department"},
		"urn:ietf:params:scim:schemas:extension:enterprise:2.0:User:manager.value": {schema: SchemaEnterpriseUser, attr: "manager", sub: "value"},
		"urn:ietf:params:scim:schemas:extension:enterprise:2.0:User":               {schema: SchemaEnterpriseUser},
		"urn:ietf:params:scim:schemas:extension:CustomExt:2.0:User:costCode":       {schema: "urn:ietf:params:scim:schemas:extension:CustomExt:2.0:User", attr: "costCode"},
	} {
		got, err := parsePath(raw)
		if err != nil {
			t.Errorf("parsePath(%q): %v", raw, err)
			continue
		}
		if got.schema != want.schema || got.attr != want.attr || got.sub != want.sub || got.filter != nil {
			t.Errorf("parsePath(%q) = %+v, want %+v", raw, got, want)
		}
	}
	p, err := parsePath(`emails[type eq "work"].value`)
	if err != nil || p.attr != "emails" || p.sub != "value" || p.filter == nil {
		t.Fatalf("filtered path = %+v, %v", p, err)
	}
	if !p.filter.match(map[string]any{"type": "Work"}) || p.filter.match(map[string]any{"type": "home"}) {
		t.Fatal("the value filter does not select by type")
	}
	for _, bad := range []string{"", "emails[type eq \"work\"", "name.", "a b", "urn:ietf:params:scim:schemas:core:2.0:User", `emails[type eq "work"]x`} {
		if _, err := parsePath(bad); err == nil {
			t.Errorf("parsePath(%q) accepted", bad)
		}
	}
}

func TestFilterLanguage(t *testing.T) {
	res := map[string]any{
		"userName": "Ada@Contoso.com", "active": true, "emails": []any{
			map[string]any{"type": "work", "value": "ada@contoso.com"},
			map[string]any{"type": "home", "value": "ada@example.test"},
		},
		"meta": map[string]any{"lastModified": "2026-10-05T09:00:00Z"},
	}
	for f, want := range map[string]bool{
		`userName eq "ada@contoso.com"`:                         true,
		`userName ne "ada@contoso.com"`:                         false,
		`userName sw "ADA"`:                                     true,
		`userName ew ".com"`:                                    true,
		`emails.value co "example"`:                             true,
		`emails[type eq "work" and value co "contoso"]`:         true,
		`emails[type eq "home"].value eq "ada@contoso.com"`:     false,
		`active eq true and not (userName eq "x")`:              true,
		`active eq false or userName pr`:                        true,
		`title pr`:                                              false,
		`title eq null`:                                         true,
		`meta.lastModified gt "2026-10-01T00:00:00Z"`:           true,
		`userName eq "a" or (active eq true and title eq null)`: true,
	} {
		e, err := parseFilter(f)
		if err != nil {
			t.Errorf("parseFilter(%q): %v", f, err)
			continue
		}
		if got := e.match(res); got != want {
			t.Errorf("%s = %v, want %v", f, got, want)
		}
	}
	e, _ := parseFilter(`type eq "work" and primary eq true`)
	if got := seed(e); !reflect.DeepEqual(got, map[string]any{"type": "work", "primary": true}) {
		t.Fatalf("seed = %v", got)
	}
	e, _ = parseFilter(`type co "w"`)
	if seed(e) != nil {
		t.Fatal("a non-equality filter seeded a value")
	}
}

func TestPatchEdgeCases(t *testing.T) {
	start := func() map[string]any {
		var m map[string]any
		_ = json.Unmarshal([]byte(`{
		  "schemas": ["urn:ietf:params:scim:schemas:core:2.0:User","urn:ietf:params:scim:schemas:extension:enterprise:2.0:User"],
		  "userName": "a@b.c",
		  "name": {"givenName": "A", "familyName": "B"},
		  "emails": [{"type": "work", "value": "a@b.c"}],
		  "urn:ietf:params:scim:schemas:extension:enterprise:2.0:User": {"department": "Eng", "costCenter": "1"}
		}`), &m)
		return m
	}
	apply := func(t *testing.T, res map[string]any, opsJSON string) {
		t.Helper()
		body, err := decodeObject([]byte(`{"Operations":` + opsJSON + `}`))
		if err != nil {
			t.Fatal(err)
		}
		ops, e := parsePatch(body)
		if e != nil {
			t.Fatal(e)
		}
		if e := userPatcher.applyAll(res, ops); e != nil {
			t.Fatal(e)
		}
	}

	t.Run("replace a complex attribute merges its sub-attributes", func(t *testing.T) {
		res := start()
		apply(t, res, `[{"op":"replace","path":"name","value":{"givenName":"C"}}]`)
		if n := res["name"].(map[string]any); n["givenName"] != "C" || n["familyName"] != "B" {
			t.Fatalf("name = %v", n)
		}
	})
	t.Run("add to a multi-valued attribute appends without duplicating", func(t *testing.T) {
		res := start()
		apply(t, res, `[{"op":"add","path":"emails","value":[{"type":"work","value":"a@b.c"},{"type":"home","value":"h@b.c"}]}]`)
		if n := len(res["emails"].([]any)); n != 2 {
			t.Fatalf("emails = %v", res["emails"])
		}
	})
	t.Run("remove a whole extension drops its schema too", func(t *testing.T) {
		res := start()
		apply(t, res, `[{"op":"remove","path":"urn:ietf:params:scim:schemas:extension:enterprise:2.0:User"}]`)
		if _, ok := res[SchemaEnterpriseUser]; ok {
			t.Fatal("extension still present")
		}
		if s := res["schemas"].([]any); len(s) != 1 {
			t.Fatalf("schemas = %v", s)
		}
	})
	t.Run("a path-less value can carry a whole extension object", func(t *testing.T) {
		res := start()
		apply(t, res, `[{"op":"replace","value":{"urn:ietf:params:scim:schemas:extension:enterprise:2.0:User":{"department":"Ops"}}}]`)
		ext := res[SchemaEnterpriseUser].(map[string]any)
		if ext["department"] != "Ops" || ext["costCenter"] != "1" {
			t.Fatalf("extension = %v", ext)
		}
	})
	t.Run("an extension the resource did not have is created and declared", func(t *testing.T) {
		res := map[string]any{"schemas": []any{SchemaUser}, "userName": "a@b.c"}
		apply(t, res, `[{"op":"add","path":"urn:ietf:params:scim:schemas:extension:enterprise:2.0:User:department","value":"Eng"}]`)
		if res[SchemaEnterpriseUser].(map[string]any)["department"] != "Eng" || len(res["schemas"].([]any)) != 2 {
			t.Fatalf("resource = %v", res)
		}
	})
	t.Run("remove a sub-attribute of the filtered values only", func(t *testing.T) {
		res := start()
		apply(t, res, `[{"op":"add","path":"emails","value":[{"type":"home","value":"h@b.c","primary":true}]},
		               {"op":"remove","path":"emails[type eq \"home\"].primary"}]`)
		for _, e := range res["emails"].([]any) {
			if _, has := e.(map[string]any)["primary"]; has {
				t.Fatalf("emails = %v", res["emails"])
			}
		}
	})
	t.Run("read-only and secret attributes are ignored", func(t *testing.T) {
		res := start()
		apply(t, res, `[{"op":"replace","value":{"id":"x","meta":{},"password":"p","groups":[],"schemas":[]}}]`)
		for _, k := range []string{"id", "meta", "password", "groups"} {
			if _, has := res[k]; has {
				t.Fatalf("%s was written", k)
			}
		}
		if len(res["schemas"].([]any)) != 2 {
			t.Fatal("schemas was overwritten")
		}
	})
	t.Run("attribute names match in any case", func(t *testing.T) {
		res := start()
		apply(t, res, `[{"op":"Replace","path":"USERNAME","value":"z@b.c"},{"op":"replace","path":"Name.GivenName","value":"Z"}]`)
		if res["userName"] != "z@b.c" || res["name"].(map[string]any)["givenName"] != "Z" {
			t.Fatalf("resource = %v", res)
		}
	})
}

func TestChangedAttributesNamesNoValues(t *testing.T) {
	old := map[string]any{"userName": "a", "displayName": "A", SchemaEnterpriseUser: map[string]any{"department": "Eng"}}
	next := map[string]any{"userName": "a", "displayName": "B", "title": "T", SchemaEnterpriseUser: map[string]any{"department": "Ops"}}
	got := changedAttributes(old, next)
	want := []string{"displayname", "title", "urn:ietf:params:scim:schemas:extension:enterprise:2.0:user:department"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("changed = %v, want %v", got, want)
	}
}
