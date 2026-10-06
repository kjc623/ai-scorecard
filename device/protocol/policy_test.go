package protocol

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"
)

func TestPolicyETagRoundTrip(t *testing.T) {
	etag := PolicyETag("1759600123")
	if etag != `"1759600123"` {
		t.Fatalf("PolicyETag = %s", etag)
	}
	for _, c := range []struct {
		header string
		want   bool
	}{
		{`"1759600123"`, true},
		{`W/"1759600123"`, true},
		{`"1", "1759600123"`, true},
		{`*`, true},
		{`"1759600122"`, false},
		{``, false},
		{`1759600123`, false}, // an unquoted value is not an entity tag
	} {
		if got := ETagMatches(c.header, etag); got != c.want {
			t.Errorf("ETagMatches(%q) = %v, want %v", c.header, got, c.want)
		}
	}
}

// TestPolicyResponseKeepsSignedBytes pins the property the device relies on: the signed envelope
// survives the response's JSON round trip byte for byte, so its signature still covers what the
// device verifies.
//
// It also pins the one way to break it: encoding/json HTML-escapes a RawMessage it re-encodes, so an
// envelope holding a bare "<" is changed by a default encoder. The server therefore stores the
// envelope as json.Marshal wrote it (already escaped) and encodes the response with escaping off;
// either alone keeps the bytes, and the test shows both.
func TestPolicyResponseKeepsSignedBytes(t *testing.T) {
	payload, err := json.Marshal(map[string]any{"version": "7", "seed_hosts": []string{"a.example" + "<" + "com"}})
	if err != nil {
		t.Fatal(err)
	}
	escaped, err := json.Marshal(map[string]any{"key_id": "policy-key-1", "algorithm": "ed25519",
		"payload": json.RawMessage(payload), "signature": "c2ln"})
	if err != nil {
		t.Fatal(err)
	}
	unescaped := bytes.ReplaceAll(escaped, []byte(`\u003c`), []byte("<"))
	if bytes.Equal(escaped, unescaped) {
		t.Fatal("fixture should contain an escaped character")
	}
	at := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)

	roundTrip := func(envelope []byte, escapeHTML bool) []byte {
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(escapeHTML)
		if err := enc.Encode(PolicyResponse{SchemaVersion: PolicySchemaVersion, BundleVersion: "7",
			SignedBundle: envelope, ServerTime: at}); err != nil {
			t.Fatal(err)
		}
		var got PolicyResponse
		if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		return got.SignedBundle
	}
	if got := roundTrip(escaped, true); !bytes.Equal(got, escaped) {
		t.Errorf("an escaped envelope changed under a default encoder:\n got %s\nwant %s", got, escaped)
	}
	if got := roundTrip(unescaped, false); !bytes.Equal(got, unescaped) {
		t.Errorf("an envelope changed under an encoder with escaping off:\n got %s\nwant %s", got, unescaped)
	}
	if got := roundTrip(unescaped, true); bytes.Equal(got, unescaped) {
		t.Error("expected a default encoder to escape a bare '<'; the comment above would be wrong")
	}
}
