package core

import (
	"encoding/json"
	"strings"

	"github.com/shadow-ai-capture/device/protocol"
)

// The device decides what kind of prompt a captured request is (contract envelopeCore.prompt_kind,
// task 08). Before this decision the classification ran over the whole captured body — the
// client's system prompt, tool definitions and re-sent history included — so a plain question
// could carry a `source_code` label taken from a tool schema, and a request no person wrote
// (Claude Code titling a session, summarising a turn, telemetry) was stored and indexed as if a
// person had typed it.
//
// The decision is a *shape* judgement, never a meaning judgement, and it is made once, here, so
// content-vault, query-api and the dashboard stop each carrying their own patch. Two signals:
//
//   - the text C1 extracted (docs/02 §4.2). When it is non-empty and matches a known client
//     pattern, the request is the client's own. Otherwise it is what a person typed.
//   - the body's shape, when no authored turn exists. A JSON body with a `messages` or `events`
//     member and no user text is a request the client made for itself.
//
// If neither signal decides, the answer is `unknown`, never `client_generated`: a false
// "client_generated" would hide a person's prompt, which the brief forbids, while a false
// "user" only shows a client's request.
type promptKindFunc func(payload []byte, extracted string) protocol.PromptKind

// clientPromptMarkers are known client-generated request patterns. They are the observable text
// of Claude Code's own meta-requests — the same strings that were being indexed as if a person
// had typed them — matched case-insensitively against a whitespace-collapsed copy of C1's output.
//
// This is a **seed**, not a closed vocabulary: a client that changes its wording stops matching,
// which degrades to `user` (show it) rather than hiding a person's prompt. A new marker is added
// when a captured body shows one; the cost of missing one is a client request shown in Search,
// and the cost of a too-broad marker is a person's prompt hidden, so each marker is a distinctive
// instruction phrase rather than a common word.
var clientPromptMarkers = []string{
	"write the title in the predominant language of the session",
	"generate a concise, sentence-case title",
	"describe your most recent action in 3-5 words",
	"your response above was cut off mid-stream",
	"another claude session sent a message",
	"[suggestion mode:",
}

// decidePromptKind is the default kind decider. It is a package-level function, swappable on the
// Pipeline, so a test can pin the decision without a client's exact wording.
func decidePromptKind(payload []byte, extracted string) protocol.PromptKind {
	text := collapseSpaces(extracted)
	if text != "" {
		for _, marker := range clientPromptMarkers {
			if strings.Contains(text, marker) {
				return protocol.PromptKindClientGenerated
			}
		}
		return protocol.PromptKindUser
	}
	if bodyHasNoAuthoredTurn(payload) {
		return protocol.PromptKindClientGenerated
	}
	return protocol.PromptKindUnknown
}

// bodyHasNoAuthoredTurn reports whether a JSON body is an AI request the client made for itself:
// it has the shape of a chat or telemetry call but no user text (C1 extracted nothing). A body
// that is not a JSON object is not recognised, so the answer falls back to `unknown`.
func bodyHasNoAuthoredTurn(payload []byte) bool {
	if len(payload) == 0 {
		return false
	}
	var body map[string]json.RawMessage
	if json.Unmarshal(payload, &body) != nil {
		return false
	}
	if _, ok := body["events"]; ok {
		return true // a telemetry batch carries no authored turn at all
	}
	if _, ok := body["messages"]; ok {
		return true // a chat body whose messages carried no user text
	}
	return false
}

// collapseSpaces lowercases and folds every run of whitespace to a single space, so a marker
// matches across a newline or an unusual indent without a regex per call.
func collapseSpaces(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	space := false
	for _, r := range strings.ToLower(s) {
		switch r {
		case ' ', '\t', '\n', '\r', '\f', '\v':
			if !space {
				b.WriteByte(' ')
				space = true
			}
		default:
			b.WriteRune(r)
			space = false
		}
	}
	return strings.TrimSpace(b.String())
}
