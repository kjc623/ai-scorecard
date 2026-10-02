// Package contract validates a device envelope against the contract schema itself.
//
// ADR 0010 makes contracts/event-envelope.schema.json the only permitted source of field names,
// and ADR 0001 makes ingest-api the one validating write path. Until contracts/generated/go
// lands, the service cannot import generated types, so validation is performed by *reading the
// schema file* and walking the raw JSON with a draft-2020-12 subset evaluator. Nothing in this
// package re-declares the envelope shape: the field vocabulary (required lists, enums, pattern,
// properties) is read from the schema at load time, and accessor names live in envelope.go as a
// deliberately thin adapter (see the comment there).
//
// When contracts/generated/go/envelope exists, envelope.go's Decode is re-pointed at the
// generated types; this file keeps validating against the same schema, so the switch does not
// change behaviour.
package contract

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// DeviceSubmissionDef is the schema definition a device is permitted to send. §5.3's request
// body carries deviceSubmission records: received_at is server-assigned and a device sending it
// is rejected (contract comment on $defs/deviceSubmission).
const DeviceSubmissionDef = "deviceSubmission"

// Schema is a loaded contract schema.
type Schema struct {
	raw  map[string]any
	defs map[string]any
}

// Load reads a schema file from disk.
func Load(path string) (*Schema, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("contract: read schema %s: %w", path, err)
	}
	return Parse(b)
}

// Parse builds a Schema from raw bytes.
func Parse(b []byte) (*Schema, error) {
	var root map[string]any
	if err := json.Unmarshal(b, &root); err != nil {
		return nil, fmt.Errorf("contract: parse schema: %w", err)
	}
	defs, ok := root["$defs"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("contract: schema has no $defs object")
	}
	return &Schema{raw: root, defs: defs}, nil
}

// Discover walks up from start looking for contracts/event-envelope.schema.json, so the service
// runs from the repo root or from services/ingest-api without a flag.
func Discover(start string) (string, error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}
	for {
		candidate := filepath.Join(dir, "contracts", "event-envelope.schema.json")
		if st, err := os.Stat(candidate); err == nil && !st.IsDir() {
			return candidate, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("contract: could not find contracts/event-envelope.schema.json above %s", start)
		}
		dir = parent
	}
}

// def returns a named definition.
func (s *Schema) def(name string) (map[string]any, error) {
	d, ok := s.defs[name].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("contract: schema has no $defs/%s", name)
	}
	return d, nil
}

// RequiredFields returns the always-required envelope fields, sorted, straight from the schema.
// It is used for the §7 presence map, so the field vocabulary is never duplicated in Go.
func (s *Schema) RequiredFields() []string {
	core, err := s.def("envelopeCore")
	if err != nil {
		return nil
	}
	return sortedStrings(core["required"])
}

// FieldNames returns every declared envelopeCore property name, sorted. It is used to turn a
// generated decoder's error message (which names a field) into a JSON Pointer, so the diagmostic
// vocabulary also comes from the schema rather than from a list in Go.
func (s *Schema) FieldNames() []string {
	core, err := s.def("envelopeCore")
	if err != nil {
		return nil
	}
	props, _ := core["properties"].(map[string]any)
	out := make([]string, 0, len(props))
	for k := range props {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// KindRegistry returns the closed `kind` enum from the schema (D8/R7).
func (s *Schema) KindRegistry() []string {
	return s.enumOf("kind")
}

// RouteRegistry returns the closed `source` enum from the schema.
func (s *Schema) RouteRegistry() []string {
	return s.enumOf("source")
}

func (s *Schema) enumOf(prop string) []string {
	core, err := s.def("envelopeCore")
	if err != nil {
		return nil
	}
	props, _ := core["properties"].(map[string]any)
	if props == nil {
		return nil
	}
	p, _ := props[prop].(map[string]any)
	if p == nil {
		return nil
	}
	if ref, _ := p["$ref"].(string); ref != "" {
		if resolved, err := s.resolveRef(ref); err == nil {
			p = resolved
		}
	}
	return sortedStrings(p["enum"])
}

func (s *Schema) resolveRef(ref string) (map[string]any, error) {
	const prefix = "#/$defs/"
	if !strings.HasPrefix(ref, prefix) {
		return nil, fmt.Errorf("contract: unsupported $ref %q", ref)
	}
	return s.def(strings.TrimPrefix(ref, prefix))
}

// Violation is one schema failure, located by JSON Pointer. It deliberately carries no instance
// value: §7 requires the report to describe the violation's *shape*, never echo the value, which
// can be content.
type Violation struct {
	Pointer  string // JSON Pointer into the envelope (no /events/N prefix)
	Expected string // the violated constraint, as shape
}

func (v *Violation) Error() string {
	return fmt.Sprintf("contract: schema violation at %s: %s", v.Pointer, v.Expected)
}

// ValidateDeviceSubmission validates one raw envelope against $defs/deviceSubmission.
func (s *Schema) ValidateDeviceSubmission(raw []byte) (*Violation, error) {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	var inst any
	if err := dec.Decode(&inst); err != nil {
		return nil, fmt.Errorf("contract: envelope is not JSON: %w", err)
	}
	def, err := s.def(DeviceSubmissionDef)
	if err != nil {
		return nil, err
	}
	return s.validate(def, inst, ""), nil
}

// ValidateEnvelope is the ingest path's validation entry point, and it is deliberately two
// independent checks rather than one:
//
//  1. The generated contract types (contracts/generated/go/envelope, T1) decode the bytes. They are
//     the contract's own code, linked at compile time, and they enforce the per-kind and per-mode
//     required/forbidden field sets and `additionalProperties: false`.
//  2. The schema walk below re-derives the same verdict from
//     contracts/event-envelope.schema.json, and supplies the JSON Pointer and violated constraint
//     that §7's `detail` is made of. The generated decoder names the field in prose; the pointer is
//     what a device-side defect report can be reproduced from.
//
// A record must pass both. Where they can disagree, the schema walk is the stricter one and wins:
// draft 2020-12 treats `format` as an annotation, so the generated decoder asserts only that a uuid
// field is non-empty (its own documented choice), while this walker asserts the uuid shape. Ingest
// must assert it, because ingest.record_event() casts these fields to uuid and a malformed value
// would abort the whole batch in the database.
func (s *Schema) ValidateEnvelope(env *Envelope) (*Violation, error) {
	if env.genErr != nil {
		return s.violationFromGenerated(env.genErr), nil
	}
	return s.ValidateDeviceSubmission(env.rawBytes)
}

// violationFromGenerated turns a generated decoder error into a located Violation. The pointer is
// recovered by looking for a schema-declared field name in the message, so the vocabulary stays in
// the schema.
func (s *Schema) violationFromGenerated(err error) *Violation {
	msg := err.Error()
	pointer := ""
	// `additionalProperties: false` surfaces as encoding/json's unknown-field error, and the field
	// it names is by definition not in the schema, so it cannot be found by the scan below.
	if i := strings.Index(msg, "unknown field "); i >= 0 {
		rest := msg[i+len("unknown field "):]
		if len(rest) > 1 && rest[0] == '"' {
			if j := strings.IndexByte(rest[1:], '"'); j >= 0 {
				pointer = "/" + escapePointer(rest[1:1+j])
			}
		}
	}
	if pointer == "" {
		best := -1
		for _, name := range s.FieldNames() {
			if i := strings.Index(msg, name); i >= 0 {
				// Prefer the earliest mention: "missing required field(s): confidence, labels"
				// names the first offender first.
				if best == -1 || i < best {
					best, pointer = i, "/"+name
				}
			}
		}
	}
	expected := msg
	if i := strings.LastIndex(msg, ": "); i >= 0 && i+2 < len(msg) {
		expected = msg[i+2:]
	}
	return &Violation{Pointer: pointer, Expected: expected}
}

// validate walks the instance. It returns the first violation, or nil.
func (s *Schema) validate(node map[string]any, inst any, ptr string) *Violation {
	if ref, ok := node["$ref"].(string); ok && ref != "" {
		resolved, err := s.resolveRef(ref)
		if err != nil {
			return &Violation{Pointer: ptr, Expected: fmt.Sprintf("unresolvable schema reference %q", ref)}
		}
		if v := s.validate(resolved, inst, ptr); v != nil {
			return v
		}
	}

	if all, ok := node["allOf"].([]any); ok {
		for _, sub := range all {
			subNode, ok := sub.(map[string]any)
			if !ok {
				continue
			}
			if v := s.validate(subNode, inst, ptr); v != nil {
				return v
			}
		}
	}

	if cond, ok := node["if"].(map[string]any); ok {
		if s.validate(cond, inst, ptr) == nil {
			if then, ok := node["then"].(map[string]any); ok {
				if v := s.validate(then, inst, ptr); v != nil {
					return v
				}
			}
		} else if els, ok := node["else"].(map[string]any); ok {
			if v := s.validate(els, inst, ptr); v != nil {
				return v
			}
		}
	}

	if not, ok := node["not"].(map[string]any); ok {
		if s.validate(not, inst, ptr) == nil {
			return &Violation{Pointer: ptr, Expected: "the instance must not satisfy this branch: " + describe(not)}
		}
	}

	if anyOf, ok := node["anyOf"].([]any); ok {
		pass := false
		for _, sub := range anyOf {
			subNode, ok := sub.(map[string]any)
			if !ok {
				continue
			}
			if s.validate(subNode, inst, ptr) == nil {
				pass = true
				break
			}
		}
		if !pass {
			return &Violation{Pointer: ptr, Expected: "at least one of: " + describeAny(anyOf)}
		}
	}

	if c, ok := node["const"]; ok {
		if !jsonEqual(c, inst) {
			return &Violation{Pointer: ptr, Expected: fmt.Sprintf("const %s", compact(c))}
		}
	}

	if enum, ok := node["enum"].([]any); ok {
		found := false
		for _, e := range enum {
			if jsonEqual(e, inst) {
				found = true
				break
			}
		}
		if !found {
			return &Violation{Pointer: ptr, Expected: "one of " + compact(enum)}
		}
	}

	if t, ok := node["type"].(string); ok && !typeMatches(t, inst) {
		return &Violation{Pointer: ptr, Expected: t}
	}

	switch v := inst.(type) {
	case string:
		if pat, ok := node["pattern"].(string); ok {
			re, err := regexp.Compile(pat)
			if err == nil && !re.MatchString(v) {
				return &Violation{Pointer: ptr, Expected: "pattern " + pat}
			}
		}
		if n, ok := num(node["minLength"]); ok && utf8.RuneCountInString(v) < int(n) {
			return &Violation{Pointer: ptr, Expected: fmt.Sprintf("minLength %d", int(n))}
		}
		if n, ok := num(node["maxLength"]); ok && utf8.RuneCountInString(v) > int(n) {
			return &Violation{Pointer: ptr, Expected: fmt.Sprintf("maxLength %d", int(n))}
		}
		if f, ok := node["format"].(string); ok {
			if !formatMatches(f, v) {
				return &Violation{Pointer: ptr, Expected: "format " + f}
			}
		}
	case json.Number:
		f, err := v.Float64()
		if err == nil {
			if n, ok := num(node["minimum"]); ok && f < n {
				return &Violation{Pointer: ptr, Expected: fmt.Sprintf("minimum %s", compact(n))}
			}
			if n, ok := num(node["maximum"]); ok && f > n {
				return &Violation{Pointer: ptr, Expected: fmt.Sprintf("maximum %s", compact(n))}
			}
		}
	case []any:
		if n, ok := num(node["minItems"]); ok && len(v) < int(n) {
			return &Violation{Pointer: ptr, Expected: fmt.Sprintf("minItems %d", int(n))}
		}
		if n, ok := num(node["maxItems"]); ok && len(v) > int(n) {
			return &Violation{Pointer: ptr, Expected: fmt.Sprintf("maxItems %d", int(n))}
		}
		if items, ok := node["items"].(map[string]any); ok {
			for i, el := range v {
				if vi := s.validate(items, el, fmt.Sprintf("%s/%d", ptr, i)); vi != nil {
					return vi
				}
			}
		}
	case map[string]any:
		if req, ok := node["required"].([]any); ok {
			for _, rname := range req {
				name, _ := rname.(string)
				if _, present := v[name]; !present {
					return &Violation{Pointer: ptr, Expected: fmt.Sprintf("required field %q", name)}
				}
			}
		}
		props, _ := node["properties"].(map[string]any)
		// Deterministic order: a schema walk must not report a different field on each run.
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			child := ptr + "/" + escapePointer(k)
			if props != nil {
				if pn, ok := props[k].(map[string]any); ok {
					if vi := s.validate(pn, v[k], child); vi != nil {
						return vi
					}
					continue
				}
			}
			if ap, present := node["additionalProperties"]; present {
				switch t := ap.(type) {
				case bool:
					if !t {
						return &Violation{Pointer: child, Expected: "no such field in this schema version (additionalProperties: false)"}
					}
				case map[string]any:
					if vi := s.validate(t, v[k], child); vi != nil {
						return vi
					}
				}
			}
		}
	}

	return nil
}

func typeMatches(t string, inst any) bool {
	switch t {
	case "object":
		_, ok := inst.(map[string]any)
		return ok
	case "array":
		_, ok := inst.([]any)
		return ok
	case "string":
		_, ok := inst.(string)
		return ok
	case "boolean":
		_, ok := inst.(bool)
		return ok
	case "null":
		return inst == nil
	case "integer":
		n, ok := inst.(json.Number)
		if !ok {
			return false
		}
		_, err := n.Int64()
		return err == nil
	case "number":
		_, ok := inst.(json.Number)
		return ok
	}
	return true
}

// formatMatches asserts the two formats the contract actually uses. In draft 2020-12 `format` is
// an annotation by default, but the contract means it: a non-uuid event_id cannot be the key the
// store idempotency is built on.
func formatMatches(format, v string) bool {
	switch format {
	case "uuid":
		return uuidRE.MatchString(v)
	case "date-time":
		_, err := parseTime(v)
		return err == nil
	}
	return true
}

func num(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case int:
		return float64(t), true
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	}
	return 0, false
}

func jsonEqual(a, b any) bool {
	ab, err1 := json.Marshal(a)
	bb, err2 := json.Marshal(b)
	if err1 != nil || err2 != nil {
		return false
	}
	return string(ab) == string(bb)
}

func compact(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

func describe(node map[string]any) string {
	if req, ok := node["required"].([]any); ok {
		return "present " + compact(req)
	}
	if anyOf, ok := node["anyOf"].([]any); ok {
		return "present any of " + describeAny(anyOf)
	}
	return compact(node)
}

func describeAny(list []any) string {
	parts := make([]string, 0, len(list))
	for _, sub := range list {
		if m, ok := sub.(map[string]any); ok {
			parts = append(parts, describe(m))
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, " | ")
}

func escapePointer(s string) string {
	s = strings.ReplaceAll(s, "~", "~0")
	return strings.ReplaceAll(s, "/", "~1")
}

func sortedStrings(v any) []string {
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, e := range list {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}
