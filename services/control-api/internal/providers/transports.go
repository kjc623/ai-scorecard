package providers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

// The transports a provider's log arrives by.
const (
	KindPubSub   = "pubsub"
	KindFirehose = "firehose"
	KindWebhook  = "webhook"
)

// Google's ID token issuer and the keys it signs with.
const (
	googleIssuer   = "https://accounts.google.com"
	googleCertsURL = "https://www.googleapis.com/oauth2/v3/certs"
)

// PubSub receives a Google Cloud Pub/Sub push subscription's deliveries: one message each, the log
// entry in its data. Pub/Sub cannot add a header, so the subscription authenticates with an ID
// token Google signs for its service account, and the intake token is that ID token's audience.
// A subscription that unwraps payloads sends the data alone as the body.
type PubSub struct {
	verifier *oidc.IDTokenVerifier
}

// NewPubSub builds the transport. Google's signing keys are fetched with client when a token names
// one not yet seen; a nil client gets a bounded default.
func NewPubSub(client *http.Client) *PubSub {
	if client == nil {
		client = newHTTPClient()
	}
	return &PubSub{verifier: googleVerifier(oidc.NewRemoteKeySet(oidc.ClientContext(context.Background(), client), googleCertsURL))}
}

// googleVerifier checks a Google ID token's signature, issuer and expiry. The audience is the
// intake token, which the intake checks.
func googleVerifier(keys oidc.KeySet) *oidc.IDTokenVerifier {
	return oidc.NewVerifier(googleIssuer, keys, &oidc.Config{
		SkipClientIDCheck:    true,
		SupportedSigningAlgs: []string{oidc.RS256},
	})
}

// Kind implements Transport.
func (*PubSub) Kind() string { return KindPubSub }

// Token implements Transport.
func (p *PubSub) Token(ctx context.Context, r *http.Request) (string, error) {
	raw, ok := bearer(r)
	if !ok {
		return "", errors.New("providers: no bearer ID token")
	}
	tok, err := p.verifier.Verify(ctx, raw)
	if err != nil {
		return "", err
	}
	if len(tok.Audience) != 1 {
		return "", errors.New("providers: the ID token does not name one audience")
	}
	return tok.Audience[0], nil
}

// Entries implements Transport.
func (*PubSub) Entries(_ *http.Request, body []byte) ([][]byte, error) {
	var push struct {
		Message *struct {
			Data []byte `json:"data"`
		} `json:"message"`
		Subscription string `json:"subscription"`
	}
	if json.Unmarshal(body, &push) != nil || push.Message == nil || push.Subscription == "" {
		return [][]byte{body}, nil
	}
	if len(push.Message.Data) == 0 {
		return nil, nil
	}
	return [][]byte{push.Message.Data}, nil
}

// Reply implements Transport. Any 2xx acknowledges the message; anything else has it sent again.
func (*PubSub) Reply(w http.ResponseWriter, _ *http.Request, status int, message string) {
	if status == http.StatusOK {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Error(w, message, status)
}

// Firehose receives an Amazon Data Firehose HTTP endpoint's deliveries: a batch of records, each
// one entry. The stream's access key is the intake token.
type Firehose struct {
	Now func() time.Time
}

// Kind implements Transport.
func (*Firehose) Kind() string { return KindFirehose }

// Token implements Transport.
func (*Firehose) Token(_ context.Context, r *http.Request) (string, error) {
	key := strings.TrimSpace(r.Header.Get("X-Amz-Firehose-Access-Key"))
	if key == "" {
		return "", errors.New("providers: no Firehose access key")
	}
	return key, nil
}

// Entries implements Transport.
func (*Firehose) Entries(_ *http.Request, body []byte) ([][]byte, error) {
	var batch struct {
		RequestID string `json:"requestId"`
		Records   []struct {
			Data []byte `json:"data"`
		} `json:"records"`
	}
	if err := json.Unmarshal(body, &batch); err != nil {
		return nil, err
	}
	if batch.RequestID == "" {
		return nil, errors.New("providers: the batch has no requestId")
	}
	entries := make([][]byte, 0, len(batch.Records))
	for _, rec := range batch.Records {
		if len(rec.Data) > 0 {
			entries = append(entries, rec.Data)
		}
	}
	return entries, nil
}

// Reply implements Transport: the JSON Firehose requires, naming its request. Firehose drops a
// batch answered 413 without backing it up, so a batch over the cap is answered as a failure it
// retries and then backs up.
func (f *Firehose) Reply(w http.ResponseWriter, r *http.Request, status int, message string) {
	if status == http.StatusRequestEntityTooLarge {
		status = http.StatusInternalServerError
	}
	now := time.Now
	if f.Now != nil {
		now = f.Now
	}
	body := map[string]any{"requestId": r.Header.Get("X-Amz-Firehose-Request-Id"), "timestamp": now().UnixMilli()}
	if status != http.StatusOK {
		body["errorMessage"] = message
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// Webhook receives a provider's own webhook: one event per request, the intake token as its bearer.
type Webhook struct{}

// Kind implements Transport.
func (*Webhook) Kind() string { return KindWebhook }

// Token implements Transport.
func (*Webhook) Token(_ context.Context, r *http.Request) (string, error) {
	token, ok := bearer(r)
	if !ok {
		return "", errors.New("providers: no bearer token")
	}
	return token, nil
}

// Entries implements Transport.
func (*Webhook) Entries(_ *http.Request, body []byte) ([][]byte, error) { return [][]byte{body}, nil }

// Reply implements Transport.
func (*Webhook) Reply(w http.ResponseWriter, _ *http.Request, status int, message string) {
	if status == http.StatusOK {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Error(w, message, status)
}

// bearer is the request's bearer token.
func bearer(r *http.Request) (string, bool) {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	token = strings.TrimSpace(token)
	return token, ok && strings.EqualFold(scheme, "Bearer") && token != ""
}
