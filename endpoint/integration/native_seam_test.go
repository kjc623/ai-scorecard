package integration

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shadow-ai-capture/device/protocol"
)

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func joinLines(lines []string) string {
	return strings.Join(lines, "\n  ")
}

// The golden frames in testdata/native/ are produced by the extension's own encoder
// (extension/tools/emit-frames.mjs) and decoded here by the real protocol types: a frame produced
// by the component that sends it, decoded by the component that receives it, with nothing
// hand-written in between. If a test fails after an extension change, regenerate the frames and
// read the failing case's "why" before deciding which side is wrong.

type frameCase struct {
	Name    string          `json:"name"`
	Why     string          `json:"why"`
	Frame   json.RawMessage `json:"frame"`
	Expects frameExpects    `json:"expects"`
}

type frameExpects struct {
	Decodable       bool              `json:"decodable"`
	ContentBytes    int               `json:"content_bytes"`
	ContentText     string            `json:"content_text,omitempty"`
	ContentIsBinary bool              `json:"content_is_binary,omitempty"`
	HasContent      bool              `json:"has_content"`
	ContentDigest   string            `json:"content_digest,omitempty"`
	Counters        map[string]uint64 `json:"counters,omitempty"`
}

func loadFrames(t *testing.T) []frameCase {
	t.Helper()
	dir := filepath.Join("testdata", "native")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v (run: node extension/tools/emit-frames.mjs)", dir, err)
	}
	var cases []frameCase
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".json" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		var c frameCase
		if err := json.Unmarshal(raw, &c); err != nil {
			t.Fatalf("%s is not a frame case: %v", e.Name(), err)
		}
		cases = append(cases, c)
	}
	if len(cases) == 0 {
		t.Fatal("no frame cases: the seam has no coverage, which is not the same as the seam being correct")
	}
	return cases
}

// The framing contract: a message is {type, version, id?, body}, and the body of an observation is
// a protocol.ObservationMessage. A frame that decodes to the wrong content bytes is worse than one
// that fails to decode, so both directions are asserted.
func TestNativeFramesDecodeIntoTheProtocolTypes(t *testing.T) {
	for _, tc := range loadFrames(t) {
		t.Run(tc.Name, func(t *testing.T) {
			var frame protocol.NativeMessage
			if err := json.Unmarshal(tc.Frame, &frame); err != nil {
				if tc.Expects.Decodable {
					t.Fatalf("%s: frame does not decode as a NativeMessage: %v (%s)", tc.Name, err, tc.Why)
				}
				return
			}
			if frame.Type != protocol.TypeObservation {
				t.Fatalf("%s: frame type is %q, this file only covers observations", tc.Name, frame.Type)
			}
			if frame.Version != protocol.Version {
				t.Fatalf("%s: frame version %d, protocol speaks %d", tc.Name, frame.Version, protocol.Version)
			}

			var obs protocol.ObservationMessage
			if err := json.Unmarshal(frame.Body, &obs); err != nil {
				if tc.Expects.Decodable {
					t.Fatalf("%s: body does not decode as ObservationMessage: %v\n  why this case exists: %s\n"+
						"  NOTE: `content` is []byte on the Go side, so a raw-text payload fails here and a "+
						"payload that merely LOOKS like base64 decodes to different bytes - check whether the "+
						"producer base64-encodes before concluding the type is wrong.",
						tc.Name, err, tc.Why)
				}
				return
			}
			if !tc.Expects.Decodable {
				t.Fatalf("%s: the case expects a decode failure but the frame decoded", tc.Name)
			}

			if obs.HasContent != tc.Expects.HasContent {
				t.Fatalf("%s: has_content=%v, want %v", tc.Name, obs.HasContent, tc.Expects.HasContent)
			}
			if len(obs.Content) != tc.Expects.ContentBytes {
				t.Fatalf("%s: decoded %d content bytes, want %d (%s)", tc.Name, len(obs.Content), tc.Expects.ContentBytes, tc.Why)
			}
			if tc.Expects.ContentText != "" && string(obs.Content) != tc.Expects.ContentText {
				t.Fatalf("%s: decoded content %q, want %q - silent corruption is the failure this case exists to catch",
					tc.Name, string(obs.Content), tc.Expects.ContentText)
			}
			if obs.ContentIsBinary != tc.Expects.ContentIsBinary {
				t.Fatalf("%s: content_is_binary=%v, want %v", tc.Name, obs.ContentIsBinary, tc.Expects.ContentIsBinary)
			}
			if tc.Expects.ContentDigest != "" && obs.ContentDigest != tc.Expects.ContentDigest {
				t.Fatalf("%s: content_digest=%q, want %q", tc.Name, obs.ContentDigest, tc.Expects.ContentDigest)
			}

			// The digest must be over the bytes that arrived. A frame whose digest describes
			// different bytes than its content is the content-identity break this seam produced
			// once, and it is invisible unless the two are compared.
			if obs.ContentDigest != "" && len(obs.Content) > 0 {
				want := "sha256:" + sha256Hex(obs.Content)
				if obs.ContentDigest != want {
					t.Fatalf("%s: content_digest %q does not describe the %d content bytes that arrived (want %q)",
						tc.Name, obs.ContentDigest, len(obs.Content), want)
				}
			}
		})
	}
}

// Every case must be decodable by a producer that follows the documented encoding. A case that is
// meant to fail decoding would be testing the Go type rather than the seam, and this harness exists
// to test the seam - so an undecodable case is a finding about the producer, not a fixture.
func TestEveryGoldenFrameIsADecodableObservation(t *testing.T) {
	var undecodable []string
	for _, tc := range loadFrames(t) {
		if !tc.Expects.Decodable {
			undecodable = append(undecodable, tc.Name+": "+tc.Why)
		}
	}
	if len(undecodable) > 0 {
		t.Fatalf("these cases expect a decode failure, which means the producer does not follow the "+
			"documented encoding:\n  %s", joinLines(undecodable))
	}
}

// Content on the wire is base64 in every case, because `Content` is []byte. This asserts the
// property directly against the raw JSON, so it holds even if the Go type is changed to something
// that would accept raw text - the encoding of the frame is the contract, not the type's leniency.
func TestObservationContentIsBase64OnTheWire(t *testing.T) {
	for _, tc := range loadFrames(t) {
		t.Run(tc.Name, func(t *testing.T) {
			var frame struct {
				Body struct {
					Content *string `json:"content"`
				} `json:"body"`
			}
			if err := json.Unmarshal(tc.Frame, &frame); err != nil {
				t.Fatalf("frame does not decode: %v", err)
			}
			if frame.Body.Content == nil {
				return // no content field: nothing to encode, which is correct at M0
			}
			decoded, err := base64.StdEncoding.DecodeString(*frame.Body.Content)
			if err != nil {
				t.Fatalf("content is not valid base64 (%v). `protocol.ObservationMessage.Content` is []byte, "+
					"so encoding/json base64-decodes it: raw text fails outright, and text that happens to be "+
					"valid base64 decodes to DIFFERENT bytes than the producer hashed", err)
			}
			if len(decoded) != tc.Expects.ContentBytes {
				t.Fatalf("base64 decodes to %d bytes, the case expects %d", len(decoded), tc.Expects.ContentBytes)
			}
			// The check that catches silent corruption: a payload that is itself valid base64 decodes
			// happily to the wrong bytes, so "is it valid base64" is not sufficient. The decoded bytes
			// must be the bytes the case says they are.
			if tc.Expects.ContentText != "" && string(decoded) != tc.Expects.ContentText {
				t.Fatalf("content decodes to %q, but this case's payload is %q. If the producer sent the "+
					"payload unencoded, base64 decoding SUCCEEDS and yields different bytes - the digest then "+
					"describes content nobody sent, and no decode-error test can see it.",
					string(decoded), tc.Expects.ContentText)
			}
		})
	}
}
