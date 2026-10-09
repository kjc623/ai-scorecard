package copilot

import (
	"sort"
	"testing"
)

// Reasons shared by many dropped keys.
const (
	noField     = "no envelope field"
	identity    = "identity comes from the sending process's owner, never from the tool"
	content     = "content (a message, response, system prompt, tool input or output, a command or a path); never read"
	unconverted = "sent only on records that are not converted"
	toolVersion = "the tool's version; prompt and agent_activity have no version field"
	repository  = "repository identity; no envelope field"
	cache       = "cache or reasoning tokens, a breakdown of the usage counts; no envelope field"
	billing     = "Copilot billing units; no envelope field"
	correlation = "a correlation id; no envelope field"
	provider    = "the provider or endpoint the tool called; the fingerprint names the tool"
)

// dropped is every attribute key Copilot sends that the normalizer does not read, and why.
var dropped = map[string]string{
	// Resource.
	"service.version":   toolVersion,
	"session.id":        "the VS Code window's id; no envelope field",
	"process.user.name": identity,
	"host.name":         "the device's own name, which the agent reports itself",

	// Agent, model and tool spans.
	"user.name":                                           identity,
	"enduser.pseudo.id":                                   identity,
	"gen_ai.provider.name":                                provider,
	"server.address":                                      provider,
	"server.port":                                         provider,
	"gen_ai.agent.name":                                   "the agent mode; a prompt has no agent field",
	"gen_ai.agent.id":                                     "the agent mode; a prompt has no agent field",
	"gen_ai.agent.description":                            "the agent mode; a prompt has no agent field",
	"gen_ai.agent.version":                                toolVersion,
	"github.copilot.agent.type":                           "the agent mode; a prompt has no agent field",
	"gen_ai.conversation.id":                              correlation,
	"gen_ai.response.id":                                  correlation,
	"gen_ai.tool.call.id":                                 correlation,
	"github.copilot.turn_id":                              correlation,
	"github.copilot.interaction_id":                       correlation,
	"gen_ai.response.finish_reasons":                      noField,
	"gen_ai.request.max_tokens":                           noField,
	"gen_ai.request.temperature":                          noField,
	"gen_ai.request.top_p":                                noField,
	"gen_ai.request.stream":                               noField,
	"gen_ai.response.time_to_first_chunk":                 noField,
	"copilot_chat.time_to_first_token":                    noField,
	"github.copilot.server_duration":                      "the server's share of the request; duration_ms is the span's",
	"github.copilot.initiator":                            noField,
	"gen_ai.usage.cache_read.input_tokens":                cache,
	"gen_ai.usage.cache_creation.input_tokens":            cache,
	"gen_ai.usage.reasoning.output_tokens":                cache,
	"gen_ai.usage.reasoning_tokens":                       cache,
	"github.copilot.cost":                                 billing,
	"github.copilot.nano_aiu":                             billing,
	"copilot_chat.turn_count":                             "a count of the agent span's chat spans, each recorded",
	"github.copilot.turn_count":                           "a count of the agent span's chat spans, each recorded",
	"gen_ai.tool.type":                                    noField,
	"gen_ai.tool.description":                             "the tool's own description; tool_name names it",
	"github.copilot.tool.parameters.edit_type":            noField,
	"github.copilot.tool.parameters.skill_name":           "skill attribution; no envelope field",
	"github.copilot.tool.parameters.mcp_server_name_hash": "MCP attribution; tool_name names the call",
	"github.copilot.tool.parameters.mcp_tool_name":        "MCP attribution; tool_name names the call",
	"github.copilot.git.repository":                       repository,
	"github.copilot.git.branch":                           repository,
	"github.copilot.git.commit_sha":                       repository,
	"github.copilot.github.org":                           repository,
	"copilot_chat.repo.remote_url":                        repository,
	"copilot_chat.repo.head_branch_name":                  repository,
	"copilot_chat.repo.head_commit_hash":                  repository,

	// Content: the prompt is read from gen_ai.input.messages on the root agent span only.
	"gen_ai.output.messages":                         content,
	"gen_ai.system_instructions":                     content,
	"gen_ai.tool.definitions":                        content,
	"gen_ai.tool.call.arguments":                     content,
	"gen_ai.tool.call.result":                        content,
	"github.copilot.tool.parameters.command":         content,
	"github.copilot.tool.parameters.file_path":       content,
	"github.copilot.tool.parameters.mcp_server_name": content,
	"copilot_chat.hook_input":                        content,
	"copilot_chat.hook_output":                       content,
	"content":                                        content + " (the user_message span event repeats the agent span's message)",
	"github.copilot.message":                         content,
	"github.copilot.hook.error_message":              content,
	"github.copilot.skill.path":                      content,
	"copilot_chat.file.relative_path":                content,

	// Only on spans, span events and log records that are not converted.
	"github.copilot.hook.decision":          unconverted,
	"github.copilot.hook.duration":          unconverted,
	"github.copilot.hook.tool_names":        unconverted,
	"copilot_chat.hook_type":                unconverted,
	"copilot_chat.hook_result_kind":         unconverted,
	"github.copilot.hook.type":              unconverted,
	"github.copilot.hook.invocation_id":     unconverted,
	"github.copilot.skill.name":             unconverted,
	"github.copilot.skill.plugin_name":      unconverted,
	"github.copilot.skill.plugin_version":   unconverted,
	"github.copilot.token_limit":            unconverted,
	"github.copilot.pre_tokens":             unconverted,
	"github.copilot.post_tokens":            unconverted,
	"github.copilot.pre_messages":           unconverted,
	"github.copilot.post_messages":          unconverted,
	"github.copilot.tokens_removed":         unconverted,
	"github.copilot.messages_removed":       unconverted,
	"github.copilot.performed_by":           unconverted,
	"github.copilot.success":                unconverted,
	"github.copilot.error_type":             unconverted,
	"github.copilot.error_status_code":      unconverted,
	"github.copilot.error_provider_call_id": unconverted,
	"github.copilot.abort_reason":           unconverted,
	"github.copilot.shutdown_type":          unconverted,
	"github.copilot.total_premium_requests": unconverted,
	"github.copilot.lines_added":            unconverted,
	"github.copilot.lines_removed":          unconverted,
	"github.copilot.files_modified_count":   unconverted,
	"event.name":                            unconverted,
	"duration_ms":                           unconverted,
	"success":                               unconverted,
	"turn.index":                            unconverted,
	"tool_call_count":                       unconverted,
	"outcome":                               unconverted,
	"language_id":                           unconverted,
	"participant":                           unconverted,
	"request_id":                            unconverted,
	"edit_surface":                          unconverted,
	"has_remaining_edits":                   unconverted,
	"is_notebook":                           unconverted,
	"line_count":                            unconverted,
	"lines_added":                           unconverted,
	"lines_removed":                         unconverted,
	"accepted":                              unconverted,
	"edit_count":                            unconverted,
	"edit_line_count":                       unconverted,
	"reply_type":                            unconverted,
	"edit_source":                           unconverted,
	"survival_rate_four_gram":               unconverted,
	"survival_rate_no_revert":               unconverted,
	"time_delay_ms":                         unconverted,
	"did_branch_change":                     unconverted,
	"rating":                                unconverted,
	"conversation_id":                       unconverted,
	"partner_agent":                         unconverted,
	"model":                                 unconverted,
}

// convertedOps are the gen_ai.operation.name values whose spans become envelopes (invoke_agent
// only at the root of a trace).
var convertedOps = map[string]bool{opChat: true, opExecuteTool: true, opInvokeAgent: true}

// droppedOps is every other span operation Copilot sends, and why.
var droppedOps = map[string]string{
	"execute_hook": "a hook the person configured; the tool call it guards is recorded from execute_tool",
}

// droppedSpanEvents is every span event Copilot sends; the normalizer reads none.
var droppedSpanEvents = map[string]string{
	"user_message":                               "repeats the agent span's message, which is the prompt",
	"exception":                                  "an error the span's status and error.type record",
	"github.copilot.hook.start":                  "hook housekeeping",
	"github.copilot.hook.end":                    "hook housekeeping",
	"github.copilot.hook.error":                  "hook housekeeping",
	"github.copilot.session.truncation":          "context housekeeping",
	"github.copilot.session.compaction_start":    "context housekeeping",
	"github.copilot.session.compaction_complete": "context housekeeping; its model request is a chat span",
	"github.copilot.skill.invoked":               "skill attribution; no envelope kind",
	"github.copilot.session.abort":               "the person stopped a turn; its spans are recorded",
	"github.copilot.session.shutdown":            "a session summary; its requests and tool calls are recorded one by one",
}

// droppedLogEvents is every log event Copilot sends; the normalizer converts none.
var droppedLogEvents = map[string]string{
	"gen_ai.client.inference.operation.details": "repeats its chat span, which is recorded",
	"copilot_chat.tool.call":                    "repeats its execute_tool span, which is recorded",
	"copilot_chat.agent.turn":                   "repeats its chat span, which is recorded",
	"copilot_chat.session.start":                "a session's start; its prompt is its agent span",
	"copilot_chat.edit.feedback":                "an editor action on a result; no envelope kind",
	"copilot_chat.edit.hunk.action":             "an editor action on a result; no envelope kind",
	"copilot_chat.inline.done":                  "an editor action on a result; no envelope kind",
	"copilot_chat.edit.survival":                "an editor measurement; no envelope kind",
	"copilot_chat.user.feedback":                "a vote on a response; no envelope kind",
	"copilot_chat.cloud.session.invoke":         "a cloud agent started from VS Code runs off the device",
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
	for _, table := range []map[string]string{droppedOps, droppedSpanEvents, droppedLogEvents} {
		for name, why := range table {
			if why == "" {
				t.Errorf("dropped %q has no reason", name)
			}
			if convertedOps[name] {
				t.Errorf("operation %q is both converted and dropped", name)
			}
		}
	}
}

// TestEveryAttributeIsMappedOrDropped walks every resource, span, span event and log record in
// every fixture. A key Copilot sends must be read by the normalizer or listed as dropped with a
// reason, and a span operation, span event or log event must be converted or listed as dropped,
// so a release that adds or renames one fails here by name.
func TestEveryAttributeIsMappedOrDropped(t *testing.T) {
	unknownKeys := map[string][]string{}
	unknownNames := map[string]bool{}
	check := func(where string, keys []string) {
		for _, k := range keys {
			if !known(k) {
				unknownKeys[k] = append(unknownKeys[k], where)
			}
		}
	}
	for file, req := range traceFixtures(t) {
		for _, rs := range req.GetResourceSpans() {
			check(file+" resource", keyList(rs.GetResource().GetAttributes()))
			for _, ss := range rs.GetScopeSpans() {
				for _, s := range ss.GetSpans() {
					op := readAttrs(s.GetAttributes()).str(attrOperation)
					if !convertedOps[op] && droppedOps[op] == "" {
						unknownNames[file+": operation "+op] = true
					}
					check(file+" "+s.GetName(), keyList(s.GetAttributes()))
					for _, kv := range s.GetAttributes() {
						if dropped[kv.GetKey()] == unconverted && convertedOps[op] {
							t.Errorf("%s: %q is dropped as %q but %s spans are converted", file, kv.GetKey(), unconverted, op)
						}
					}
					for _, e := range s.GetEvents() {
						if droppedSpanEvents[e.GetName()] == "" {
							unknownNames[file+": span event "+e.GetName()] = true
						}
						check(file+" "+e.GetName(), keyList(e.GetAttributes()))
					}
				}
			}
		}
	}
	for file, req := range logFixtures(t) {
		for _, rl := range req.GetResourceLogs() {
			check(file+" resource", keyList(rl.GetResource().GetAttributes()))
			for _, sl := range rl.GetScopeLogs() {
				for _, lr := range sl.GetLogRecords() {
					name := logEventName(lr)
					if droppedLogEvents[name] == "" {
						unknownNames[file+": log event "+name] = true
					}
					check(file+" "+name, keyList(lr.GetAttributes()))
				}
			}
		}
	}
	for _, k := range sortedKeys(unknownKeys) {
		t.Errorf("attribute %q is neither mapped nor dropped (on %v)", k, unknownKeys[k])
	}
	for _, n := range sortedKeys(unknownNames) {
		t.Errorf("%s is neither converted nor dropped", n)
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
