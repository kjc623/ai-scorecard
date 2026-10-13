package providers

import (
	"strings"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/protocol/canon"
)

func TestGeminiCodeAssistRequestLogs(t *testing.T) {
	recs, err := DecodeGeminiCodeAssist([]byte(geminiRequest))
	if err != nil || len(recs) != 1 {
		t.Fatalf("request = %v, %v", recs, err)
	}
	want := Record{ID: "e60facb1485b17b7", OccurredAt: time.Date(2026, 10, 13, 9, 0, 0, 123e6, time.UTC), App: "gemini_code_assist",
		UPN: "ada@contoso.example", Text: "add one test here"}
	if r := recs[0]; r.ID != want.ID || !r.OccurredAt.Equal(want.OccurredAt) || r.App != want.App || r.UPN != want.UPN || r.Text != want.Text {
		t.Fatalf("record %+v", r)
	}

	cli := strings.Replace(geminiRequest, `"method":"CompleteTask"`, `"method":"CompleteTask","client_name":"cloudcode_cli"`, 1)
	if recs, _ := DecodeGeminiCodeAssist([]byte(cli)); len(recs) != 1 || recs[0].App != "gemini_cli" {
		t.Fatalf("the CLI's request = %+v", recs)
	}

	history := strings.Replace(geminiRequest, `[{"author":"USER","content":"add one test here"}]`,
		`[{"author":"USER","content":"first"},{"author":"MODEL","content":"done"},{"author":"USER","content":"and now the second"}]`, 1)
	if recs, _ := DecodeGeminiCodeAssist([]byte(history)); len(recs) != 1 || recs[0].Text != "and now the second" {
		t.Fatalf("a conversation = %+v", recs)
	}

	for name, entry := range map[string]string{
		"a response": `{"insertId":"r1","labels":{"request_id":"e60facb1485b17b7"},"jsonPayload":{
		 "@type":"type.googleapis.com/google.cloud.cloudaicompanion.logging.v1.ResponseLog",
		 "taskCompletionResponse":{"output":{"messages":[{"author":"MODEL","content":"sure"}]}}}}`,
		"an inline completion": `{"insertId":"r2","labels":{"method":"CompleteCode"},"jsonPayload":{
		 "@type":"type.googleapis.com/google.cloud.cloudaicompanion.logging.v1.RequestLog",
		 "codeCompletionRequest":{"input_data_context":{"additional_context":{"files":[{"path":"a.go"}]}}}}}`,
		"metadata":            `{"insertId":"m1","jsonPayload":{"clientName":"CloudCodeVscode","clientVersion":"1.93.1"}}`,
		"another log's entry": `{"insertId":"x1","textPayload":"GET /healthz 200"}`,
	} {
		if recs, err := DecodeGeminiCodeAssist([]byte(entry)); err != nil || len(recs) != 0 {
			t.Errorf("%s = %v, %v", name, recs, err)
		}
	}
	for _, entry := range []string{`not json`, `{"hello":"world"}`} {
		if _, err := DecodeGeminiCodeAssist([]byte(entry)); err == nil {
			t.Errorf("%s decoded", entry)
		}
	}
}

func TestBedrockInvocationPrompts(t *testing.T) {
	const role = "arn:aws:sts::123456789012:assumed-role/DataPipeline/botocore-session-1760000000"
	for name, c := range map[string]struct{ body, want string }{
		"converse": {`{"messages":[{"role":"user","content":[{"text":"hello"},{"text":"world"}]}]}`, "hello\nworld"},
		"anthropic blocks": {`{"anthropic_version":"bedrock-2023-05-31","system":"x","messages":[{"role":"user",
		 "content":[{"type":"text","text":"What is the capital of France?"}]}]}`, "What is the capital of France?"},
		"anthropic string": {`{"messages":[{"role":"user","content":"plain"},{"role":"assistant","content":"reply"}]}`, "plain"},
		"tool results only": {`{"messages":[{"role":"user","content":"go"},{"role":"assistant","content":[{"type":"tool_use","id":"t"}]},
		 {"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":"42"}]}]}`, ""},
		"titan":  {`{"inputText":"Write a haiku"}`, "Write a haiku"},
		"llama":  {`{"prompt":"<s>[INST] hi [/INST]","max_gen_len":64}`, "<s>[INST] hi [/INST]"},
		"cohere": {`{"message":"Summarise","chat_history":[]}`, "Summarise"},
		"in s3":  {`null`, ""},
	} {
		recs, err := DecodeBedrockInvocation([]byte(bedrockEvent("req-"+name, role, c.body)))
		if err != nil || len(recs) != 1 {
			t.Fatalf("%s = %v, %v", name, recs, err)
		}
		if r := recs[0]; r.Text != c.want || r.ID != "req-"+name || r.App != "aws_bedrock" || r.OccurredAt.IsZero() {
			t.Errorf("%s = %+v", name, r)
		}
	}
	if _, err := DecodeBedrockInvocation([]byte(`{"eventVersion":"1.08","eventSource":"bedrock.amazonaws.com"}`)); err == nil {
		t.Error("a CloudTrail event decoded as an invocation")
	}
}

func TestBedrockPrincipals(t *testing.T) {
	for arn, want := range map[string][2]string{
		"arn:aws:sts::123456789012:assumed-role/AWSReservedSSO_Dev_0123abcd/Ada@Contoso.example": {"Ada@Contoso.example", ""},
		"arn:aws:sts::123456789012:assumed-role/DataPipeline/i-0abc":                             {"", "arn:aws:sts::123456789012:assumed-role/DataPipeline"},
		"arn:aws:iam::123456789012:user/ops/ada@contoso.example":                                 {"ada@contoso.example", ""},
		"arn:aws:iam::123456789012:user/ci-bot":                                                  {"", "arn:aws:iam::123456789012:user/ci-bot"},
		"arn:aws:iam::123456789012:root":                                                         {"", "arn:aws:iam::123456789012:root"},
		"":                                                                                       {"", ""},
	} {
		if upn, account := bedrockPrincipal(arn); upn != want[0] || account != want[1] {
			t.Errorf("%q = %q, %q", arn, upn, account)
		}
	}
}

func TestPerplexityQueries(t *testing.T) {
	recs, err := DecodePerplexityEvent([]byte(`{"uuid":"6ba7b810-9dad-11d1-80b4-00c04fd430c8","timestamp":"2026-10-13T14:25:12.345678+00:00",
	 "event_type":"query","user_email":"alice@contoso.example","ip_address":"203.0.113.45","session_id":"query_xyz789",
	 "metadata":{"query_str":"What are the best practices for API security?","attachments":[{"file_uuid":"f1","file_name":"threat-model.pdf"}]}}`))
	if err != nil || len(recs) != 1 {
		t.Fatalf("query = %v, %v", recs, err)
	}
	r := recs[0]
	if r.ID != "6ba7b810-9dad-11d1-80b4-00c04fd430c8" || r.App != "perplexity_web" || r.UPN != "alice@contoso.example" ||
		r.Text != "What are the best practices for API security?" || !r.OccurredAt.Equal(time.Date(2026, 10, 13, 14, 25, 12, 345678000, time.UTC)) ||
		len(r.Attachments) != 1 || r.Attachments[0] != (canon.Attachment{Name: "threat-model.pdf", ContentDigest: canon.Unreadable}) {
		t.Fatalf("record %+v", r)
	}

	recs, _ = DecodePerplexityEvent([]byte(`{"uuid":"u2","timestamp":"2025-02-20T09:05:31","event_type":"query",
	 "user_email":"bob@contoso.example","metadata":{"query":"older field name"}}`))
	if len(recs) != 1 || recs[0].Text != "older field name" || !recs[0].OccurredAt.Equal(time.Date(2025, 2, 20, 9, 5, 31, 0, time.UTC)) {
		t.Fatalf("a query without an offset = %+v", recs)
	}

	for _, e := range []string{
		`{"uuid":"u3","timestamp":"2026-10-13T09:00:00Z","event_type":"answer_generated","user_email":"a@b","metadata":{"answer":"x"}}`,
		`{"uuid":"u4","timestamp":"2026-10-13T09:00:00Z","event_type":"login","user_email":"a@b","metadata":{}}`,
	} {
		if recs, err := DecodePerplexityEvent([]byte(e)); err != nil || len(recs) != 0 {
			t.Errorf("%s = %v, %v", e, recs, err)
		}
	}
	if _, err := DecodePerplexityEvent([]byte(`{"event":{"uuid":"u5"}}`)); err == nil {
		t.Error("a Splunk-wrapped event decoded")
	}
}

func TestACloudWatchLogsDeliveryKeepsTheEventsThatDecode(t *testing.T) {
	decode := CloudWatchLogs(DecodeBedrockInvocation)
	role := "arn:aws:sts::123456789012:assumed-role/Ops/s"
	recs, err := decode(subscription(t, "DATA_MESSAGE", bedrockEvent("a", role, `{"inputText":"one"}`), "garbage", bedrockEvent("b", role, `{"inputText":"two"}`)))
	if err == nil || len(recs) != 2 || recs[0].Text != "one" || recs[1].Text != "two" {
		t.Fatalf("records %+v, error %v", recs, err)
	}
	if recs, err := decode(subscription(t, "CONTROL_MESSAGE", "CWL CONTROL MESSAGE")); err != nil || len(recs) != 0 {
		t.Fatalf("control message = %v, %v", recs, err)
	}
	if _, err := decode([]byte(bedrockEvent("c", role, `{"inputText":"bare"}`))); err == nil {
		t.Fatal("a bare event decoded as a subscription delivery")
	}
}
