// Package isolation runs the document parser (package parser) in a child process, one process
// per document, and enforces the child's limits from the parent, where the child cannot evade
// them:
//
//   - a wall-clock timeout covering spawn, write, parse and read, after which the child is
//     killed;
//   - a memory limit: on Windows a job object caps the child's commit charge and the parent
//     kills the child when its peak crosses the residency limit; on Linux the parent samples
//     /proc/<pid>/statm and kills on breach; on macOS, which offers neither, the timeout and the
//     child's own decompression and output limits bound it;
//   - an output limit on what the parent reads back;
//   - a size limit checked before any child starts;
//   - bounded concurrency, and a per-format breaker that suspends a format after repeated
//     failures.
//
// Killing a child kills everything it started: the job object on Windows, the process group
// elsewhere.
package isolation

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/shadow-ai-capture/device/classifier-host/parser"
	"github.com/shadow-ai-capture/device/protocol"
)

// Cause is why a parse did not produce usable text, or CauseOK.
type Cause string

const (
	CauseOK          Cause = "ok"
	CauseBusy        Cause = "busy"
	CauseTimeout     Cause = "timeout"
	CauseMemory      Cause = "memory"
	CauseCrash       Cause = "crash"
	CauseMalformed   Cause = "malformed_result"
	CauseOutputCap   Cause = "output_cap"
	CauseInputCap    Cause = "input_cap"
	CauseUndecodable Cause = "undecodable"
	CauseUnsupported Cause = "unsupported_media_type"
	CauseNesting     Cause = "nesting_over_cap"
	CauseSuspended   Cause = "format_suspended"
)

// Detail maps a cause to the protocol's closed detail vocabulary.
func (c Cause) Detail() protocol.Detail {
	switch c {
	case CauseOK:
		return protocol.DetailNone
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
	default:
		return protocol.DetailParserFailed
	}
}

// Result is one document's parse outcome. Text is usable only when Cause is CauseOK.
type Result struct {
	Text     string
	Cause    Cause
	Err      string // a diagnostic; never document content
	Duration time.Duration
	// PeakBytes is the child's peak memory as the parent observed it.
	PeakBytes int64
}

// Command is the child to start for each document.
type Command struct {
	Path string
	Args []string
	Env  []string // KEY=VALUE pairs added to this process's environment
}

// DefaultCommand runs this executable as `parse-child`.
func DefaultCommand() (Command, error) {
	exe, err := os.Executable()
	if err != nil {
		return Command{}, fmt.Errorf("isolation: locating this executable to run the parser child: %w", err)
	}
	return Command{Path: exe, Args: []string{"parse-child"}}, nil
}

// Limits are the parent-enforced limits.
type Limits struct {
	// Timeout bounds one document from spawn to result.
	Timeout time.Duration
	// MemoryBytes is the Windows job object's per-process commit limit; zero sets none.
	MemoryBytes int64
	// ResidencyBytes is the observed memory at which the parent kills the child; zero means
	// 80% of MemoryBytes.
	ResidencyBytes int64
	// ResidencyPoll is how often the child's memory is sampled.
	ResidencyPoll time.Duration
	// OutputBytes bounds what the parent reads from the child.
	OutputBytes int
	// DocumentBytes refuses a larger document before a child starts.
	DocumentBytes int64
	// KillGrace is how long the parent waits for a killed child to exit.
	KillGrace time.Duration
	// MaxConcurrent bounds the children alive at once.
	MaxConcurrent int
	// FormatFailures failures of one media type within FormatWindow suspend it for
	// FormatCooldown.
	FormatFailures int
	FormatWindow   time.Duration
	FormatCooldown time.Duration
}

// DefaultLimits are the limits the classifier host runs with.
func DefaultLimits() Limits {
	return Limits{
		Timeout:        250 * time.Millisecond,
		MemoryBytes:    96 << 20,
		ResidencyPoll:  2 * time.Millisecond,
		OutputBytes:    4 << 20,
		DocumentBytes:  parser.DefaultLimits().MaxDocumentBytes,
		KillGrace:      2 * time.Second,
		MaxConcurrent:  4,
		FormatFailures: 3,
		FormatWindow:   time.Minute,
		FormatCooldown: 5 * time.Minute,
	}
}

// Runner parses documents in child processes. It is safe for concurrent use.
type Runner struct {
	cmd     Command
	lim     Limits
	slots   chan struct{}
	breaker *Breaker
}

// New returns a runner. A zero ResidencyBytes is derived from MemoryBytes: a Go child at its job
// limit stalls rather than exits, so every limit the parent sets is also one it polices.
func New(cmd Command, lim Limits) *Runner {
	if lim.ResidencyBytes == 0 {
		lim.ResidencyBytes = lim.MemoryBytes * 8 / 10
	}
	return &Runner{
		cmd:     cmd,
		lim:     lim,
		slots:   make(chan struct{}, max(lim.MaxConcurrent, 1)),
		breaker: NewBreaker(lim.FormatFailures, lim.FormatWindow, lim.FormatCooldown, time.Now),
	}
}

// Parse runs one document through one child under every limit.
func (r *Runner) Parse(ctx context.Context, mediaType string, doc []byte) Result {
	start := time.Now()
	if int64(len(doc)) > r.lim.DocumentBytes {
		return failure(CauseInputCap, fmt.Sprintf("document is %d bytes, over the parser's %d-byte limit", len(doc), r.lim.DocumentBytes), start)
	}
	if !r.breaker.Allow(mediaType) {
		return failure(CauseSuspended, "parsing "+mediaType+" is suspended after repeated failures", start)
	}
	select {
	case r.slots <- struct{}{}:
		defer func() { <-r.slots }()
	case <-ctx.Done():
		return failure(CauseBusy, "the parse was cancelled while waiting for a parser slot", start)
	}
	res := r.run(ctx, mediaType, doc, start)
	r.breaker.Record(mediaType, res.Cause == CauseOK)
	return res
}

func (r *Runner) run(ctx context.Context, mediaType string, doc []byte, start time.Time) Result {
	lim := r.lim
	payload, err := parser.EncodeRequest(parser.Header{MediaType: mediaType, DeclaredSize: int64(len(doc))}, doc)
	if err != nil {
		return failure(CauseCrash, "encoding the request: "+err.Error(), start)
	}

	cmd := exec.Command(r.cmd.Path, r.cmd.Args...)
	if len(r.cmd.Env) > 0 {
		cmd.Env = append(os.Environ(), r.cmd.Env...)
	}
	setProcessAttributes(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return failure(CauseCrash, "stdin pipe: "+err.Error(), start)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return failure(CauseCrash, "stdout pipe: "+err.Error(), start)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &limitedWriter{w: &stderr, n: 4096}
	if err := cmd.Start(); err != nil {
		return failure(CauseCrash, "starting the parser child: "+err.Error(), start)
	}

	// The child reads nothing until the request is written, so the limits are in force before it
	// sees a byte of the document.
	enf, enfErr := newEnforcer(cmd.Process.Pid, lim.MemoryBytes)
	var notes []string
	if enfErr != nil {
		notes = append(notes, "memory limit not enforced: "+enfErr.Error())
	}

	breach := make(chan int64, 1)
	stopMonitor := make(chan struct{})
	monitorDone := make(chan struct{})
	go func() {
		defer close(monitorDone)
		if enf == nil || lim.ResidencyBytes <= 0 {
			return
		}
		ticker := time.NewTicker(lim.ResidencyPoll)
		defer ticker.Stop()
		for {
			select {
			case <-stopMonitor:
				return
			case <-ticker.C:
				if n, ok := enf.sample(); ok && n > lim.ResidencyBytes {
					breach <- n
					return
				}
			}
		}
	}()

	writeErr := make(chan error, 1)
	go func() {
		err := protocol.WriteFrame(stdin, payload)
		stdin.Close()
		writeErr <- err
	}()

	type output struct {
		b   []byte
		err error
	}
	outCh := make(chan output, 1)
	go func() {
		b, err := io.ReadAll(io.LimitReader(stdout, int64(lim.OutputBytes)+1))
		if err == nil && len(b) > lim.OutputBytes {
			err = errOutputCap
		}
		outCh <- output{b, err}
	}()

	deadline := time.NewTimer(lim.Timeout)
	defer deadline.Stop()
	cause, detail := CauseOK, ""
	var out output
	gotOutput := false
	select {
	case out = <-outCh:
		gotOutput = true
		if errors.Is(out.err, errOutputCap) {
			cause, detail = CauseOutputCap, fmt.Sprintf("the parser child wrote more than the parent's %d-byte limit", lim.OutputBytes)
		}
	case n := <-breach:
		cause, detail = CauseMemory, fmt.Sprintf("parser child reached %d bytes, over the %d-byte residency limit", n, lim.ResidencyBytes)
	case <-deadline.C:
		cause, detail = CauseTimeout, "parser child exceeded the "+lim.Timeout.String()+" limit"
	case <-ctx.Done():
		cause, detail = CauseTimeout, "parse cancelled: "+ctx.Err().Error()
	}
	if cause != CauseOK {
		kill(cmd, enf)
	}
	if !gotOutput {
		select {
		case out = <-outCh:
		case <-time.After(lim.KillGrace):
			notes = append(notes, "the child's output did not close within the kill grace")
		}
	}

	// Wait is called only once the output has been read, as os/exec requires.
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	var deadlineC <-chan time.Time
	if cause == CauseOK {
		deadlineC = deadline.C
	}
	grace := time.NewTimer(lim.KillGrace)
	defer grace.Stop()
	exited := false
	select {
	case err := <-waitCh:
		exited = true
		if cause == CauseOK && err != nil {
			cause, detail = CauseCrash, "parser child exited: "+err.Error()
		}
	case <-deadlineC:
		// The child closed its output but has not exited.
		cause, detail = CauseTimeout, "parser child exceeded the "+lim.Timeout.String()+" limit"
		kill(cmd, enf)
		select {
		case <-waitCh:
			exited = true
		case <-grace.C:
		}
	case <-grace.C:
	}
	close(stopMonitor)
	<-monitorDone

	res := Result{Duration: time.Since(start)}
	if enf != nil {
		res.PeakBytes = enf.peak()
		enf.close()
	}
	if !exited {
		notes = append(notes, "the child did not exit within the kill grace")
	} else if s := strings.TrimSpace(stderr.String()); s != "" {
		notes = append(notes, "stderr: "+s)
	}
	if cause != CauseOK {
		res.Cause, res.Err = cause, join(detail, notes)
		return res
	}
	var parsed parser.Result
	msg := ""
	if out.err != nil {
		msg = "reading the parser result: " + out.err.Error()
	} else if parsed, err = parser.ReadResult(bytes.NewReader(out.b)); err != nil {
		msg = "the parser result is not a result frame: " + err.Error()
	}
	if msg != "" {
		select {
		case werr := <-writeErr:
			if werr != nil {
				msg += "; writing the document: " + werr.Error()
			}
		default:
		}
		res.Cause, res.Err = CauseMalformed, join(msg, notes)
		return res
	}
	res.Cause = statusCause(parsed.Status)
	res.Err = join(parsed.Err, notes)
	if res.Cause == CauseOK {
		res.Text = parsed.Text
	}
	return res
}

var errOutputCap = errors.New("isolation: the parser child's output exceeded the parent's limit")

// statusCause maps the child's status to a cause.
func statusCause(s parser.Status) Cause {
	switch s {
	case parser.StatusOK:
		return CauseOK
	case parser.StatusInputOverCap:
		return CauseInputCap
	case parser.StatusOutputCap:
		return CauseOutputCap
	case parser.StatusDepthExceeded:
		return CauseNesting
	case parser.StatusUndecodable:
		return CauseUndecodable
	case parser.StatusUnsupported:
		return CauseUnsupported
	default:
		return CauseMalformed
	}
}

func failure(c Cause, msg string, start time.Time) Result {
	return Result{Cause: c, Err: msg, Duration: time.Since(start)}
}

func join(detail string, notes []string) string {
	parts := notes
	if detail != "" {
		parts = append([]string{detail}, notes...)
	}
	return strings.Join(parts, "; ")
}

// kill terminates the child and everything it started.
func kill(cmd *exec.Cmd, enf enforcer) {
	if enf != nil {
		_ = enf.kill()
	}
	_ = cmd.Process.Kill()
}

// enforcer is the platform's handle on one child.
type enforcer interface {
	// sample returns the child's memory use as the platform measures it.
	sample() (int64, bool)
	// peak returns the highest memory use the platform observed.
	peak() int64
	// kill terminates the child and anything it started.
	kill() error
	// close releases platform handles without killing.
	close()
}

// limitedWriter keeps the first n bytes written to it and discards the rest.
type limitedWriter struct {
	w io.Writer
	n int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.n > 0 {
		k := min(len(p), l.n)
		l.n -= k
		if _, err := l.w.Write(p[:k]); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// Breaker suspends a media type after repeated parse failures, so a format that keeps failing
// costs one refusal per document instead of one child per document.
type Breaker struct {
	mu          sync.Mutex
	maxFailures int
	window      time.Duration
	cooldown    time.Duration
	now         func() time.Time
	formats     map[string]*formatState
}

type formatState struct {
	failures       []time.Time
	suspendedUntil time.Time
}

// NewBreaker suspends a format for cooldown once maxFailures failures fall within window.
func NewBreaker(maxFailures int, window, cooldown time.Duration, now func() time.Time) *Breaker {
	return &Breaker{maxFailures: max(maxFailures, 1), window: window, cooldown: cooldown, now: now, formats: map[string]*formatState{}}
}

// Allow reports whether format may be parsed now.
func (b *Breaker) Allow(format string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	st := b.formats[format]
	return st == nil || !b.now().Before(st.suspendedUntil)
}

// Record records one outcome. A success clears the format's failures.
func (b *Breaker) Record(format string, ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	st := b.formats[format]
	if st == nil {
		st = &formatState{}
		b.formats[format] = st
	}
	if ok {
		st.failures, st.suspendedUntil = nil, time.Time{}
		return
	}
	now := b.now()
	kept := st.failures[:0]
	for _, t := range st.failures {
		if now.Sub(t) < b.window {
			kept = append(kept, t)
		}
	}
	st.failures = append(kept, now)
	if len(st.failures) >= b.maxFailures {
		st.suspendedUntil = now.Add(b.cooldown)
		st.failures = nil
	}
}
