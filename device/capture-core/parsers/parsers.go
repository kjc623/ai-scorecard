// Package parsers turns an intercepted request body into the text a person or a tool sent: one
// parser per target app or API, chosen by host and path, with a generic fallback for every other
// destination.
//
// A parser that recognises its destination but not the body's shape says so with
// ErrUnknownShape instead of guessing, so a vendor's format change shows up as a degraded record
// and a health detail rather than as silently wrong text.
package parsers

import (
	"errors"
	"fmt"
	"strings"

	"github.com/shadow-ai-capture/device/capture-core/dedup"
)

// ErrUnknownShape is returned by a parser whose host and path match a body in none of its known
// shapes.
var ErrUnknownShape = errors.New("parsers: the body is in no shape this parser knows")

// ErrNoText is returned for a body that carries no text to read: a known shape whose turn holds
// only images or files, or a body the generic parser cannot interpret.
var ErrNoText = errors.New("parsers: no user-authored segment identifiable in this body")

// Result is what a parser read from one request body. Requests only: a response is never parsed.
type Result struct {
	Text        string
	Attachments []dedup.Attachment
	// Shape names the known body shape the parser recognised.
	Shape string
}

// Parser reads the bodies sent to one target.
type Parser interface {
	// Match reports whether a request to host (lower case, without a port) and path is this
	// parser's.
	Match(host, path string) bool
	Parse(body []byte, mediaType string) (Result, error)
	// Version identifies the set of shapes the parser knows; it changes whenever they change.
	Version() string
	// BlockResponse is the answer to a request a rule blocks, in the error shape the target's
	// clients display: the HTTP status, the Content-Type and the body. The body carries the
	// message, followed by link when there is one.
	BlockResponse(message, link string) (status int, contentType string, body []byte)
}

// BlockText is the text a block response shows: the rule's message, then its link.
func BlockText(message, link string) string {
	if link == "" {
		return message
	}
	if message == "" {
		return link
	}
	return message + " " + link
}

// UnknownShape returns ErrUnknownShape with a reason. The reason names structure only (a field, a
// position, an undocumented type), never a value from the body, because the error may be logged.
func UnknownShape(target, reason string) error {
	return fmt.Errorf("%w: %s: %s", ErrUnknownShape, target, reason)
}

// Registry chooses a parser by host and path. A request no parser matches, and a request whose
// parser panicked, is read by the generic parser.
type Registry struct {
	parsers  []Parser
	fallback Parser
}

// NewRegistry returns a registry of parsers, consulted in order, with the generic parser as the
// fallback.
func NewRegistry(parsers ...Parser) *Registry {
	return &Registry{parsers: parsers, fallback: Generic{}}
}

// Lookup returns the parser for a request to host and path.
func (r *Registry) Lookup(host, path string) Parser {
	host = normaliseHost(host)
	for _, p := range r.parsers {
		if p.Match(host, path) {
			return p
		}
	}
	return r.fallback
}

// For returns the extraction of one request to host and path, the form in which a route hands
// the registry to the pipeline as its extractor.
func (r *Registry) For(host, path string) *Extraction {
	return &Extraction{registry: r, host: host, path: path}
}

// Extraction is one request's parse through a registry. After the pipeline has called Extract,
// the route reads what happened from it: a parser that panicked is a defect to count, and an
// unknown shape is a health detail.
type Extraction struct {
	registry     *Registry
	host, path   string
	panicked     bool
	unknownShape bool
}

// Extract implements the pipeline's extractor.
func (e *Extraction) Extract(body []byte, mediaType string) (string, []dedup.Attachment, error) {
	p := e.registry.Lookup(e.host, e.path)
	res, panicked, err := parseIsolated(p, body, mediaType)
	if panicked {
		e.panicked = true
		if p == e.registry.fallback {
			return "", nil, ErrNoText
		}
		res, panicked, err = parseIsolated(e.registry.fallback, body, mediaType)
		if panicked {
			return "", nil, ErrNoText
		}
	}
	if errors.Is(err, ErrUnknownShape) {
		e.unknownShape = true
	}
	if err != nil {
		return "", nil, err
	}
	return res.Text, res.Attachments, nil
}

// Panicked reports whether a parser panicked during Extract.
func (e *Extraction) Panicked() bool { return e.panicked }

// UnknownShape reports whether Extract returned ErrUnknownShape.
func (e *Extraction) UnknownShape() bool { return e.unknownShape }

// parseIsolated runs one parser so that its panic stays its own.
func parseIsolated(p Parser, body []byte, mediaType string) (res Result, panicked bool, err error) {
	defer func() {
		if recover() != nil {
			res, panicked, err = Result{}, true, nil
		}
	}()
	res, err = p.Parse(body, mediaType)
	return res, false, err
}

// normaliseHost lower-cases a Host header value and drops its port and a trailing dot.
func normaliseHost(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	if strings.HasPrefix(host, "[") {
		if i := strings.IndexByte(host, ']'); i > 0 {
			return host[1:i]
		}
	}
	if i := strings.LastIndexByte(host, ':'); i >= 0 && strings.Count(host, ":") == 1 {
		host = host[:i]
	}
	return strings.TrimSuffix(host, ".")
}
