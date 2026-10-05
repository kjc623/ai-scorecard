package scim

import (
	"net/http"
	"strings"
)

// PATCH (RFC 7644 §3.5.2), as Entra and Okta send it. What the RFC alone would refuse and these
// providers send anyway, each accepted on purpose:
//
//   - `op` in any case: Entra sends "Replace", "Add", "Remove" unless the tenant opted in to its
//     compliance flag.
//   - booleans as strings: Entra's `"active": "False"`. They are normalised where the resource is
//     normalised (normalizeUser), so every route to `active` reads the same.
//   - no `path`, with an object value whose keys are themselves paths: Entra's
//     `{"op":"replace","value":{"name.givenName":"…","emails[type eq \"work\"].value":"…",
//     "urn:…:enterprise:2.0:User:department":"…"}}` and Okta's `{"op":"replace","value":{"active":false}}`.
//   - add or replace with no value, or a null one: treated as remove, the only reading that does
//     not invent data.
//   - remove with a value: Entra's `{"op":"Remove","path":"members","value":[{"value":"…"}]}`
//     removes those members, not the whole attribute.
//   - a filtered path that matches nothing on add or replace: the value it describes is created,
//     because Entra sets `emails[type eq "work"].value` on a user who has no work email yet.
//   - read-only attributes in a value (Okta sends the group's `id` in a rename): ignored, not
//     refused.

type patchOp struct {
	op       string
	path     string
	value    any
	hasValue bool
}

func parsePatch(body map[string]any) ([]patchOp, *Error) {
	raw, ok := getKey(body, "Operations")
	if !ok {
		return nil, badRequest("invalidSyntax", "a PATCH request carries an Operations array")
	}
	list, ok := raw.([]any)
	if !ok || len(list) == 0 {
		return nil, badRequest("invalidSyntax", "Operations must be a non-empty array")
	}
	ops := make([]patchOp, 0, len(list))
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, badRequest("invalidSyntax", "each operation is an object")
		}
		var op patchOp
		if v, ok := getKey(m, "op"); ok {
			s, _ := v.(string)
			op.op = strings.ToLower(strings.TrimSpace(s))
		}
		switch op.op {
		case "add", "replace", "remove":
		default:
			return nil, badRequest("invalidSyntax", "op must be add, replace or remove")
		}
		if v, ok := getKey(m, "path"); ok {
			s, isString := v.(string)
			if v != nil && !isString {
				return nil, badRequest("invalidPath", "path must be a string")
			}
			op.path = strings.TrimSpace(s)
		}
		op.value, op.hasValue = getKey(m, "value")
		ops = append(ops, op)
	}
	return ops, nil
}

// patcher applies operations to one resource representation. ignore names core attributes a
// provider may send but this resource does not take from it.
type patcher struct {
	ignore map[string]bool
}

func (pt patcher) applyAll(res map[string]any, ops []patchOp) *Error {
	for _, op := range ops {
		if op.path == "" {
			if op.op == "remove" {
				return &Error{Status: http.StatusBadRequest, ScimType: "noTarget", Detail: "remove needs a path"}
			}
			obj, ok := op.value.(map[string]any)
			if !ok {
				return badRequest("invalidValue", "an operation without a path needs an object value")
			}
			for _, k := range sortedKeys(obj) {
				if err := pt.applyKey(op.op, res, k, obj[k]); err != nil {
					return err
				}
			}
			continue
		}
		p, err := parsePath(op.path)
		if err != nil {
			return badRequest("invalidPath", "the operation's path is not a SCIM attribute path")
		}
		if err := pt.apply(op.op, res, p, op.value, op.hasValue); err != nil {
			return err
		}
	}
	return nil
}

// applyKey applies one key of a path-less value: a whole extension object, or an attribute path.
func (pt patcher) applyKey(op string, res map[string]any, k string, v any) *Error {
	if strings.EqualFold(k, "schemas") {
		return nil
	}
	if sub, isObj := v.(map[string]any); isObj && isSchemaKey(k) {
		p := attrPath{}
		if !isCoreSchema(k) {
			p.schema = k
			for _, known := range knownSchemas {
				if strings.EqualFold(k, known) {
					p.schema = known
				}
			}
		}
		for _, sk := range sortedKeys(sub) {
			q := p
			q.attr = sk
			if !validName(sk) {
				return badRequest("invalidPath", "an extension value carries a malformed attribute name")
			}
			if err := pt.apply(op, res, q, sub[sk], true); err != nil {
				return err
			}
		}
		return nil
	}
	p, err := parsePath(k)
	if err != nil {
		return badRequest("invalidPath", "a key of the operation's value is not a SCIM attribute path")
	}
	return pt.apply(op, res, p, v, true)
}

func (pt patcher) apply(op string, res map[string]any, p attrPath, v any, hasValue bool) *Error {
	if p.schema == "" && pt.ignore[strings.ToLower(p.attr)] {
		return nil
	}
	if op != "remove" && (!hasValue || v == nil) {
		op, v, hasValue = "remove", nil, false
	}
	if p.attr == "" {
		// The whole extension object.
		if op == "remove" {
			deleteKey(res, p.schema)
			removeSchema(res, p.schema)
			return nil
		}
		obj, ok := v.(map[string]any)
		if !ok {
			return badRequest("invalidValue", "an extension is replaced with an object")
		}
		for _, k := range sortedKeys(obj) {
			q := p
			q.attr = k
			if !validName(k) {
				return badRequest("invalidPath", "an extension value carries a malformed attribute name")
			}
			if err := pt.apply(op, res, q, obj[k], true); err != nil {
				return err
			}
		}
		return nil
	}
	c := container(res, p, op != "remove")
	if c == nil {
		return nil
	}
	key, exists := findKey(c, p.attr)
	if !exists {
		key = p.attr
	}
	switch {
	case p.filter == nil && p.sub == "":
		cur := c[key]
		switch op {
		case "remove":
			if arr, isArr := cur.([]any); isArr && hasValue {
				kept := removeValues(arr, asList(v))
				if len(kept) == 0 {
					delete(c, key)
				} else {
					c[key] = kept
				}
				return nil
			}
			delete(c, key)
		case "add":
			if arr, isArr := cur.([]any); isArr {
				c[key] = appendUnique(arr, asList(v))
				return nil
			}
			if arr, isArr := v.([]any); isArr {
				c[key] = appendUnique(nil, arr)
				return nil
			}
			if curObj, ok := cur.(map[string]any); ok {
				if vObj, ok := v.(map[string]any); ok {
					merge(curObj, vObj)
					return nil
				}
			}
			c[key] = v
		case "replace":
			if curObj, ok := cur.(map[string]any); ok {
				if vObj, ok := v.(map[string]any); ok {
					merge(curObj, vObj)
					return nil
				}
			}
			c[key] = v
		}
	case p.filter == nil:
		switch cur := c[key].(type) {
		case map[string]any:
			if op == "remove" {
				deleteKey(cur, p.sub)
			} else {
				setKey(cur, p.sub, v)
			}
		case []any:
			// A sub-attribute of every value of a multi-valued attribute.
			for _, e := range cur {
				if m, ok := e.(map[string]any); ok {
					if op == "remove" {
						deleteKey(m, p.sub)
					} else {
						setKey(m, p.sub, v)
					}
				}
			}
		default:
			if op != "remove" {
				c[key] = map[string]any{p.sub: v}
			}
		}
	default:
		arr, _ := c[key].([]any)
		out := make([]any, 0, len(arr)+1)
		matched := false
		for _, e := range arr {
			m, ok := e.(map[string]any)
			if !ok || !p.filter.match(m) {
				out = append(out, e)
				continue
			}
			matched = true
			switch {
			case op == "remove" && p.sub == "":
				continue
			case op == "remove":
				deleteKey(m, p.sub)
			case p.sub != "":
				setKey(m, p.sub, v)
			default:
				if vObj, ok := v.(map[string]any); ok {
					merge(m, vObj)
				} else {
					out = append(out, v)
					continue
				}
			}
			out = append(out, m)
		}
		if !matched && op != "remove" {
			elem := seed(p.filter)
			if elem == nil {
				return &Error{Status: http.StatusBadRequest, ScimType: "noTarget", Detail: "the path's filter matches no value and does not describe one"}
			}
			if p.sub != "" {
				setKey(elem, p.sub, v)
			} else if vObj, ok := v.(map[string]any); ok {
				merge(elem, vObj)
			}
			out = append(out, elem)
		}
		if len(out) == 0 {
			delete(c, key)
		} else {
			c[key] = out
		}
	}
	return nil
}

func merge(dst, src map[string]any) {
	for _, k := range sortedKeys(src) {
		if src[k] == nil {
			deleteKey(dst, k)
			continue
		}
		setKey(dst, k, src[k])
	}
}

func appendUnique(arr []any, add []any) []any {
	out := append([]any(nil), arr...)
	for _, v := range add {
		if v == nil {
			continue
		}
		dup := false
		for _, e := range out {
			if sameValue(e, v) {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, v)
		}
	}
	return out
}

func removeValues(arr []any, drop []any) []any {
	out := make([]any, 0, len(arr))
	for _, e := range arr {
		gone := false
		for _, d := range drop {
			if sameValue(e, d) {
				gone = true
				break
			}
		}
		if !gone {
			out = append(out, e)
		}
	}
	return out
}
