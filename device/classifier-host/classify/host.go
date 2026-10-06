// Package classify is the classifier host: the classification pipeline and the server that
// answers capture-core over a byte stream.
//
// A request passes a mode check, document parsing in a child process (for documents),
// normalisation, the rules, the validators and the keyword model, and the answer is a label set
// with a confidence band. When a stage cannot complete (its budget ran out, the parser failed,
// the body could not be read) the answer is `degraded`, carries no labels and names the stage,
// so "the classifier could not tell" is never reported as "nothing found".
//
// The request type, protocol.ClassifyRequest, carries no identity: the classifier sees bytes and
// a collection mode, never who sent them or where.
package classify

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/shadow-ai-capture/device/classifier-host/norm"
	"github.com/shadow-ai-capture/device/classifier-host/parser/isolation"
	"github.com/shadow-ai-capture/device/classifier-host/release"
	"github.com/shadow-ai-capture/device/protocol"
)

// Parser parses one document in an isolated child process.
type Parser interface {
	Parse(ctx context.Context, mediaType string, doc []byte) isolation.Result
}

// Options configure a host.
type Options struct {
	// Release is the loaded, verified release. Required.
	Release *release.Release
	// Parser parses documents. With none, a document degrades with parser_failed.
	Parser Parser
	// Budget is the per-stage time budget. Zero means DefaultBudget().
	Budget Budget
	// Limits bound normalisation. Zero means norm.DefaultLimits().
	Limits norm.Limits
	// Now is the clock. Nil means time.Now.
	Now func() time.Time
	// MaxExcerptChars bounds the M2 excerpt. Zero means protocol.MaxExcerptChars.
	MaxExcerptChars int
}

// Host classifies requests. It is safe for concurrent use.
type Host struct {
	opts Options
}

// New returns a host for a loaded release.
func New(opts Options) (*Host, error) {
	if opts.Release == nil {
		return nil, errors.New("classify: a host needs a loaded release")
	}
	if opts.Budget == (Budget{}) {
		opts.Budget = DefaultBudget()
	}
	if opts.Limits == (norm.Limits{}) {
		opts.Limits = norm.DefaultLimits()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.MaxExcerptChars <= 0 {
		opts.MaxExcerptChars = protocol.MaxExcerptChars
	}
	return &Host{opts: opts}, nil
}

// Version is the classifier_version this host reports: the release version.
func (h *Host) Version() string { return h.opts.Release.Version }

// Handshake answers capture-core's connect-time handshake. A protocol or release mismatch is a
// refusal, which capture-core treats as a degraded classifier rather than an error.
func (h *Host) Handshake(req protocol.HandshakeRequest) protocol.HandshakeResponse {
	if req.ProtocolVersion != protocol.Version || req.CoreVersion == "" ||
		req.ClassifierVersion != "" && req.ClassifierVersion != h.Version() {
		return protocol.HandshakeResponse{OK: false, ClassifierVersion: h.Version(), Reason: protocol.DetailVersionMismatch}
	}
	return protocol.HandshakeResponse{OK: true, ClassifierVersion: h.Version()}
}

// Classify answers one request. It never fails: every defect becomes a degraded response that
// names its stage, and every response satisfies protocol.ClassifyResponse.Validate.
func (h *Host) Classify(ctx context.Context, req protocol.ClassifyRequest) protocol.ClassifyResponse {
	p := &pipeline{opts: &h.opts, req: req}
	p.run(ctx)
	return p.response(h.Version())
}

// isDocument reports whether a media type is a document the parser child reads. JSON and XML are
// text: normalisation extracts them in process.
func isDocument(mediaType string) bool {
	mt := strings.ToLower(mediaType)
	if i := strings.IndexByte(mt, ';'); i >= 0 {
		mt = mt[:i]
	}
	switch mt = strings.TrimSpace(mt); mt {
	case "application/pdf", "application/msword", "application/zip", "application/x-zip-compressed",
		"application/gzip", "application/x-gzip",
		"application/vnd.oasis.opendocument.text", "application/vnd.oasis.opendocument.spreadsheet":
		return true
	}
	return strings.HasPrefix(mt, "application/vnd.openxmlformats-officedocument.")
}
