package protocol

import "encoding/json"

// The agent's own update channel. Each release build signs a statement naming its Windows package
// (version, file, sha256, size) with the release signing key, whose public half every package pins
// (SAC_CLASSIFIER_PUBKEY: the key that also signs the classifier release). control-api serves the
// statement, as signed, and the package from the release it was deployed with; the device fetches
// both with its certificate, verifies the statement under the pinned key and the package against the
// statement, and installs a newer version over itself. The server holds no key that can sign one.
const (
	AgentReleasePath = "/v1/agent/release"
	AgentPackagePath = "/v1/agent/package"
)

// AgentReleaseType is the statement's type, so no other payload the release key signs can be read
// as one.
const AgentReleaseType = "agent_release"

// AgentPlatformWindowsAMD64 is the platform of the Windows MSI.
const AgentPlatformWindowsAMD64 = "windows-amd64"

// AgentRelease is the signed statement's payload.
type AgentRelease struct {
	Type     string `json:"type"`
	Platform string `json:"platform"`
	Version  string `json:"version"`
	// File is the package's file name, which an installer may depend on.
	File   string `json:"file"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// SignedAgentRelease is GET /v1/agent/release's 200 body: the Ed25519 signature (base64, standard
// encoding) covers Payload's bytes exactly as they appear here.
type SignedAgentRelease struct {
	Algorithm string          `json:"algorithm"`
	Payload   json.RawMessage `json:"payload"`
	Signature string          `json:"signature"`
}
