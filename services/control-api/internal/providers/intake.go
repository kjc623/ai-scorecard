package providers

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/shadow-ai-capture/control-api/internal/store"
)

// A provider with no API to read writes its record of each prompt to a log in the customer's own
// cloud. The customer forwards that log to the connection's intake URL, and every delivery
// presents the token the Integrations page issued when the provider was connected. A provider's
// intake is a Transport, how its deliveries arrive, and a Decoder, how each log entry they carry
// reads as records; the rest (the token, the tenant, recording and the connection's health) is the
// same for every provider. A delivery is recorded before it is answered, so a sender that does not
// hear success sends it again, and the event ids derived from the provider's own ids record it once.

// IntakeTokenPrefix marks an intake token.
const IntakeTokenPrefix = "sacintake_"

// ErrDeliveryMalformed is a delivery that is not what its transport sends. It is a closed
// ops.provider_connection.last_error code.
const ErrDeliveryMalformed = "delivery_malformed"

// Bounds on one delivery: its body as sent, and once a transport has decompressed it.
const (
	MaxDeliveryBytes   = 16 << 20
	maxInflatedBytes   = 64 << 20
	intakeTokenEntropy = 32
)

// Transport is how one provider's deliveries arrive.
type Transport interface {
	// Kind names what the customer sets up to send, for the Integrations page: "pubsub" (a Google
	// Cloud Pub/Sub push subscription), "firehose" (an Amazon Data Firehose HTTP endpoint) or
	// "webhook" (the provider's own webhook).
	Kind() string
	// Token is the intake token the delivery presents; an error when it presents none this
	// transport accepts.
	Token(ctx context.Context, r *http.Request) (string, error)
	// Entries unwraps a delivery's body, decompressed, into the log entries it carries.
	Entries(r *http.Request, body []byte) ([][]byte, error)
	// Reply answers the sender as it expects: status is the outcome, message says why it failed.
	Reply(w http.ResponseWriter, r *http.Request, status int, message string)
}

// Decoder reads one log entry into the records of prompts it holds; none for an entry that is not a
// prompt. An error is an entry, or a part of one, that is not the provider's log; the records it
// returns beside the error are the parts that are.
type Decoder func(entry []byte) ([]Record, error)

// IntakeSource is one provider's intake.
type IntakeSource struct {
	Transport Transport
	Decode    Decoder
}

// IntakeStore is what the intake needs of the database.
type IntakeStore interface {
	RecordStore
	// IntakeTenant returns the tenant whose active connection to provider, connectionID, holds the
	// token whose hash is tokenHash; "" when none does.
	IntakeTenant(ctx context.Context, provider, connectionID, tokenHash string) (string, error)
	// IntakeOpen reports whether the connection is active and its tenant's ingest is open.
	IntakeOpen(ctx context.Context, tenantID, connectionID string) (bool, error)
	// Delivered records a delivery's outcome on its connection.
	Delivered(ctx context.Context, tenantID, connectionID string, at time.Time, r Result) error
	// Policy reads the tenant's settings a delivery records under.
	Policy(ctx context.Context, tenantID string) (Policy, error)
}

// Intake receives the deliveries of every provider this deployment reads from a log.
type Intake struct {
	Sources     map[string]IntakeSource
	Store       IntakeStore
	Recorder    *Recorder
	UserRefKeys UserRefKeys
	// BaseURL is the public origin the customer's cloud sends to (SAC_PUBLIC_URL).
	BaseURL string
	Logger  *slog.Logger
	Now     func() time.Time
}

// NewIntake wires the intake.
func NewIntake(sources map[string]IntakeSource, st IntakeStore, rec *Recorder, refs UserRefKeys, baseURL string, logger *slog.Logger) (*Intake, error) {
	if st == nil || rec == nil || refs == nil || baseURL == "" {
		return nil, errors.New("providers: the intake needs a store, a recorder, the user_ref keys and the public URL")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Intake{Sources: sources, Store: st, Recorder: rec, UserRefKeys: refs,
		BaseURL: strings.TrimRight(baseURL, "/"), Logger: logger, Now: time.Now}, nil
}

// IntakePathPrefix is where deliveries arrive: <prefix><provider>/<connection id>.
const IntakePathPrefix = "/intake/v1/"

// Register adds the delivery route to mux.
func (in *Intake) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST "+IntakePathPrefix+"{provider}/{connection}", in.handle)
}

// Available reports whether this deployment receives provider's deliveries.
func (in *Intake) Available(provider string) bool {
	_, ok := in.Sources[provider]
	return ok
}

// Kind is what the customer sets up to send provider's deliveries; "" when there is no intake.
func (in *Intake) Kind(provider string) string {
	if src, ok := in.Sources[provider]; ok {
		return src.Transport.Kind()
	}
	return ""
}

// URL is where a connection's deliveries are sent.
func (in *Intake) URL(provider, connectionID string) string {
	return in.BaseURL + IntakePathPrefix + provider + "/" + connectionID
}

// NewIntakeToken returns a new intake token and the hash the connection stores.
func NewIntakeToken() (token, hash string, err error) {
	b := make([]byte, intakeTokenEntropy)
	if _, err := rand.Read(b); err != nil {
		return "", "", fmt.Errorf("providers: intake token: %w", err)
	}
	token = IntakeTokenPrefix + base64.RawURLEncoding.EncodeToString(b)
	return token, HashIntakeToken(token), nil
}

// HashIntakeToken is the stored form of an intake token.
func HashIntakeToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (in *Intake) handle(w http.ResponseWriter, r *http.Request) {
	provider, connection := r.PathValue("provider"), r.PathValue("connection")
	src, ok := in.Sources[provider]
	if !ok || !store.IsUUID(connection) {
		http.Error(w, "no such intake", http.StatusNotFound)
		return
	}
	t := src.Transport
	ctx := r.Context()
	log := in.Logger.With("provider", provider, "connection", connection)
	token, err := t.Token(ctx, r)
	if err != nil || !strings.HasPrefix(token, IntakeTokenPrefix) {
		log.Warn("provider intake: delivery refused", "reason", "no intake token", "error", err)
		t.Reply(w, r, http.StatusUnauthorized, "the delivery presents no intake token")
		return
	}
	tenant, err := in.Store.IntakeTenant(ctx, provider, connection, HashIntakeToken(token))
	if err != nil {
		log.Error("provider intake: resolve tenant", "error", err)
		t.Reply(w, r, http.StatusInternalServerError, "the delivery could not be recorded")
		return
	}
	if tenant == "" {
		log.Warn("provider intake: delivery refused", "reason", "the token is not the connection's")
		t.Reply(w, r, http.StatusUnauthorized, "the intake token is not this connection's")
		return
	}
	log = log.With("tenant", tenant)
	open, err := in.Store.IntakeOpen(ctx, tenant, connection)
	if err != nil {
		log.Error("provider intake: read connection", "error", err)
		t.Reply(w, r, http.StatusInternalServerError, "the delivery could not be recorded")
		return
	}
	if !open {
		t.Reply(w, r, http.StatusForbidden, "the tenant is not accepting events")
		return
	}
	body, err := readDelivery(w, r)
	if err != nil {
		in.delivered(ctx, log, tenant, connection, Result{Error: ErrDeliveryMalformed})
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) || errors.Is(err, errInflatedTooLarge) {
			t.Reply(w, r, http.StatusRequestEntityTooLarge, "the delivery is over the cap")
			return
		}
		t.Reply(w, r, http.StatusBadRequest, "the delivery body cannot be read")
		return
	}
	entries, err := t.Entries(r, body)
	if err != nil {
		log.Warn("provider intake: delivery is malformed", "error", err)
		in.delivered(ctx, log, tenant, connection, Result{Error: ErrDeliveryMalformed})
		t.Reply(w, r, http.StatusBadRequest, "the delivery is not what this intake receives")
		return
	}
	res, err := in.record(ctx, log, Connection{TenantID: tenant, ConnectionID: connection, Provider: provider}, src.Decode, entries)
	if err != nil {
		log.Error("provider intake: record", "error", err)
		in.delivered(ctx, log, tenant, connection, Result{Read: res.Read, New: res.New, Error: ErrInternal})
		t.Reply(w, r, http.StatusInternalServerError, "the delivery could not be recorded")
		return
	}
	in.delivered(ctx, log, tenant, connection, res)
	log.Info("provider intake: delivery recorded", "entries", len(entries), "read", res.Read, "new", res.New, "skipped", res.Skipped)
	t.Reply(w, r, http.StatusOK, "")
}

// record decodes and records every entry of one delivery. An entry that is not the provider's log
// and a record the recorder passes over are counted and skipped, so one bad entry does not hold
// back the rest of a delivery that would be sent again forever.
func (in *Intake) record(ctx context.Context, log *slog.Logger, c Connection, decode Decoder, entries [][]byte) (Result, error) {
	var res Result
	policy, err := in.Store.Policy(ctx, c.TenantID)
	if err != nil {
		return res, fmt.Errorf("read policy: %w", err)
	}
	refKey, err := in.UserRefKeys.Key(ctx, c.TenantID)
	if err != nil {
		return res, fmt.Errorf("user_ref key: %w", err)
	}
	for _, entry := range entries {
		recs, err := decode(entry)
		if err != nil {
			res.Skipped++
			log.Warn("provider intake: an entry was passed over", "error", err)
		}
		for _, rec := range recs {
			res.Read++
			outcome, err := in.Recorder.Record(ctx, c, policy, refKey, rec)
			if IsSkipped(err) {
				res.Skipped++
				log.Warn("provider intake: a record was passed over", "error", err)
				continue
			}
			if err != nil {
				return res, err
			}
			if outcome == OutcomeInserted {
				res.New++
			}
		}
	}
	return res, nil
}

func (in *Intake) delivered(ctx context.Context, log *slog.Logger, tenant, connection string, res Result) {
	if err := in.Store.Delivered(context.WithoutCancel(ctx), tenant, connection, in.Now().UTC(), res); err != nil {
		log.Error("provider intake: record delivery", "error", err)
	}
}

var errInflatedTooLarge = errors.New("providers: the delivery inflates past the cap")

// readDelivery reads a delivery's body within MaxDeliveryBytes, inflated when it is gzip-encoded.
func readDelivery(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxDeliveryBytes))
	if err != nil {
		return nil, err
	}
	switch strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Encoding"))) {
	case "", "identity":
		return body, nil
	case "gzip":
		return gunzip(body)
	default:
		return nil, errors.New("providers: unsupported content encoding")
	}
}

// gunzip inflates b within maxInflatedBytes.
func gunzip(b []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	out, err := io.ReadAll(io.LimitReader(zr, maxInflatedBytes+1))
	if err != nil {
		return nil, err
	}
	if len(out) > maxInflatedBytes {
		return nil, errInflatedTooLarge
	}
	return out, nil
}

// isGzip reports whether b starts with the gzip magic number.
func isGzip(b []byte) bool { return len(b) > 2 && b[0] == 0x1f && b[1] == 0x8b }
