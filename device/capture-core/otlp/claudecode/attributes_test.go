package claudecode

import (
	"sort"
	"testing"
)

// Reasons shared by many dropped keys.
const (
	noField     = "no envelope field"
	identity    = "identity comes from the sending process's owner, never from the tool"
	content     = "content (prompt, response, tool input, a command or a path); never read"
	unconverted = "sent only on events that are not converted"
	toolVersion = "the tool's version; prompt and agent_activity have no version field"
	platform    = "the device's own platform, which the agent reports itself"
	attribution = "agent, skill, plugin or MCP attribution; no envelope field"
	repository  = "repository identity; no envelope field"
)

// dropped is every attribute key Claude Code sends that the normalizer does not read, and why.
var dropped = map[string]string{
	// Resource.
	"service.version": toolVersion,
	"os.type":         platform,
	"os.version":      platform,
	"host.arch":       platform,

	// Standard attributes.
	"ccr.session.id":          "cloud session id; no envelope field",
	"app.version":             toolVersion,
	"app.entrypoint":          noField,
	"organization.id":         identity,
	"user.account_uuid":       identity,
	"user.account_id":         identity,
	"user.id":                 identity,
	"user.email":              identity,
	"terminal.type":           noField,
	"vcs.repository.url.full": repository,
	"vcs.owner.name":          repository,
	"vcs.repository.name":     repository,
	"vcs.provider.name":       repository,
	"prompt.id":               "correlation id; no envelope field",
	"workspace.host_paths":    content,
	"workflow.run_id":         noField,
	"workflow.name":           noField,

	// user_prompt.
	"message.uuid":   "transcript id; no envelope field",
	"command_name":   noField,
	"command_source": noField,

	// tool_result and tool_decision.
	"tool_use_id":            noField,
	"error_type":             "the outcome records error without its category",
	"error":                  "error text can quote the request or the tool's input; never read",
	"decision_source":        "who allowed the call; the outcome records only that it ran",
	"tool_input_size_bytes":  noField,
	"tool_result_size_bytes": noField,
	"mcp_server_scope":       noField,
	"vcs.ref.head.revision":  "commit identity; no envelope field",
	"vcs.ref.head.name":      "commit identity; no envelope field",
	"vcs.ref.head.type":      "commit identity; no envelope field",
	"tool_parameters":        content,
	"tool_input":             content,
	"tool_source":            noField,
	"source":                 "who rejected the call; denied covers a person's and a policy's rejection alike",

	// api_request and api_error.
	"cost_usd":              noField,
	"cost_usd_micros":       noField,
	"cache_read_tokens":     "cache tokens, which input_tokens excludes; no envelope field",
	"cache_creation_tokens": "cache tokens, which input_tokens excludes; no envelope field",
	"request_id":            noField,
	"client_request_id":     noField,
	"speed":                 noField,
	"query_source":          noField,
	"effort":                noField,
	"agent.name":            attribution,
	"skill.name":            attribution,
	"plugin.name":           attribution,
	"marketplace.name":      attribution,
	"mcp_server.name":       attribution,
	"mcp_tool.name":         attribution,
	"status_code":           noField,
	"attempt":               noField,

	// Only on events that are not converted.
	"response":                            content,
	"body":                                content,
	"body_ref":                            content,
	"hook_definitions":                    content,
	"managed_settings.helper.path":        content,
	"server_name":                         content,
	"response_length":                     unconverted,
	"action":                              unconverted,
	"additional_context_chars":            unconverted,
	"agent.source":                        unconverted,
	"agent_path_count":                    unconverted,
	"agent_type":                          unconverted,
	"appearance_id":                       unconverted,
	"artifacts_deleted":                   unconverted,
	"auth_method":                         unconverted,
	"blocked":                             unconverted,
	"body_length":                         unconverted,
	"body_truncated":                      unconverted,
	"category":                            unconverted,
	"command_path_count":                  unconverted,
	"enabled_via":                         unconverted,
	"enabled_via_override":                unconverted,
	"error.type":                          unconverted,
	"error_category":                      unconverted,
	"error_code":                          unconverted,
	"error_count":                         unconverted,
	"error_name":                          unconverted,
	"event_type":                          unconverted,
	"files_past_cutoff":                   unconverted,
	"files_retained_fresh":                unconverted,
	"final_model":                         unconverted,
	"findings":                            unconverted,
	"from_mode":                           unconverted,
	"has_category":                        unconverted,
	"has_explanation":                     unconverted,
	"has_hooks":                           unconverted,
	"has_mcp":                             unconverted,
	"hook_event":                          unconverted,
	"hook_matcher":                        unconverted,
	"hook_name":                           unconverted,
	"hook_source":                         unconverted,
	"hook_type":                           unconverted,
	"host_owned_mcp":                      unconverted,
	"initial_user_message_chars":          unconverted,
	"install.trigger":                     unconverted,
	"invocation_trigger":                  unconverted,
	"is_async":                            unconverted,
	"is_built_in":                         unconverted,
	"is_plugin":                           unconverted,
	"managed_only":                        unconverted,
	"managed_settings.helper.applied":     unconverted,
	"managed_settings.helper.entry":       unconverted,
	"managed_settings.helper.state":       unconverted,
	"managed_settings.resolved_sha256":    unconverted,
	"managed_settings.settings":           unconverted,
	"managed_settings.settings_truncated": unconverted,
	"managed_settings.source_behavior":    unconverted,
	"managed_settings.sources":            unconverted,
	"managed_settings.trigger":            unconverted,
	"marketplace.is_official":             unconverted,
	"mention_type":                        unconverted,
	"message.id":                          unconverted,
	"model_swapped":                       unconverted,
	"num_blocking":                        unconverted,
	"num_cancelled":                       unconverted,
	"num_hooks":                           unconverted,
	"num_non_blocking_error":              unconverted,
	"num_outputs_persisted":               unconverted,
	"num_success":                         unconverted,
	"period_days":                         unconverted,
	"plugin.scope":                        unconverted,
	"plugin.version":                      unconverted,
	"plugin_id":                           unconverted,
	"plugin_id_hash":                      unconverted,
	"post_tokens":                         unconverted,
	"pre_tokens":                          unconverted,
	"precompute_reuse":                    unconverted,
	"request_body_id":                     unconverted,
	"result":                              unconverted,
	"safe_mode":                           unconverted,
	"server_fallback_hop":                 unconverted,
	"server_scope":                        unconverted,
	"session_files_deleted":               unconverted,
	"skill.kind":                          unconverted,
	"skill.source":                        unconverted,
	"skill_path_count":                    unconverted,
	"skip_reason":                         unconverted,
	"status":                              unconverted,
	"stdout_chars":                        unconverted,
	"survey_type":                         unconverted,
	"system_message_chars":                unconverted,
	"to_mode":                             unconverted,
	"total_attempts":                      unconverted,
	"total_duration_ms":                   unconverted,
	"total_retry_duration_ms":             unconverted,
	"total_tokens":                        unconverted,
	"total_tool_uses":                     unconverted,
	"transcripts_deleted":                 unconverted,
	"transcripts_exempted_desktop":        unconverted,
	"transport_type":                      unconverted,
	"trigger":                             unconverted,
	"used_default":                        unconverted,
}

const housekeeping = "the tool's own configuration and housekeeping, not a prompt, model request or tool call"

// droppedEvents is every Claude Code event the normalizer does not convert, and why.
var droppedEvents = map[string]string{
	"assistant_response":        "the model's answer; the request is recorded from api_request",
	"api_refusal":               "a refusal ends a request that api_request records",
	"api_request_body":          "a raw API body; prompt text is read only from user_prompt, behind the mode gate",
	"api_response_body":         "a raw API body holding the model's answer",
	"api_retries_exhausted":     "a summary of failures that api_error records one by one",
	"at_mention":                "a reference inside a prompt that user_prompt records",
	"compaction":                "context housekeeping; its model request is recorded from api_request",
	"subagent_completed":        "a summary; the subagent's requests and tool calls are recorded one by one",
	"feedback_survey":           "the tool's own survey; no envelope kind",
	"system_prompt":             "the tool's system prompt, which no person typed",
	"permission_mode_changed":   housekeeping,
	"auth":                      housekeeping,
	"mcp_server_connection":     housekeeping,
	"internal_error":            housekeeping,
	"plugin_installed":          housekeeping,
	"plugin_loaded":             housekeeping,
	"skill_activated":           housekeeping,
	"hook_registered":           housekeeping,
	"hook_execution_start":      housekeeping,
	"hook_execution_complete":   housekeeping,
	"hook_plugin_metrics":       housekeeping,
	"retention_sweep":           housekeeping,
	"managed_settings_resolved": housekeeping,
}

// converted is every event the normalizer turns into an envelope (a tool_decision only on reject).
var converted = map[string]bool{
	eventUserPrompt:   true,
	eventToolResult:   true,
	eventToolDecision: true,
	eventAPIRequest:   true,
	eventAPIError:     true,
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

// TestEveryAttributeIsMappedOrDropped walks every log record in every fixture. A key Claude Code
// sends must be read by the normalizer or listed as dropped with a reason, and an event must be
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
					event := eventName(lr, readAttrs(lr.GetAttributes()))
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
