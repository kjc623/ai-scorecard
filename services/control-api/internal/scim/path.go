package scim

import (
	"errors"
	"reflect"
	"sort"
	"strings"
)

// The schema URNs this provider speaks.
const (
	SchemaUser           = "urn:ietf:params:scim:schemas:core:2.0:User"
	SchemaGroup          = "urn:ietf:params:scim:schemas:core:2.0:Group"
	SchemaEnterpriseUser = "urn:ietf:params:scim:schemas:extension:enterprise:2.0:User"
	// SchemaDirectoryUser is the product's own user extension: where the person sits in the
	// directory, for teams that follow an organisational unit.
	SchemaDirectoryUser         = "urn:ietf:params:scim:schemas:extension:sundial:2.0:User"
	SchemaListResponse          = "urn:ietf:params:scim:api:messages:2.0:ListResponse"
	SchemaPatchOp               = "urn:ietf:params:scim:api:messages:2.0:PatchOp"
	SchemaError                 = "urn:ietf:params:scim:api:messages:2.0:Error"
	SchemaServiceProviderConfig = "urn:ietf:params:scim:schemas:core:2.0:ServiceProviderConfig"
	SchemaResourceType          = "urn:ietf:params:scim:schemas:core:2.0:ResourceType"
	SchemaSchema                = "urn:ietf:params:scim:schemas:core:2.0:Schema"
)

// knownSchemas are matched as whole prefixes before the generic rule, so that both of Entra's
// spellings of an extension attribute, `…:2.0:User:department` and the older `…:2.0:User.department`,
// resolve to the same place.
var knownSchemas = []string{SchemaEnterpriseUser, SchemaDirectoryUser, SchemaUser, SchemaGroup}

var errBadPath = errors.New("scim: malformed attribute path")

// attrPath is one parsed RFC 7644 §3.10 attribute path:
//
//	[schemaURN ":"] attr ["[" filter "]"] ["." sub]
//
// schema is empty for the core schema (whose URN prefix is optional and stripped); for an extension it
// is the URN, and attr lives inside the resource's object of that name. attr empty means the whole
// extension object.
type attrPath struct {
	schema string
	attr   string
	filter expr
	sub    string
}

func parsePath(raw string) (attrPath, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return attrPath{}, errBadPath
	}
	var p attrPath
	if hasPrefixFold(s, "urn:") {
		schema, rest, ok := splitURN(s)
		if !ok {
			return attrPath{}, errBadPath
		}
		if !isCoreSchema(schema) {
			p.schema = schema
		}
		if rest == "" {
			if p.schema == "" {
				return attrPath{}, errBadPath
			}
			return p, nil
		}
		s = rest
	}
	i := strings.IndexAny(s, ".[")
	if i < 0 {
		p.attr = s
	} else {
		p.attr = s[:i]
		if s[i] == '[' {
			j := closingBracket(s, i)
			if j < 0 {
				return attrPath{}, errBadPath
			}
			f, err := parseFilter(s[i+1 : j])
			if err != nil {
				return attrPath{}, err
			}
			p.filter = f
			s = s[j+1:]
			if s != "" {
				if s[0] != '.' {
					return attrPath{}, errBadPath
				}
				p.sub = s[1:]
				if p.sub == "" {
					return attrPath{}, errBadPath
				}
			}
		} else {
			p.sub = s[i+1:]
			if p.sub == "" {
				return attrPath{}, errBadPath
			}
		}
	}
	if !validName(p.attr) || (p.sub != "" && !validName(p.sub)) {
		return attrPath{}, errBadPath
	}
	return p, nil
}

// splitURN separates a schema URN from the attribute path after it.
func splitURN(s string) (schema, rest string, ok bool) {
	for _, known := range knownSchemas {
		if len(s) >= len(known) && strings.EqualFold(s[:len(known)], known) {
			r := s[len(known):]
			if r == "" {
				return known, "", true
			}
			if r[0] == ':' || r[0] == '.' {
				return known, r[1:], true
			}
		}
	}
	// An extension this provider does not know (a customer's custom one): the URN ends at the last
	// colon before any value filter.
	head := s
	if k := strings.IndexByte(s, '['); k >= 0 {
		head = s[:k]
	}
	c := strings.LastIndexByte(head, ':')
	if c <= len("urn:") {
		return "", "", false
	}
	return s[:c], s[c+1:], true
}

// isSchemaKey reports whether a key in a path-less PATCH value names a whole extension object rather
// than one attribute: a known extension URN exactly, or an unknown URN that ends in a resource type.
func isSchemaKey(k string) bool {
	if !hasPrefixFold(k, "urn:") {
		return false
	}
	for _, known := range knownSchemas {
		if strings.EqualFold(k, known) {
			return true
		}
	}
	last := k[strings.LastIndexByte(k, ':')+1:]
	return strings.EqualFold(last, "User") || strings.EqualFold(last, "Group")
}

func isCoreSchema(s string) bool {
	return strings.EqualFold(s, SchemaUser) || strings.EqualFold(s, SchemaGroup)
}

func validName(s string) bool {
	if s == "" {
		return false
	}
	return !strings.ContainsAny(s, ".[]\" \t()")
}

func closingBracket(s string, open int) int {
	inQuote, escaped := false, false
	for i := open + 1; i < len(s); i++ {
		c := s[i]
		switch {
		case escaped:
			escaped = false
		case inQuote && c == '\\':
			escaped = true
		case c == '"':
			inQuote = !inQuote
		case !inQuote && c == ']':
			return i
		}
	}
	return -1
}

func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

// findKey looks a key up case-insensitively: SCIM attribute names are case-insensitive (RFC 7643
// §2.1), and Entra and Okta do not always spell them the way the resource was created.
func findKey(m map[string]any, name string) (string, bool) {
	if _, ok := m[name]; ok {
		return name, true
	}
	for k := range m {
		if strings.EqualFold(k, name) {
			return k, true
		}
	}
	return "", false
}

func getKey(m map[string]any, name string) (any, bool) {
	k, ok := findKey(m, name)
	if !ok {
		return nil, false
	}
	return m[k], true
}

func setKey(m map[string]any, name string, v any) {
	if k, ok := findKey(m, name); ok {
		m[k] = v
		return
	}
	m[name] = v
}

func deleteKey(m map[string]any, name string) {
	if k, ok := findKey(m, name); ok {
		delete(m, k)
	}
}

// container returns the object an attribute of p lives in: the resource itself for the core schema,
// or the extension object, created (and declared in `schemas`) when create is set.
func container(res map[string]any, p attrPath, create bool) map[string]any {
	if p.schema == "" {
		return res
	}
	if v, ok := getKey(res, p.schema); ok {
		if m, ok := v.(map[string]any); ok {
			return m
		}
	}
	if !create {
		return nil
	}
	m := map[string]any{}
	setKey(res, p.schema, m)
	addSchema(res, p.schema)
	return m
}

func addSchema(res map[string]any, urn string) {
	list, _ := getKey(res, "schemas")
	arr, _ := list.([]any)
	for _, s := range arr {
		if str, ok := s.(string); ok && strings.EqualFold(str, urn) {
			return
		}
	}
	setKey(res, "schemas", append(arr, urn))
}

func removeSchema(res map[string]any, urn string) {
	list, _ := getKey(res, "schemas")
	arr, _ := list.([]any)
	out := arr[:0:0]
	for _, s := range arr {
		if str, ok := s.(string); ok && strings.EqualFold(str, urn) {
			continue
		}
		out = append(out, s)
	}
	setKey(res, "schemas", out)
}

// values returns every value an attribute path names in target, flattening multi-valued attributes
// and sub-attributes of each value. It is what a filter compares and what a mapped attribute reads.
func values(target map[string]any, raw string) []any {
	p, err := parsePath(raw)
	if err != nil {
		return nil
	}
	return pathValues(target, p)
}

func pathValues(target map[string]any, p attrPath) []any {
	c := container(target, p, false)
	if c == nil {
		return nil
	}
	if p.attr == "" {
		return []any{c}
	}
	v, ok := getKey(c, p.attr)
	if !ok || v == nil {
		return nil
	}
	var elems []any
	if arr, ok := v.([]any); ok {
		for _, e := range arr {
			if p.filter != nil {
				m, ok := e.(map[string]any)
				if !ok || !p.filter.match(m) {
					continue
				}
			}
			elems = append(elems, e)
		}
	} else {
		if p.filter != nil {
			return nil
		}
		elems = []any{v}
	}
	if p.sub == "" {
		return elems
	}
	var out []any
	for _, e := range elems {
		if m, ok := e.(map[string]any); ok {
			if sv, ok := getKey(m, p.sub); ok && sv != nil {
				out = append(out, sv)
			}
		}
	}
	return out
}

// firstString is the first string value of an attribute path, trimmed; numbers are spelled as
// given. Empty when the attribute is absent.
func firstString(target map[string]any, raw string) string {
	for _, v := range values(target, raw) {
		switch t := v.(type) {
		case string:
			if s := strings.TrimSpace(t); s != "" {
				return s
			}
		case interface{ String() string }:
			return t.String()
		}
	}
	return ""
}

// sameValue is equality for multi-valued members: two complex values with a `value` are the same
// value when their `value`s are (Entra sends `{"$ref":null,"value":id}`, Okta `{"value":id,
// "display":…}`, for the same member); anything else compares whole.
func sameValue(a, b any) bool {
	am, aok := a.(map[string]any)
	bm, bok := b.(map[string]any)
	if aok && bok {
		av, aHas := getKey(am, "value")
		bv, bHas := getKey(bm, "value")
		if aHas && bHas {
			as, aStr := av.(string)
			bs, bStr := bv.(string)
			if aStr && bStr {
				return strings.EqualFold(as, bs)
			}
			return reflect.DeepEqual(av, bv)
		}
	}
	return reflect.DeepEqual(a, b)
}

func asList(v any) []any {
	if arr, ok := v.([]any); ok {
		return arr
	}
	return []any{v}
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
