package providers

import (
	"encoding/json"
	"errors"
	"strings"
)

// geminiRequestLog is the payload type of a Gemini Code Assist request log entry.
const geminiRequestLog = "type.googleapis.com/google.cloud.cloudaicompanion.logging.v1.RequestLog"

// geminiLogEntry is a Cloud Logging entry, as a log sink publishes it, with the parts of a Gemini
// Code Assist request log the intake reads.
type geminiLogEntry struct {
	InsertID    string            `json:"insertId"`
	Timestamp   string            `json:"timestamp"`
	Labels      map[string]string `json:"labels"`
	JSONPayload *struct {
		Type                  string `json:"@type"`
		TaskCompletionRequest *struct {
			Input struct {
				Messages []struct {
					Author  string `json:"author"`
					Content string `json:"content"`
				} `json:"messages"`
			} `json:"input"`
		} `json:"taskCompletionRequest"`
	} `json:"jsonPayload"`
}

// DecodeGeminiCodeAssist reads a Gemini Code Assist request log entry, which Gemini for Google
// Cloud writes to Cloud Logging when the project logs prompts and responses. A chat or agent
// request (taskCompletionRequest) is the person's prompt: the last message they wrote. Inline code
// completions carry the editor's code, not a prompt, and responses and metadata are not prompts, so
// those entries are not records. A request from the Gemini CLI, which the IDE's agent mode also
// runs, names its client cloudcode_cli and is filed under the Gemini CLI.
func DecodeGeminiCodeAssist(entry []byte) ([]Record, error) {
	var e geminiLogEntry
	if err := json.Unmarshal(entry, &e); err != nil {
		return nil, err
	}
	if e.JSONPayload == nil || e.JSONPayload.Type != geminiRequestLog {
		if e.InsertID == "" {
			return nil, errors.New("providers: the entry is not a Cloud Logging entry")
		}
		return nil, nil
	}
	req := e.JSONPayload.TaskCompletionRequest
	if req == nil {
		return nil, nil
	}
	var text string
	for _, m := range req.Input.Messages {
		if strings.EqualFold(m.Author, "USER") && strings.TrimSpace(m.Content) != "" {
			text = m.Content
		}
	}
	id := e.Labels["request_id"]
	if id == "" {
		id = e.InsertID
	}
	app := "gemini_code_assist"
	if e.Labels["client_name"] == "cloudcode_cli" {
		app = "gemini_cli"
	}
	return []Record{{ID: id, OccurredAt: parseTime(e.Timestamp), App: app, UPN: e.Labels["user_id"], Text: text}}, nil
}
