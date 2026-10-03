// Package docparse is the seam between the classifier host and the §10 parser child.
//
// It exists as its own tiny package for one reason: the host core (package classify) is built
// for native **and** js/wasm (ADR 0016), while the parser child is a process spawn that the wasm
// target cannot do at all (§9.1's table: "Document parsing — Unavailable" in the extension's
// copy). Keeping the interface here — types and protocol only, no os, no exec — lets the host
// compile for both targets and treat "no parser in this target" as the degraded case §9.7
// requires, instead of a build-tag fork of the pipeline.
//
// The implementation is parser/isolation.
package docparse

import (
	"context"
	"time"

	"github.com/shadow-ai-capture/device/protocol"
)

// Cause is the bounded set of reasons a parse did not complete. It is what the §10 failure
// counters are grouped by, and it maps onto the protocol.Detail vocabulary one-to-one.
type Cause string

const (
	CauseNone          Cause = ""
	CauseOK            Cause = "ok"
	CauseUnavailable   Cause = "unavailable"
	CauseDeferred      Cause = "deferred"
	CauseTimeout       Cause = "timeout"
	CauseMemory        Cause = "memory"
	CauseCrash         Cause = "crash"
	CauseMalformed     Cause = "malformed_result"
	CauseOutputCap     Cause = "output_cap"
	CauseInputCap      Cause = "input_cap"
	CauseUndecodable   Cause = "undecodable"
	CauseUnsupported   Cause = "unsupported_media_type"
	CauseDepthExceeded Cause = "nesting_over_cap"
	CauseCircuitOpen   Cause = "format_circuit_open"
)

// Detail maps a cause to the closed protocol vocabulary (§9.7, §10). Every non-OK cause maps to a
// detail that is in protocol.AllDetails, so a health report carrying it validates — and a cause
// that is not a failure maps to DetailNone, so a successful stage cannot carry a failure detail
// into a response that a report would then group by.
func (c Cause) Detail() protocol.Detail {
	switch c {
	case CauseNone, CauseOK:
		return protocol.DetailNone
	case CauseDeferred:
		return protocol.DetailParserFailed
	case CauseTimeout:
		return protocol.DetailParserTimeout
	case CauseMemory:
		return protocol.DetailParserMemory
	case CauseCrash, CauseMalformed:
		return protocol.DetailParserCrash
	case CauseOutputCap:
		return protocol.DetailParserOutputCap
	case CauseInputCap:
		return protocol.DetailContentOverCap
	case CauseUndecodable:
		return protocol.DetailUndecodableContent
	case CauseUnsupported, CauseDepthExceeded, CauseCircuitOpen, CauseUnavailable:
		return protocol.DetailParserFailed
	default:
		return protocol.DetailParserFailed
	}
}

// Result is one document's parse outcome, in the shape the pipeline records as a stage result.
type Result struct {
	// Text is the extracted text. It is usable only when Cause is CauseOK (possibly with
	// Truncated set, which the caller must degrade per §10's output cap).
	Text string

	// Truncated is true when the output cap cut the result short.
	Truncated bool

	// Cause is why the parse did not complete, or CauseOK.
	Cause Cause

	// Err is a short diagnostic for the stage result. It never carries document content.
	Err string

	// Detail is the protocol detail for the stage result.
	Detail protocol.Detail

	// Duration is the parent-side wall clock of the parse.
	Duration time.Duration

	// ExitCode and PeakBytes are diagnostics: the child's exit code and the parent's observed
	// peak commit charge. They are recorded so a breach is visible rather than inferred.
	ExitCode  int
	PeakBytes int64
}

// Parser is the §10 child as the host sees it. Implementations must enforce the memory cap,
// wall-clock timeout, hard kill and output cap **in the parent** (§10: "a limit a child
// enforces on itself is not a limit").
type Parser interface {
	// Available reports whether this target can parse documents at all. The wasm copy cannot.
	Available() bool

	// Parse runs exactly one document through exactly one child and reaps it.
	Parse(ctx context.Context, mediaType string, doc []byte) Result

	// AllowFormat reports whether the per-format circuit breaker currently permits a parse.
	AllowFormat(mediaType string) bool

	// RecordFormat records a parse outcome for the breaker and the per-format coverage count.
	RecordFormat(mediaType string, ok bool)
}
