package providers

import (
	"encoding/json"
	"errors"
	"strings"
)

// CloudWatchLogs reads an entry that is a CloudWatch Logs subscription's delivery, gzip-compressed
// as a subscription filter sends it to Firehose, and decodes each log event's message with decode.
// The control messages CloudWatch Logs sends to check the destination carry no events.
func CloudWatchLogs(decode Decoder) Decoder {
	return func(entry []byte) ([]Record, error) {
		if isGzip(entry) {
			var err error
			if entry, err = gunzip(entry); err != nil {
				return nil, err
			}
		}
		var sub struct {
			MessageType string `json:"messageType"`
			LogEvents   []struct {
				Message string `json:"message"`
			} `json:"logEvents"`
		}
		if err := json.Unmarshal(entry, &sub); err != nil {
			return nil, err
		}
		switch sub.MessageType {
		case "CONTROL_MESSAGE":
			return nil, nil
		case "DATA_MESSAGE":
		default:
			return nil, errors.New("providers: the entry is not a CloudWatch Logs subscription delivery")
		}
		var out []Record
		var errs []error
		for _, e := range sub.LogEvents {
			recs, err := decode([]byte(e.Message))
			out = append(out, recs...)
			if err != nil {
				errs = append(errs, err)
			}
		}
		return out, errors.Join(errs...)
	}
}

// bedrockInvocation is an Amazon Bedrock model invocation log record, with the parts the intake
// reads.
type bedrockInvocation struct {
	SchemaType string `json:"schemaType"`
	Timestamp  string `json:"timestamp"`
	RequestID  string `json:"requestId"`
	Identity   struct {
		ARN string `json:"arn"`
	} `json:"identity"`
	Input struct {
		InputBodyJSON json.RawMessage `json:"inputBodyJson"`
	} `json:"input"`
}

// DecodeBedrockInvocation reads one Amazon Bedrock model invocation log record, which Bedrock writes
// to CloudWatch Logs for each Converse and InvokeModel call when the account logs invocations. The
// prompt is the text of the last message the caller wrote. A request body over Bedrock's inline
// limit is logged to S3 alone, so its record carries no prompt.
func DecodeBedrockInvocation(entry []byte) ([]Record, error) {
	var inv bedrockInvocation
	if err := json.Unmarshal(entry, &inv); err != nil {
		return nil, err
	}
	if inv.SchemaType != "ModelInvocationLog" {
		return nil, errors.New("providers: the event is not a Bedrock model invocation log")
	}
	upn, account := bedrockPrincipal(inv.Identity.ARN)
	return []Record{{ID: inv.RequestID, OccurredAt: parseTime(inv.Timestamp), App: "aws_bedrock",
		UPN: upn, Account: account, Text: bedrockPrompt(inv.Input.InputBodyJSON)}}, nil
}

// bedrockPrompt is the text of the last user message of a Bedrock request body: Converse's, the
// Anthropic and OpenAI-compatible message bodies, where content is a string or a list of blocks,
// and the single prompt of the text models' bodies.
func bedrockPrompt(raw json.RawMessage) string {
	var body struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		Prompt    string `json:"prompt"`
		InputText string `json:"inputText"`
		Message   string `json:"message"`
	}
	if json.Unmarshal(raw, &body) != nil {
		return ""
	}
	for i := len(body.Messages) - 1; i >= 0; i-- {
		if m := body.Messages[i]; m.Role == "user" {
			return messageText(m.Content)
		}
	}
	for _, s := range []string{body.Prompt, body.InputText, body.Message} {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// messageText is a message's content as text: the string itself, or its text blocks joined. A
// block that carries no text (an image, a tool's result) is not the person's.
func messageText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string  `json:"type"`
		Text *string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Text != nil && (b.Type == "" || b.Type == "text") && *b.Text != "" {
			parts = append(parts, *b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// bedrockPrincipal is who an invocation's AWS principal is. An IAM Identity Center user's role
// session is named after their user name, usually their email address, so a session or IAM user
// name with an @ is taken as the UPN. Any other principal is an account no directory names: a role,
// without the session name that changes with each session, or the principal's ARN.
func bedrockPrincipal(arn string) (upn, account string) {
	parts := strings.SplitN(arn, ":", 6)
	if len(parts) != 6 {
		return "", strings.TrimSpace(arn)
	}
	resource := parts[5]
	if rest, ok := strings.CutPrefix(resource, "assumed-role/"); ok {
		role, session, _ := strings.Cut(rest, "/")
		if strings.Contains(session, "@") {
			return session, ""
		}
		return "", strings.Join(parts[:5], ":") + ":assumed-role/" + role
	}
	if name, ok := strings.CutPrefix(resource, "user/"); ok {
		if i := strings.LastIndex(name, "/"); i >= 0 {
			name = name[i+1:]
		}
		if strings.Contains(name, "@") {
			return name, ""
		}
	}
	return "", arn
}
