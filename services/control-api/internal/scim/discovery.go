package scim

import "strings"

// The discovery documents (RFC 7644 §4). They describe what this provider actually does, so a
// provider that reads them is not told about a capability (bulk, sort, etag, password change) that
// would then fail.

func (s *Service) serviceProviderConfig() map[string]any {
	doc := map[string]any{
		"schemas":          []any{SchemaServiceProviderConfig},
		"documentationUri": "",
		"patch":            map[string]any{"supported": true},
		"bulk":             map[string]any{"supported": false, "maxOperations": 0, "maxPayloadSize": 0},
		"filter":           map[string]any{"supported": true, "maxResults": MaxPageSize},
		"changePassword":   map[string]any{"supported": false},
		"sort":             map[string]any{"supported": false},
		"etag":             map[string]any{"supported": false},
		"authenticationSchemes": []any{map[string]any{
			"type":        "oauthbearertoken",
			"name":        "OAuth Bearer Token",
			"description": "A per-tenant SCIM token created in Settings > Deployment, sent as Authorization: Bearer.",
			"primary":     true,
		}},
		"meta": map[string]any{"resourceType": "ServiceProviderConfig"},
	}
	if s.cfg.BaseURL != "" {
		doc["meta"].(map[string]any)["location"] = s.cfg.BaseURL + "/ServiceProviderConfig"
	}
	return doc
}

func (s *Service) resourceTypes() []map[string]any {
	user := map[string]any{
		"schemas":     []any{SchemaResourceType},
		"id":          "User",
		"name":        "User",
		"endpoint":    "/Users",
		"description": "A person in the customer's directory",
		"schema":      SchemaUser,
		"schemaExtensions": []any{
			map[string]any{"schema": SchemaEnterpriseUser, "required": false},
			map[string]any{"schema": SchemaDirectoryUser, "required": false},
		},
		"meta": map[string]any{"resourceType": "ResourceType"},
	}
	group := map[string]any{
		"schemas":     []any{SchemaResourceType},
		"id":          "Group",
		"name":        "Group",
		"endpoint":    "/Groups",
		"description": "A group in the customer's directory",
		"schema":      SchemaGroup,
		"meta":        map[string]any{"resourceType": "ResourceType"},
	}
	if s.cfg.BaseURL != "" {
		user["meta"].(map[string]any)["location"] = s.cfg.BaseURL + "/ResourceTypes/User"
		group["meta"].(map[string]any)["location"] = s.cfg.BaseURL + "/ResourceTypes/Group"
	}
	return []map[string]any{user, group}
}

func attribute(name, typ string, multi, required bool, sub ...map[string]any) map[string]any {
	a := map[string]any{
		"name":        name,
		"type":        typ,
		"multiValued": multi,
		"description": "",
		"required":    required,
		"mutability":  "readWrite",
		"returned":    "default",
	}
	if typ == "string" || typ == "reference" {
		a["caseExact"] = false
		a["uniqueness"] = "none"
	}
	if len(sub) > 0 {
		list := make([]any, 0, len(sub))
		for _, s := range sub {
			list = append(list, s)
		}
		a["subAttributes"] = list
	}
	return a
}

func multiValued(name string) map[string]any {
	return attribute(name, "complex", true, false,
		attribute("value", "string", false, false),
		attribute("display", "string", false, false),
		attribute("type", "string", false, false),
		attribute("primary", "boolean", false, false))
}

func (s *Service) schemas() []map[string]any {
	userName := attribute("userName", "string", false, true)
	userName["uniqueness"] = "server"
	members := attribute("members", "complex", true, false,
		attribute("value", "string", false, false),
		attribute("$ref", "reference", false, false),
		attribute("type", "string", false, false))
	manager := attribute("manager", "complex", false, false,
		attribute("value", "string", false, false),
		attribute("$ref", "reference", false, false),
		attribute("displayName", "string", false, false))
	docs := []map[string]any{
		{
			"id": SchemaUser, "name": "User", "description": "User Account",
			"attributes": []any{
				userName,
				attribute("externalId", "string", false, false),
				attribute("name", "complex", false, false,
					attribute("formatted", "string", false, false),
					attribute("familyName", "string", false, false),
					attribute("givenName", "string", false, false),
					attribute("middleName", "string", false, false)),
				attribute("displayName", "string", false, false),
				attribute("nickName", "string", false, false),
				attribute("title", "string", false, false),
				attribute("userType", "string", false, false),
				attribute("preferredLanguage", "string", false, false),
				attribute("locale", "string", false, false),
				attribute("timezone", "string", false, false),
				attribute("active", "boolean", false, false),
				multiValued("emails"),
				multiValued("phoneNumbers"),
				attribute("addresses", "complex", true, false,
					attribute("formatted", "string", false, false),
					attribute("streetAddress", "string", false, false),
					attribute("locality", "string", false, false),
					attribute("region", "string", false, false),
					attribute("postalCode", "string", false, false),
					attribute("country", "string", false, false),
					attribute("type", "string", false, false),
					attribute("primary", "boolean", false, false)),
				multiValued("roles"),
			},
		},
		{
			"id": SchemaEnterpriseUser, "name": "EnterpriseUser", "description": "Enterprise User",
			"attributes": []any{
				attribute("employeeNumber", "string", false, false),
				attribute("costCenter", "string", false, false),
				attribute("organization", "string", false, false),
				attribute("division", "string", false, false),
				attribute("department", "string", false, false),
				manager,
			},
		},
		{
			"id": SchemaDirectoryUser, "name": "DirectoryUser", "description": "Where the person sits in the directory",
			"attributes": []any{
				// The parent of the person's on-premises distinguished name, such as
				// `OU=Sales,DC=contoso,DC=com`.
				attribute("orgUnit", "string", false, false),
			},
		},
		{
			"id": SchemaGroup, "name": "Group", "description": "Group",
			"attributes": []any{
				attribute("displayName", "string", false, true),
				attribute("externalId", "string", false, false),
				members,
			},
		},
	}
	for _, d := range docs {
		d["schemas"] = []any{SchemaSchema}
		m := map[string]any{"resourceType": "Schema"}
		if s.cfg.BaseURL != "" {
			m["location"] = s.cfg.BaseURL + "/Schemas/" + d["id"].(string)
		}
		d["meta"] = m
	}
	return docs
}

func findDoc(docs []map[string]any, id string) map[string]any {
	for _, d := range docs {
		if strings.EqualFold(d["id"].(string), id) {
			return d
		}
	}
	return nil
}
