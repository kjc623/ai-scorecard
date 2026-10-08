package codex

import (
	"sort"
	"testing"
)

// Reasons shared by many dropped keys.
const (
	noField     = "no envelope field"
	identity    = "identity comes from the sending process's owner, never from the tool"
	content     = "content (prompt, response, tool input or output, a rationale or an error text); never read"
	unconverted = "sent only on events that are not converted"
	toolVersion = "the tool's version; prompt and agent_activity have no version field"
	authDetail  = "how Codex authenticated the request; no envelope field"
)

// dropped is every attribute key Codex sends that the normalizer does not read, and why.
var dropped = map[string]string{
	// Resource.
	"service.version":        toolVersion,
	"telemetry.sdk.language": "the exporter SDK's identity; no envelope field",
	"telemetry.sdk.name":     "the exporter SDK's identity; no envelope field",
	"telemetry.sdk.version":  "the exporter SDK's identity; no envelope field",
	"env":                    "the otel.environment setting; no envelope field",
	"host.name":              "the device's own name, which the agent reports itself",

	// Standard attributes.
	"app.version":     toolVersion,
	"auth_mode":       authDetail,
	"originator":      "the client surface, which service.name already selects; no envelope field",
	"user.account_id": identity,
	"user.email":      identity,
	"user.id":         identity,
	"terminal.type":   noField,
	"slug":            "the model's display slug; model carries the model",

	// Requests and responses.
	"auth.header_attached":                        authDetail,
	"auth.header_name":                            authDetail,
	"auth.retry_after_unauthorized":               authDetail,
	"auth.recovery_mode":                          authDetail,
	"auth.recovery_phase":                         authDetail,
	"auth.connection_reused":                      authDetail,
	"auth.request_id":                             noField,
	"auth.cf_ray":                                 noField,
	"auth.error":                                  "the outcome records error without its category",
	"auth.error_code":                             "the outcome records error without its category",
	"auth.agent_id":                               noField,
	"auth.task_id":                                noField,
	"auth.env_openai_api_key_present":             authDetail,
	"auth.env_codex_api_key_present":              authDetail,
	"auth.env_codex_api_key_enabled":              authDetail,
	"auth.env_provider_key_name":                  authDetail,
	"auth.env_provider_key_present":               authDetail,
	"auth.env_refresh_token_url_override_present": authDetail,
	"endpoint":                                    "the API route; every route is a model request",
	"cached_token_count":                          "cache tokens, which input_tokens includes; no envelope field",
	"cache_write_token_count":                     "cache tokens, which input_tokens includes; no envelope field",
	"reasoning_token_count":                       "reasoning tokens, which output_tokens includes; no envelope field",
	"tool_token_count":                            "the total of input and output tokens, which those fields carry",
	"ttft_ms":                                     noField,
	"service_tier":                                noField,
	"model_reasoning_effort":                      noField,

	// tool_decision and tool_result.
	"tool_namespace":    noField,
	"source":            "who decided; denied covers a person's, a policy's and a reviewer's denial alike",
	"tool_result_seq":   "a per-process counter; call_id identifies the call",
	"product_sku":       noField,
	"output_truncated":  noField,
	"agent_name":        "agent attribution; no envelope field",
	"arguments":         content,
	"output":            content,
	"mcp_server":        "MCP attribution, a user-chosen server name; no envelope field",
	"mcp_server_origin": "MCP attribution; no envelope field",

	// Only on events that are not converted.
	"rationale":                      content,
	"response":                       content,
	"mcp_servers":                    "user-chosen MCP server names; sent only on events that are not converted",
	"skill.name":                     "a user-chosen skill name; sent only on events that are not converted",
	"provider_name":                  unconverted,
	"reasoning_effort":               unconverted,
	"reasoning_summary":              unconverted,
	"context_window":                 unconverted,
	"auto_compact_token_limit":       unconverted,
	"approval_policy":                unconverted,
	"sandbox_policy":                 unconverted,
	"auth.mode":                      unconverted,
	"auth.step":                      unconverted,
	"auth.outcome":                   unconverted,
	"auth.recovery_reason":           unconverted,
	"auth.state_changed":             unconverted,
	"outcome":                        unconverted,
	"initial_duration_ms":            unconverted,
	"escalated_duration_ms":          unconverted,
	"startup.phase":                  unconverted,
	"startup.status":                 unconverted,
	"turn.id":                        unconverted,
	"usage.estimated_usd":            unconverted,
	"turn.interrupted":               unconverted,
	"speed":                          unconverted,
	"plugin_install.tool_type":       unconverted,
	"plugin_install.tool_id":         unconverted,
	"plugin_install.tool_name":       unconverted,
	"plugin_install.response_action": unconverted,
	"plugin_install.user_confirmed":  unconverted,
	"plugin_install.completed":       unconverted,
	"skill.scope":                    unconverted,
	"skill.plugin_id":                unconverted,
	"skill.invocation_type":          unconverted,
	"agent.type":                     unconverted,
	"item.id":                        unconverted,
	"parent.conversation.id":         unconverted,
	"parent.turn.id":                 unconverted,
	"root.turn.id":                   unconverted,
	"initiating.agent.path":          unconverted,
	"response_length":                unconverted,
	"response_truncated":             unconverted,
	"review.id":                      unconverted,
	"status":                         unconverted,
	"risk_level":                     unconverted,
	"user_authorization":             unconverted,
	"started_at_ms":                  unconverted,
	"completed_at_ms":                unconverted,
	"rationale_length":               unconverted,
	"rationale_truncated":            unconverted,
}

const housekeeping = "the tool's own startup, connection and housekeeping, not a prompt, model request or tool call"

// droppedEvents is every Codex event the normalizer does not convert, and why.
var droppedEvents = map[string]string{
	"codex.conversation_starts":             "the session's settings; its prompts and requests are recorded one by one",
	"codex.websocket_connect":               "a connection, not a model request; its requests are recorded from websocket_request and sse_event",
	"codex.auth_recovery":                   housekeeping,
	"codex.startup_phase":                   housekeeping,
	"codex.turn_ttft":                       "a latency summary of a request that sse_event records",
	"codex.turn_cost":                       "a cost summary of a turn whose requests sse_event records",
	"codex.sandbox_outcome":                 "how a call ran in the sandbox; the call is recorded from tool_result",
	"codex.guardian_assessment":             "the automated reviewer's verdict; the call's decision is recorded from tool_decision",
	"codex.plugin_install_elicitation_sent": housekeeping,
	"codex.plugin_install_suggestion":       housekeeping,
	"codex.skill_invocation":                "skill attribution; no envelope kind",
	"codex.agent_response":                  "the model's answer; the request is recorded from sse_event",
}

// converted is every event the normalizer turns into an envelope: a tool_decision only when the
// call is denied, an api_request or websocket_request only when it fails, and an sse_event only
// for a completed response.
var converted = map[string]bool{
	eventUserPrompt:       true,
	eventToolDecision:     true,
	eventToolResult:       true,
	eventAPIRequest:       true,
	eventWebsocketRequest: true,
	eventSSE:              true,
}

func TestTheTablesAreConsistent(t *testing.T) {
	for k, why := range dropped {
		if why == "" {
			t.Errorf("dropped attribute %q has no reason", k)
		}
		if _, ok := mapped[k]; ok {
			t.Errorf("attribute %q is both mapped and dropped", k)
		}
	}
	for e, why := range droppedEvents {
		if why == "" {
			t.Errorf("dropped event %q has no reason", e)
		}
		if converted[e] {
			t.Errorf("event %q is both converted and dropped", e)
		}
	}
}

// TestEveryAttributeIsMappedOrDropped walks every log record in every fixture. A key Codex sends
// must be read by the normalizer or listed as dropped with a reason, and an event must be
// converted or listed as dropped, so a release that adds or renames one fails here by name.
func TestEveryAttributeIsMappedOrDropped(t *testing.T) {
	unknownKeys := map[string][]string{}
	unknownEvents := map[string]bool{}
	for file, req := range logFixtures(t) {
		for _, rl := range req.GetResourceLogs() {
			for _, kv := range rl.GetResource().GetAttributes() {
				if !known(kv.GetKey()) {
					unknownKeys[kv.GetKey()] = append(unknownKeys[kv.GetKey()], file+" resource")
				}
			}
			for _, sl := range rl.GetScopeLogs() {
				for _, lr := range sl.GetLogRecords() {
					event := readAttrs(lr.GetAttributes()).str(attrEventName)
					if !converted[event] && droppedEvents[event] == "" {
						unknownEvents[file+": "+event] = true
					}
					for _, kv := range lr.GetAttributes() {
						key := kv.GetKey()
						if !known(key) {
							unknownKeys[key] = append(unknownKeys[key], file+" "+event)
						}
						if dropped[key] == unconverted && converted[event] {
							t.Errorf("%s: %q is dropped as %q but %s is converted", file, key, unconverted, event)
						}
					}
				}
			}
		}
	}
	for _, k := range sortedKeys(unknownKeys) {
		t.Errorf("attribute %q is neither mapped nor dropped (on %v)", k, unknownKeys[k])
	}
	for _, e := range sortedKeys(unknownEvents) {
		t.Errorf("event %s is neither converted nor dropped", e)
	}
}

func known(key string) bool {
	_, m := mapped[key]
	_, d := dropped[key]
	return m || d
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
