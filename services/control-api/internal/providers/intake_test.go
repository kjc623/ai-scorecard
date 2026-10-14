package providers

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

const (
	intakeTenant     = "22222222-2222-4222-8222-222222222222"
	intakeConnection = "c0000000-0000-4000-8000-0000000000aa"
	intakeToken      = IntakeTokenPrefix + "0123456789abcdefghijklmnopqrstuvwxyzABCDEFG"
)

// fakeIntakeStore is the database as the intake sees it: one active connection per provider in
// tokens, all with the same id and token, and what the deliveries recorded.
type fakeIntakeStore struct {
	*fakeStore
	tokens    map[string]string // provider -> token hash
	closed    bool
	delivered []Result
}

func (s *fakeIntakeStore) IntakeTenant(_ context.Context, provider, connectionID, hash string) (string, error) {
	if connectionID == intakeConnection && s.tokens[provider] != "" && s.tokens[provider] == hash {
		return intakeTenant, nil
	}
	return "", nil
}

func (s *fakeIntakeStore) IntakeOpen(context.Context, string, string) (bool, error) {
	return !s.closed, nil
}

func (s *fakeIntakeStore) Delivered(_ context.Context, tenant, connection string, _ time.Time, r Result) error {
	if tenant != intakeTenant || connection != intakeConnection {
		return errors.New("delivered to another connection")
	}
	s.delivered = append(s.delivered, r)
	return nil
}

type intakeRig struct {
	st  *fakeIntakeStore
	mux *http.ServeMux
	key *rsa.PrivateKey
}

func newIntakeRig(t *testing.T) *intakeRig {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	st := &fakeIntakeStore{fakeStore: newFakeStore(), tokens: map[string]string{
		GeminiCodeAssist: HashIntakeToken(intakeToken), AmazonBedrock: HashIntakeToken(intakeToken),
		PerplexityEnterprise: HashIntakeToken(intakeToken),
	}}
	pubsub := &PubSub{verifier: googleVerifier(&oidc.StaticKeySet{PublicKeys: []crypto.PublicKey{&key.PublicKey}})}
	firehose := &Firehose{Now: func() time.Time { return time.UnixMilli(1760000000123) }}
	rec := &Recorder{Store: st, Classifier: &fakeClassifier{}, Vault: &fakeVault{}}
	in, err := NewIntake(map[string]IntakeSource{
		GeminiCodeAssist:     {Transport: pubsub, Decode: DecodeGeminiCodeAssist},
		AmazonBedrock:        {Transport: firehose, Decode: CloudWatchLogs(DecodeBedrockInvocation)},
		PerplexityEnterprise: {Transport: &Webhook{}, Decode: DecodePerplexityEvent},
	}, st, rec, fakeKeys{}, "https://app.contoso.example/", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	in.Register(mux)
	return &intakeRig{st: st, mux: mux, key: key}
}

func (r *intakeRig) post(path string, header http.Header, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	for k, v := range header {
		req.Header[k] = v
	}
	rec := httptest.NewRecorder()
	r.mux.ServeHTTP(rec, req)
	return rec
}

// googleIDToken is an ID token as Google signs one for a push subscription's service account.
func (r *intakeRig) googleIDToken(t *testing.T, claims map[string]any) string {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: r.key}, (&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	base := map[string]any{"iss": "https://accounts.google.com", "aud": intakeToken, "email": "logs@proj.iam.gserviceaccount.com",
		"email_verified": true, "sub": "1", "iat": now.Unix(), "exp": now.Add(time.Hour).Unix()}
	for k, v := range claims {
		base[k] = v
	}
	tok, err := jwt.Signed(signer).Claims(base).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func pubsubPush(entry string) []byte {
	b, _ := json.Marshal(map[string]any{
		"message":      map[string]any{"data": base64.StdEncoding.EncodeToString([]byte(entry)), "messageId": "1", "publishTime": "2026-10-13T09:00:00Z"},
		"subscription": "projects/proj/subscriptions/sundial",
	})
	return b
}

const geminiRequest = `{"insertId":"abc","logName":"projects/proj/logs/cloudaicompanion.googleapis.com%2Frequest",
 "timestamp":"2026-10-13T09:00:00.123Z","labels":{"method":"CompleteTask","product":"code_assist","request_id":"e60facb1485b17b7",
 "user_id":"ada@contoso.example"},
 "jsonPayload":{"@type":"type.googleapis.com/google.cloud.cloudaicompanion.logging.v1.RequestLog",
  "taskCompletionRequest":{"input":{"messages":[{"author":"USER","content":"add one test here"}],"preamble":""}}}}`

func TestAPubSubDeliveryIsRecordedUnderItsConnection(t *testing.T) {
	r := newIntakeRig(t)
	path := "/intake/v1/gemini_code_assist/" + intakeConnection
	auth := http.Header{"Authorization": {"Bearer " + r.googleIDToken(t, nil)}}
	rec := r.post(path, auth, pubsubPush(geminiRequest))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delivery = %d %s", rec.Code, rec.Body)
	}
	if len(r.st.events) != 1 || len(r.st.delivered) != 1 {
		t.Fatalf("%d events, %d deliveries", len(r.st.events), len(r.st.delivered))
	}
	env := r.st.events[0]
	if env["tenant_id"] != intakeTenant || env["connection_id"] != intakeConnection || env["tool_fingerprint"] != "app:gemini_code_assist" ||
		env["event_id"] != EventID(intakeConnection, "e60facb1485b17b7") || env["source"] != RouteProviderAPI {
		t.Fatalf("envelope %v", env)
	}
	if d := r.st.delivered[0]; d.Read != 1 || d.New != 1 || d.Error != "" {
		t.Fatalf("delivered %+v", d)
	}
	// Pub/Sub sends a message again until it hears success; the second is the same event.
	if rec := r.post(path, auth, pubsubPush(geminiRequest)); rec.Code != http.StatusNoContent || r.st.delivered[1].New != 0 {
		t.Fatalf("redelivery = %d %+v", rec.Code, r.st.delivered)
	}
	// A subscription that unwraps payloads sends the entry alone.
	if rec := r.post(path, auth, []byte(geminiRequest)); rec.Code != http.StatusNoContent || r.st.delivered[2].Read != 1 {
		t.Fatalf("unwrapped = %d %+v", rec.Code, r.st.delivered)
	}
}

func TestAPubSubDeliveryProvesTheTokenWithAGoogleIDToken(t *testing.T) {
	r := newIntakeRig(t)
	path := "/intake/v1/gemini_code_assist/" + intakeConnection
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	forged := &intakeRig{key: other}
	for name, header := range map[string]http.Header{
		"no token":           {},
		"the bare token":     {"Authorization": {"Bearer " + intakeToken}},
		"another audience":   {"Authorization": {"Bearer " + r.googleIDToken(t, map[string]any{"aud": IntakeTokenPrefix + "other"})}},
		"two audiences":      {"Authorization": {"Bearer " + r.googleIDToken(t, map[string]any{"aud": []string{intakeToken, "x"}})}},
		"another issuer":     {"Authorization": {"Bearer " + r.googleIDToken(t, map[string]any{"iss": "https://issuer.example"})}},
		"expired":            {"Authorization": {"Bearer " + r.googleIDToken(t, map[string]any{"exp": time.Now().Add(-time.Hour).Unix()})}},
		"signed by another":  {"Authorization": {"Bearer " + forged.googleIDToken(t, nil)}},
		"another connection": {"Authorization": {"Bearer " + r.googleIDToken(t, nil)}},
	} {
		p := path
		if name == "another connection" {
			p = "/intake/v1/gemini_code_assist/c0000000-0000-4000-8000-0000000000bb"
		}
		if rec := r.post(p, header, pubsubPush(geminiRequest)); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s = %d", name, rec.Code)
		}
	}
	if len(r.st.events) != 0 || len(r.st.delivered) != 0 {
		t.Fatalf("a refused delivery was recorded: %d events, %v", len(r.st.events), r.st.delivered)
	}
	// Google writes its issuer without the scheme too.
	ok := http.Header{"Authorization": {"Bearer " + r.googleIDToken(t, map[string]any{"iss": "accounts.google.com"})}}
	if rec := r.post(path, ok, pubsubPush(geminiRequest)); rec.Code != http.StatusNoContent {
		t.Fatalf("scheme-less issuer = %d", rec.Code)
	}
}

// bedrockEvent is one model invocation log record, as Bedrock writes it to CloudWatch Logs.
func bedrockEvent(requestID, arn, body string) string {
	return `{"schemaType":"ModelInvocationLog","schemaVersion":"1.0","timestamp":"2026-10-13T09:00:00Z","accountId":"123456789012",
 "region":"us-east-1","requestId":"` + requestID + `","operation":"Converse","modelId":"anthropic.claude-sonnet-4-20250514-v1:0",
 "identity":{"arn":"` + arn + `"},"input":{"inputContentType":"application/json","inputBodyJson":` + body + `,"inputTokenCount":25}}`
}

// subscription is a CloudWatch Logs subscription delivery of messages, gzip-compressed as it
// reaches Firehose.
func subscription(t *testing.T, messageType string, messages ...string) []byte {
	t.Helper()
	var events []map[string]any
	for i, m := range messages {
		events = append(events, map[string]any{"id": string(rune('a' + i)), "timestamp": 1760000000000, "message": m})
	}
	raw, _ := json.Marshal(map[string]any{"owner": "123456789012", "logGroup": "bedrock", "logStream": "aws/bedrock/modelinvocations",
		"subscriptionFilters": []string{"sundial"}, "messageType": messageType, "logEvents": events})
	return gz(t, raw)
}

func gz(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func firehoseBatch(records ...[]byte) []byte {
	var rs []map[string]any
	for _, r := range records {
		rs = append(rs, map[string]any{"data": base64.StdEncoding.EncodeToString(r)})
	}
	b, _ := json.Marshal(map[string]any{"requestId": "ed4acda5-034f-9f42-bba1-f29aea6d7d8f", "timestamp": 1578090901599, "records": rs})
	return b
}

func TestAFirehoseBatchIsRecordedAndAnsweredAsFirehoseRequires(t *testing.T) {
	r := newIntakeRig(t)
	sso := "arn:aws:sts::123456789012:assumed-role/AWSReservedSSO_Dev_0123abcd/ada@contoso.example"
	converse := `{"messages":[{"role":"user","content":[{"text":"first"}]},{"role":"assistant","content":[{"text":"ok"}]},
	 {"role":"user","content":[{"text":"summarise the Q3 plan"},{"image":{"format":"png"}}]}],"system":[{"text":"be brief"}]}`
	batch := firehoseBatch(
		subscription(t, "DATA_MESSAGE", bedrockEvent("req-1", sso, converse), `not json`),
		subscription(t, "CONTROL_MESSAGE", "CWL CONTROL MESSAGE: Checking health of destination Firehose."),
	)
	header := http.Header{"X-Amz-Firehose-Access-Key": {intakeToken}, "X-Amz-Firehose-Request-Id": {"ed4acda5-034f-9f42-bba1-f29aea6d7d8f"},
		"Content-Type": {"application/json"}, "Content-Encoding": {"gzip"}}
	rec := r.post("/intake/v1/amazon_bedrock/"+intakeConnection, header, gz(t, batch))
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/json" ||
		strings.TrimSpace(rec.Body.String()) != `{"requestId":"ed4acda5-034f-9f42-bba1-f29aea6d7d8f","timestamp":1760000000123}` {
		t.Fatalf("answer = %d %q %s", rec.Code, rec.Header().Get("Content-Type"), rec.Body)
	}
	if len(r.st.events) != 1 {
		t.Fatalf("%d events", len(r.st.events))
	}
	if env := r.st.events[0]; env["tool_fingerprint"] != "app:aws_bedrock" || env["subject_name"] != "ada@contoso.example" ||
		env["size_bytes"] != float64(len("summarise the Q3 plan")) {
		t.Fatalf("envelope %v", env)
	}
	if d := r.st.delivered[0]; d.Read != 1 || d.New != 1 || d.Skipped != 1 {
		t.Fatalf("delivered %+v", d)
	}

	rec = r.post("/intake/v1/amazon_bedrock/"+intakeConnection, http.Header{"X-Amz-Firehose-Request-Id": {"r-2"}}, batch)
	var refused map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &refused)
	if rec.Code != http.StatusUnauthorized || refused["requestId"] != "r-2" || refused["errorMessage"] == "" {
		t.Fatalf("no access key = %d %s", rec.Code, rec.Body)
	}
}

func TestADeliveryThatCannotBeRecordedIsRefusedSoItIsSentAgain(t *testing.T) {
	r := newIntakeRig(t)
	webhook := http.Header{"Authorization": {"Bearer " + intakeToken}}
	path := "/intake/v1/perplexity_enterprise/" + intakeConnection
	query := `{"uuid":"6ba7b810-9dad-11d1-80b4-00c04fd430c8","timestamp":"2026-10-13T09:00:00+00:00","event_type":"query",
	 "user_email":"ada@contoso.example","metadata":{"query_str":"best practices for API security?"}}`

	r.st.recordErr = errors.New("database unavailable")
	if rec := r.post(path, webhook, []byte(query)); rec.Code != http.StatusInternalServerError {
		t.Fatalf("a failed record = %d", rec.Code)
	}
	if d := r.st.delivered[0]; d.Error != ErrInternal {
		t.Fatalf("delivered %+v", d)
	}
	r.st.recordErr = nil

	r.st.closed = true
	if rec := r.post(path, webhook, []byte(query)); rec.Code != http.StatusForbidden {
		t.Fatalf("closed ingest = %d", rec.Code)
	}
	r.st.closed = false

	if rec := r.post("/intake/v1/amazon_bedrock/"+intakeConnection, http.Header{"X-Amz-Firehose-Access-Key": {intakeToken}},
		[]byte(`{"records":[]}`)); rec.Code != http.StatusBadRequest {
		t.Fatalf("a batch with no request id = %d", rec.Code)
	}
	if d := r.st.delivered[len(r.st.delivered)-1]; d.Error != ErrDeliveryMalformed {
		t.Fatalf("delivered %+v", d)
	}

	bomb := gz(t, bytes.Repeat([]byte(" "), maxInflatedBytes+1))
	if rec := r.post(path, http.Header{"Authorization": {"Bearer " + intakeToken}, "Content-Encoding": {"gzip"}}, bomb); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("a delivery inflating past the cap = %d", rec.Code)
	}
	// Firehose would drop a batch answered 413 without backing it up.
	if rec := r.post("/intake/v1/amazon_bedrock/"+intakeConnection, http.Header{"X-Amz-Firehose-Access-Key": {intakeToken},
		"Content-Encoding": {"gzip"}}, bomb); rec.Code != http.StatusInternalServerError {
		t.Fatalf("a Firehose batch inflating past the cap = %d", rec.Code)
	}

	if rec := r.post("/intake/v1/no_such_provider/"+intakeConnection, webhook, []byte(query)); rec.Code != http.StatusNotFound {
		t.Fatalf("an unknown provider = %d", rec.Code)
	}
	if rec := r.post("/intake/v1/perplexity_enterprise/not-a-uuid", webhook, []byte(query)); rec.Code != http.StatusNotFound {
		t.Fatalf("a malformed connection = %d", rec.Code)
	}
	if rec := r.post(path, webhook, []byte(query)); rec.Code != http.StatusNoContent || len(r.st.events) != 1 {
		t.Fatalf("the delivery sent again = %d, %d events", rec.Code, len(r.st.events))
	}
}

func TestIntakeTokens(t *testing.T) {
	a, ha, err := NewIntakeToken()
	if err != nil {
		t.Fatal(err)
	}
	b, _, _ := NewIntakeToken()
	if a == b || !strings.HasPrefix(a, IntakeTokenPrefix) || len(a) != len(IntakeTokenPrefix)+43 {
		t.Fatalf("tokens %q %q", a, b)
	}
	if ha != HashIntakeToken(a) || !strings.HasPrefix(ha, "sha256:") || len(ha) != len("sha256:")+64 {
		t.Fatalf("hash %q", ha)
	}
}
