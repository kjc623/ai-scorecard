// Package contract checks a device envelope against the event envelope schema.
//
// The schema is the one embedded in the generated contract package, compiled once with a JSON
// Schema 2020-12 validator that asserts formats: ingest.record_event() casts uuid and timestamp
// fields, so a malformed value must be one event's rejection here rather than an error that aborts
// the batch in the database. A violation is reported as a JSON Pointer and the violated constraint,
// never the offending value, which can be content.
package contract

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
	"github.com/shadow-ai-capture/contracts/envelope"
)

// Envelope is one device envelope parsed as a generic JSON object.
type Envelope struct {
	fields map[string]any
}

// Parse reads one envelope. It fails when the element is not a JSON object.
func Parse(raw []byte) (*Envelope, error) {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	fields, ok := doc.(map[string]any)
	if !ok {
		return nil, errors.New("contract: the envelope is not a JSON object")
	}
	return &Envelope{fields: fields}, nil
}

// String returns a string field, or "" when it is absent or not a string.
func (e *Envelope) String(name string) string {
	s, _ := e.fields[name].(string)
	return s
}

// Present returns the field names the envelope carries, sorted.
func (e *Envelope) Present() []string {
	names := make([]string, 0, len(e.fields))
	for name := range e.fields {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// contentFields are the declared fields that carry content or are derived from it. A quarantined
// envelope never holds them; ingest.rejected refuses them by constraint as well.
var contentFields = []string{"content_digest", "content_excerpt", "attachments"}

// Violation is one schema failure.
type Violation struct {
	Pointer  string // JSON Pointer into the envelope
	Expected string // the violated constraint
	// Mode is true when the constraint is a collection-mode rule (a field required or forbidden at
	// a given collection_mode), which the device is told as mode_violation.
	Mode bool
}

// Validator validates device envelopes against the contract.
type Validator struct {
	schema   *jsonschema.Schema
	doc      any
	declared map[string]bool
}

// NewValidator compiles the embedded contract schema.
func NewValidator() (*Validator, error) {
	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(envelope.Schema))
	if err != nil {
		return nil, fmt.Errorf("contract: the embedded schema is not JSON: %w", err)
	}
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	if err := c.AddResource(envelope.SchemaID, doc); err != nil {
		return nil, fmt.Errorf("contract: %w", err)
	}
	schema, err := c.Compile(envelope.SchemaID + "#/$defs/deviceSubmission")
	if err != nil {
		return nil, fmt.Errorf("contract: compile the schema: %w", err)
	}
	declared := map[string]bool{}
	if props, ok := lookup(doc, []string{"$defs", "envelopeCore", "properties"}).(map[string]any); ok {
		for name := range props {
			declared[name] = !slices.Contains(contentFields, name)
		}
	}
	return &Validator{schema: schema, doc: doc, declared: declared}, nil
}

// Redacted returns the envelope reduced to the declared fields that carry no content, for the
// quarantine table. An undeclared field is dropped with its value: its name is still in the
// presence map.
func (v *Validator) Redacted(e *Envelope) map[string]any {
	out := make(map[string]any, len(e.fields))
	for name, value := range e.fields {
		if v.declared[name] {
			out[name] = value
		}
	}
	return out
}

// Validate returns the envelope's first violation of the device-submission schema, or nil. When
// several constraints fail, a collection-mode rule is reported first, then the lowest pointer, so
// the answer is the same on every run.
func (v *Validator) Validate(e *Envelope) *Violation {
	err := v.schema.Validate(e.fields)
	if err == nil {
		return nil
	}
	var verr *jsonschema.ValidationError
	if !errors.As(err, &verr) {
		return &Violation{Expected: "a record the contract schema can evaluate"}
	}
	var found []Violation
	for _, leaf := range leaves(verr) {
		found = append(found, v.describe(leaf, e.fields))
	}
	sort.SliceStable(found, func(i, j int) bool {
		a, b := found[i], found[j]
		if a.Mode != b.Mode {
			return a.Mode
		}
		if a.Pointer != b.Pointer {
			return a.Pointer < b.Pointer
		}
		return a.Expected < b.Expected
	})
	return &found[0]
}

// leaves returns the errors that have no causes: the individual failed constraints.
func leaves(err *jsonschema.ValidationError) []*jsonschema.ValidationError {
	if len(err.Causes) == 0 {
		return []*jsonschema.ValidationError{err}
	}
	var out []*jsonschema.ValidationError
	for _, c := range err.Causes {
		out = append(out, leaves(c)...)
	}
	return out
}

// describe turns one failed constraint into a Violation. The schema location says which if/then
// branch the constraint belongs to, so a required or forbidden field is reported with the kind and
// mode that make it so.
func (v *Validator) describe(leaf *jsonschema.ValidationError, inst map[string]any) Violation {
	pointer := toPointer(leaf.InstanceLocation)
	schemaPath := fragment(leaf.SchemaURL)
	condition, mode := v.condition(schemaPath)
	when := ""
	if condition != "" {
		when = " when " + condition
	}

	switch k := leaf.ErrorKind.(type) {
	case *kind.Required:
		return Violation{Pointer: pointer + "/" + escape(k.Missing[0]), Expected: "required" + when, Mode: mode}
	case *kind.AdditionalProperties:
		names := slices.Sorted(slices.Values(k.Properties))
		return Violation{Pointer: pointer + "/" + escape(names[0]), Expected: "no such field in this schema version"}
	case *kind.Not:
		// The schema forbids fields with `not: {required: [...]}` or `not: {anyOf: [{required}...]}`;
		// report the first forbidden field the instance carries.
		object, _ := lookup(inst, leaf.InstanceLocation).(map[string]any)
		for _, name := range forbidden(lookup(v.doc, append(schemaPath, "not"))) {
			if _, ok := object[name]; ok {
				return Violation{Pointer: pointer + "/" + escape(name), Expected: "absent" + when, Mode: mode}
			}
		}
		return Violation{Pointer: pointer, Expected: "must not match a forbidden shape" + when, Mode: mode}
	case *kind.Const:
		return Violation{Pointer: pointer, Expected: "const " + compact(k.Want) + when, Mode: mode}
	case *kind.Enum:
		return Violation{Pointer: pointer, Expected: "one of " + compact(k.Want), Mode: mode}
	case *kind.Type:
		return Violation{Pointer: pointer, Expected: "type " + strings.Join(k.Want, " or ")}
	case *kind.Format:
		return Violation{Pointer: pointer, Expected: "format " + k.Want}
	case *kind.Pattern:
		return Violation{Pointer: pointer, Expected: "pattern " + k.Want}
	case *kind.MinLength:
		return Violation{Pointer: pointer, Expected: fmt.Sprintf("minLength %d", k.Want)}
	case *kind.MaxLength:
		return Violation{Pointer: pointer, Expected: fmt.Sprintf("maxLength %d", k.Want)}
	case *kind.MinItems:
		return Violation{Pointer: pointer, Expected: fmt.Sprintf("minItems %d", k.Want)}
	case *kind.MaxItems:
		return Violation{Pointer: pointer, Expected: fmt.Sprintf("maxItems %d", k.Want)}
	case *kind.Minimum:
		return Violation{Pointer: pointer, Expected: "minimum " + k.Want.RatString()}
	case *kind.Maximum:
		return Violation{Pointer: pointer, Expected: "maximum " + k.Want.RatString()}
	}
	return Violation{Pointer: pointer, Expected: strings.Join(leaf.ErrorKind.KeywordPath(), "/") + when, Mode: mode}
}

// condition describes the if/then branch that schemaPath lies in ("kind is prompt and
// collection_mode is m0"), and reports whether that branch is a collection-mode rule.
func (v *Validator) condition(schemaPath []string) (string, bool) {
	var cond map[string]any
	for i, seg := range schemaPath {
		if seg != "then" {
			continue
		}
		if parent, ok := lookup(v.doc, schemaPath[:i]).(map[string]any); ok {
			cond, _ = parent["if"].(map[string]any)
		}
	}
	props, _ := cond["properties"].(map[string]any)
	if len(props) == 0 {
		return "", false
	}
	var parts []string
	for _, name := range []string{"kind", "collection_mode"} {
		p, _ := props[name].(map[string]any)
		switch {
		case p == nil:
		case p["const"] != nil:
			parts = append(parts, fmt.Sprintf("%s is %v", name, p["const"]))
		case p["enum"] != nil:
			parts = append(parts, fmt.Sprintf("%s is one of %s", name, compact(p["enum"])))
		}
	}
	_, mode := props["collection_mode"]
	return strings.Join(parts, " and "), mode
}

// forbidden lists the field names a `not` schema forbids.
func forbidden(not any) []string {
	n, _ := not.(map[string]any)
	var names []string
	add := func(req any) {
		list, _ := req.([]any)
		for _, item := range list {
			if s, ok := item.(string); ok {
				names = append(names, s)
			}
		}
	}
	add(n["required"])
	anyOf, _ := n["anyOf"].([]any)
	for _, branch := range anyOf {
		if b, ok := branch.(map[string]any); ok {
			add(b["required"])
		}
	}
	return names
}

// lookup walks a parsed JSON document along path.
func lookup(doc any, path []string) any {
	for _, seg := range path {
		switch node := doc.(type) {
		case map[string]any:
			doc = node[seg]
		case []any:
			i, err := strconv.Atoi(seg)
			if err != nil || i < 0 || i >= len(node) {
				return nil
			}
			doc = node[i]
		default:
			return nil
		}
	}
	return doc
}

// fragment returns the JSON Pointer of a schema location URL as path segments.
func fragment(schemaURL string) []string {
	_, frag, _ := strings.Cut(schemaURL, "#")
	if unescaped, err := url.PathUnescape(frag); err == nil {
		frag = unescaped
	}
	frag = strings.TrimPrefix(frag, "/")
	if frag == "" {
		return nil
	}
	segs := strings.Split(frag, "/")
	for i, s := range segs {
		segs[i] = strings.NewReplacer("~1", "/", "~0", "~").Replace(s)
	}
	return segs
}

func toPointer(path []string) string {
	var b strings.Builder
	for _, seg := range path {
		b.WriteString("/" + escape(seg))
	}
	return b.String()
}

func escape(seg string) string {
	return strings.NewReplacer("~", "~0", "/", "~1").Replace(seg)
}

func compact(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}
