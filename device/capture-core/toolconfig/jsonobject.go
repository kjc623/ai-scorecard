package toolconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"reflect"
)

// object is a JSON object that keeps its members' order and their values as written, so a merge
// changes only the members it sets.
type object struct{ members []member }

type member struct {
	key   string
	value json.RawMessage
}

var errNotObject = errors.New("not a JSON object")

// parseObject reads a JSON object. An empty document is an empty object.
func parseObject(data []byte) (*object, error) {
	o := &object{}
	if len(bytes.TrimSpace(data)) == 0 {
		return o, nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if tok != json.Delim('{') {
		return nil, errNotObject
	}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := kt.(string)
		if !ok {
			return nil, errNotObject
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		o.members = append(o.members, member{key: key, value: v})
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("data after the JSON object")
	}
	return o, nil
}

func (o *object) get(key string) (json.RawMessage, bool) {
	for i := len(o.members) - 1; i >= 0; i-- {
		if o.members[i].key == key {
			return o.members[i].value, true
		}
	}
	return nil, false
}

// set replaces the member in place, or appends it. A duplicate of the key is dropped, so the value
// set is the one every reader sees.
func (o *object) set(key string, v json.RawMessage) {
	out := o.members[:0]
	done := false
	for _, m := range o.members {
		if m.key != key {
			out = append(out, m)
			continue
		}
		if !done {
			out = append(out, member{key: key, value: v})
			done = true
		}
	}
	o.members = out
	if !done {
		o.members = append(o.members, member{key: key, value: v})
	}
}

func (o *object) delete(key string) {
	out := o.members[:0]
	for _, m := range o.members {
		if m.key != key {
			out = append(out, m)
		}
	}
	o.members = out
}

func (o *object) empty() bool { return len(o.members) == 0 }

// compact renders the object on one line.
func (o *object) compact() (json.RawMessage, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, m := range o.members {
		if i > 0 {
			b.WriteByte(',')
		}
		k, err := marshalString(m.key)
		if err != nil {
			return nil, err
		}
		b.Write(k)
		b.WriteByte(':')
		if err := json.Compact(&b, m.value); err != nil {
			return nil, err
		}
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// indented renders the object indented by two spaces, with a final new line.
func (o *object) indented() ([]byte, error) {
	c, err := o.compact()
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	if err := json.Indent(&b, c, "", "  "); err != nil {
		return nil, err
	}
	b.WriteByte('\n')
	return b.Bytes(), nil
}

// marshalString encodes s as a JSON string without escaping HTML characters.
func marshalString(s string) (json.RawMessage, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}

// sameJSON reports whether two objects hold the same values, whatever their formatting.
func sameJSON(a, b *object) bool {
	ca, err := a.compact()
	if err != nil {
		return false
	}
	cb, err := b.compact()
	if err != nil {
		return false
	}
	var va, vb any
	if json.Unmarshal(ca, &va) != nil || json.Unmarshal(cb, &vb) != nil {
		return false
	}
	return reflect.DeepEqual(va, vb)
}
