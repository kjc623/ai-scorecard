package protocol

import (
	"encoding/json"
	"strings"
	"time"
)

// The GET /v1/policy exchange. The device authenticates with its current credential, sends the
// version it holds as If-None-Match, and receives either 304 with no body or a PolicyResponse
// whose SignedBundle it hands, byte for byte, to the policy verifier.
//
// The signed envelope travels inside the response as it was stored, not re-assembled from parts:
// the signature covers the payload bytes exactly as signed, so the server serves the bytes it
// signed and the device verifies the bytes it received. A device must decode SignedBundle as raw
// JSON (json.RawMessage keeps the bytes) and never re-serialise it before verifying.

// PolicySchemaVersion is the document version of the /v1/policy response.
const PolicySchemaVersion = "1.0"

// HeaderETag and HeaderIfNoneMatch carry the bundle version, quoted, as RFC 9110 entity tags.
const (
	HeaderETag        = "ETag"
	HeaderIfNoneMatch = "If-None-Match"
)

// PolicyResponse is the 200 body of GET /v1/policy.
type PolicyResponse struct {
	SchemaVersion string `json:"schema_version"`
	// BundleVersion is the bundle's version, monotonic per tenant; the same value the signed
	// payload's "version" carries and the ETag quotes.
	BundleVersion string `json:"bundle_version"`
	// SignedBundle is the signed envelope {key_id, algorithm, payload, signature} that
	// capture-core/policy verifies (Verifier.Open / Store.Apply), exactly as signed.
	SignedBundle json.RawMessage `json:"signed_bundle"`
	// NotAfter is when the server stops vouching for this bundle, when it says so. Absent means no
	// stated expiry: the device keeps polling and keeps the bundle in force until a newer verifies.
	NotAfter *time.Time `json:"not_after,omitempty"`
	// UpgradeRequired is set when the device's agent is below the bundle's minimum version.
	UpgradeRequired bool      `json:"upgrade_required"`
	ServerTime      time.Time `json:"server_time"`
}

// PolicyETag renders a bundle version as the entity tag GET /v1/policy sends and accepts.
func PolicyETag(bundleVersion string) string { return `"` + bundleVersion + `"` }

// ETagMatches reports whether an If-None-Match header value names etag. It accepts a list, weak
// tags (W/"..."), and "*", which RFC 9110 defines as matching any current representation.
func ETagMatches(ifNoneMatch, etag string) bool {
	if strings.TrimSpace(ifNoneMatch) == "" || etag == "" {
		return false
	}
	want := strings.TrimPrefix(etag, "W/")
	for _, part := range strings.Split(ifNoneMatch, ",") {
		tag := strings.TrimSpace(part)
		if tag == "*" {
			return true
		}
		if strings.TrimPrefix(tag, "W/") == want {
			return true
		}
	}
	return false
}
