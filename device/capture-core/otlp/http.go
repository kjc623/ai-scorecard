package otlp

import (
	"compress/gzip"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/shadow-ai-capture/device/protocol"
)

// MaxBody is the largest request body accepted, before and after decompression.
const MaxBody = 4 << 20

const (
	contentProtobuf = "application/x-protobuf"
	contentJSON     = "application/json"
)

var errTooLarge = errors.New("request body over the limit")

func (r *Receiver) httpHandler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/v1/logs", r.export("logs", func(req *http.Request, from Sender, body []byte, json bool) (proto.Message, error) {
		m := &collogspb.ExportLogsServiceRequest{}
		if err := unmarshal(body, json, m); err != nil {
			return nil, err
		}
		if json {
			fixLogIDs(m)
		}
		r.routeLogs(req.Context(), from, m)
		return &collogspb.ExportLogsServiceResponse{}, nil
	}))
	mux.Handle("/v1/traces", r.export("traces", func(req *http.Request, from Sender, body []byte, json bool) (proto.Message, error) {
		m := &coltracepb.ExportTraceServiceRequest{}
		if err := unmarshal(body, json, m); err != nil {
			return nil, err
		}
		if json {
			fixSpanIDs(m)
		}
		r.routeSpans(req.Context(), from, m)
		return &coltracepb.ExportTraceServiceResponse{}, nil
	}))
	mux.Handle("/v1/metrics", r.export("metrics", func(_ *http.Request, _ Sender, body []byte, json bool) (proto.Message, error) {
		if err := unmarshal(body, json, &colmetricspb.ExportMetricsServiceRequest{}); err != nil {
			return nil, err
		}
		r.counters.Add(protocol.CounterSkippedNotGenerative)
		return &colmetricspb.ExportMetricsServiceResponse{}, nil
	}))
	return mux
}

func unmarshal(body []byte, json bool, m proto.Message) error {
	if json {
		return protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal(body, m)
	}
	return proto.Unmarshal(body, m)
}

type exportFunc func(req *http.Request, from Sender, body []byte, json bool) (proto.Message, error)

// export authenticates the request before its body is read, resolves the connection's sender, then
// reads, decodes and routes the request and answers in its encoding.
func (r *Receiver) export(signal string, handle exportFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mt, _, _ := mime.ParseMediaType(req.Header.Get("Content-Type"))
		json := mt == contentJSON
		if req.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			r.reject(w, signal, req.ContentLength, http.StatusMethodNotAllowed, codes.Unimplemented, json, "method not allowed")
			return
		}
		if !authorized(req.Header.Get("Authorization"), r.token) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			r.reject(w, signal, req.ContentLength, http.StatusUnauthorized, codes.Unauthenticated, json, "missing or invalid bearer token")
			return
		}
		from := r.senderOf(req.Context())
		if mt != contentProtobuf && mt != contentJSON {
			r.reject(w, signal, req.ContentLength, http.StatusUnsupportedMediaType, codes.InvalidArgument, json, "content type must be application/x-protobuf or application/json")
			return
		}
		if req.ContentLength > MaxBody {
			r.reject(w, signal, req.ContentLength, http.StatusRequestEntityTooLarge, codes.ResourceExhausted, json, "request body over 4 MiB")
			return
		}
		body, err := readBody(w, req)
		switch {
		case errors.Is(err, errTooLarge):
			r.reject(w, signal, req.ContentLength, http.StatusRequestEntityTooLarge, codes.ResourceExhausted, json, "request body over 4 MiB")
			return
		case err != nil:
			r.reject(w, signal, req.ContentLength, http.StatusBadRequest, codes.InvalidArgument, json, "request body could not be read")
			return
		}
		resp, err := handle(req, from, body, json)
		if err != nil {
			r.reject(w, signal, int64(len(body)), http.StatusBadRequest, codes.InvalidArgument, json, "request body is not a valid OTLP export request")
			return
		}
		r.succeeded()
		writeMessage(w, http.StatusOK, json, resp)
	})
}

// readBody reads the body, decompressing gzip, and refuses more than MaxBody bytes on either side
// of the decompression.
func readBody(w http.ResponseWriter, req *http.Request) ([]byte, error) {
	var src io.Reader = http.MaxBytesReader(w, req.Body, MaxBody)
	switch enc := strings.ToLower(strings.TrimSpace(req.Header.Get("Content-Encoding"))); enc {
	case "", "identity":
	case "gzip":
		zr, err := gzip.NewReader(src)
		if err != nil {
			return nil, tooLarge(err)
		}
		defer zr.Close()
		src = zr
	default:
		return nil, errors.New("unsupported content encoding")
	}
	body, err := io.ReadAll(io.LimitReader(src, MaxBody+1))
	if err != nil {
		return nil, tooLarge(err)
	}
	if len(body) > MaxBody {
		return nil, errTooLarge
	}
	return body, nil
}

func tooLarge(err error) error {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		return errTooLarge
	}
	return err
}

// reject answers a failed request with an OTLP Status message and logs the endpoint, size and
// outcome only.
func (r *Receiver) reject(w http.ResponseWriter, signal string, size int64, code int, grpcCode codes.Code, json bool, msg string) {
	r.counters.Add(protocol.CounterErrors)
	r.cfg.Log.Printf("otlp: http /v1/%s request of %d bytes refused: %d %s", signal, size, code, msg)
	writeMessage(w, code, json, status.New(grpcCode, msg).Proto())
}

func writeMessage(w http.ResponseWriter, code int, json bool, m proto.Message) {
	var (
		body []byte
		err  error
	)
	if json {
		w.Header().Set("Content-Type", contentJSON)
		body, err = protojson.Marshal(m)
	} else {
		w.Header().Set("Content-Type", contentProtobuf)
		body, err = proto.Marshal(m)
	}
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.WriteHeader(code)
	_, _ = w.Write(body)
}
