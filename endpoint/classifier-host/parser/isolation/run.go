//go:build !js

// Package isolation is the parent side of docs/01-collectors.md §10: it spawns one parser child
// per document and enforces the limits that §10 insists must be enforced by the parent, "because
// a limit a child enforces on itself is not a limit".
//
//   - **Memory cap.** On Windows the child is assigned to a job object with
//     JOB_OBJECT_LIMIT_PROCESS_MEMORY immediately after CreateProcess, and the parent additionally
//     samples the child's commit charge and kills it on breach. On Linux the parent samples
//     /proc/<pid>/statm. On platforms with neither (macOS) sampling is unavailable, and that is
//     reported rather than hidden: the timeout and hard kill still bound the child, and the
//     missing residency monitor is an open decision in README.md.
//   - **Wall-clock timeout and hard kill.** One deadline covers spawn, write, read and wait. On
//     expiry the child is terminated — the job on Windows, the process group on Unix — and the
//     result is `degraded` with `parser_timeout`.
//
// The rest of §10's table lives here too: one child per document (no reuse), a bounded output
// read, a declared-size cap checked *before* a spawn, bounded fan-out, and a per-format circuit
// breaker so a repeatedly failing format becomes a coverage gap instead of a CPU burn.
//
// Nothing here exists on js/wasm — there is no process spawn in that target — which is why §9.1's
// table says document parsing is unavailable in the extension's copy. The build tag makes that a
// compile-time fact rather than a runtime hope.
package isolation

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/shadow-ai-capture/device/classifier-host/docparse"
	"github.com/shadow-ai-capture/device/classifier-host/parser"
	"github.com/shadow-ai-capture/device/protocol"
)

// enforcer is the platform's handle on one child: a job object on Windows, a /proc reader on
// Linux, and a timeout-only stub where neither exists. newEnforcer is implemented per platform.
//
// A non-nil error means the parent-enforced memory cap is *not* active; the enforcer is still
// returned when it can kill and sample, because a degraded cap must not disable the kill.
type enforcer interface {
	// sample returns the child's current commit charge or resident set.
	sample() (int64, bool)
	// peak returns the high-water mark the platform can report (job peak, or the last sample).
	peak() int64
	// kill terminates the child and anything it spawned.
	kill() error
	// close releases platform handles. It must not kill.
	close()
}

// Command is the parser child to spawn. Path is normally the classifier-host binary itself and
// Args is ["parse-child"]; tests use the test binary with a helper entry point, so the same child
// code runs under the same enforcement.
type Command struct {
	Path string
	Args []string
	Env  []string // KEY=VALUE additions to the parent environment
}

// String is for diagnostics. It never contains document content.
func (c Command) String() string {
	return c.Path + " " + strings.Join(c.Args, " ")
}

// Limits are the parent-enforced bounds of §10, plus the fan-out bound of its Concurrency row.
type Limits struct {
	// Timeout is the wall clock for one document, covering spawn, write, parse and read.
	// §10: "well inside the interactive budget and generous enough for legitimate large
	// documents" — the value is A12's parameter.
	Timeout time.Duration

	// MemoryCapBytes is the job object's per-process memory limit. Zero disables the job cap.
	MemoryCapBytes int64

	// ResidencyCapBytes is the sampled commit/RSS threshold at which the parent kills the
	// child. Zero disables sampling.
	ResidencyCapBytes int64

	// ResidencyPoll is how often the child's memory is sampled.
	ResidencyPoll time.Duration

	// OutputCapBytes bounds what the parent reads from the child's stdout. A child that writes
	// more is killed and reported, rather than read into the host's memory.
	OutputCapBytes int

	// DeclaredCapBytes refuses a document before a child is spawned at all.
	DeclaredCapBytes int64

	// KillGrace is how long the parent waits for a killed child to be reaped before releasing
	// its handles and moving on.
	KillGrace time.Duration

	// MaxConcurrent bounds fan-out: "many attachments cannot become a memory-exhaustion event".
	MaxConcurrent int

	// Child is the child's own document caps. The child re-checks them, because defence in
	// depth on the hostile path is cheap.
	Child parser.Limits

	// MaxFormatFailures and FormatCooldown configure the per-format circuit breaker.
	MaxFormatFailures int
	FormatCooldown    time.Duration
	FormatWindow      time.Duration
}

// DefaultLimits is the shipped configuration. The numbers are A12 parameters: a memory cap in
// the tens of megabytes happens to be the job limit, a timeout in the low hundreds of
// milliseconds, an output cap in the low megabytes.
func DefaultLimits() Limits {
	return Limits{
		Timeout:           250 * time.Millisecond,
		MemoryCapBytes:    96 << 20,
		ResidencyCapBytes: 64 << 20,
		ResidencyPoll:     2 * time.Millisecond,
		OutputCapBytes:    4 << 20,
		DeclaredCapBytes:  32 << 20,
		KillGrace:         2 * time.Second,
		MaxConcurrent:     4,
		Child:             parser.DefaultLimits(),
		MaxFormatFailures: 3,
		FormatCooldown:    5 * time.Minute,
		FormatWindow:      time.Minute,
	}
}

func (l Limits) withDefaults() Limits {
	d := DefaultLimits()
	if l.Timeout <= 0 {
		l.Timeout = d.Timeout
	}
	if l.ResidencyPoll <= 0 {
		l.ResidencyPoll = d.ResidencyPoll
	}
	if l.OutputCapBytes <= 0 {
		l.OutputCapBytes = d.OutputCapBytes
	}
	if l.DeclaredCapBytes <= 0 {
		l.DeclaredCapBytes = d.DeclaredCapBytes
	}
	if l.KillGrace <= 0 {
		l.KillGrace = d.KillGrace
	}
	if l.MaxConcurrent <= 0 {
		l.MaxConcurrent = d.MaxConcurrent
	}
	if l.MaxFormatFailures <= 0 {
		l.MaxFormatFailures = d.MaxFormatFailures
	}
	if l.FormatCooldown <= 0 {
		l.FormatCooldown = d.FormatCooldown
	}
	if l.FormatWindow <= 0 {
		l.FormatWindow = d.FormatWindow
	}
	if l.ResidencyCapBytes == 0 && l.MemoryCapBytes > 0 {
		// §10's requirement is that the *parent* enforces the cap it sets. A job object alone
		// makes the child's allocations fail, and a Go runtime answers an allocation failure by
		// thrashing before it exits — measured here: a child under a 192 MB job cap sat at exactly
		// the cap for 15 s without dying. So a job limit that has no explicit sampler gets one
		// derived from it, and every cap the parent sets is a cap the parent polices.
		l.ResidencyCapBytes = l.MemoryCapBytes * 8 / 10
	}
	return l
}

// Runner spawns parser children. It is safe for concurrent use.
type Runner struct {
	cmd     Command
	limits  Limits
	breaker *Breaker
	sem     *Semaphore
}

// DefaultCommand returns the parser child command for this executable.
func DefaultCommand() Command {
	exe, err := os.Executable()
	if err != nil {
		return Command{}
	}
	return Command{Path: exe, Args: []string{"parse-child"}}
}

// New builds a runner.
func New(cmd Command, lim Limits) *Runner {
	lim = lim.withDefaults()
	return &Runner{
		cmd:     cmd,
		limits:  lim,
		breaker: NewBreaker(lim.MaxFormatFailures, lim.FormatWindow, lim.FormatCooldown, time.Now),
		sem:     NewSemaphore(lim.MaxConcurrent),
	}
}

// Limits returns the effective limits.
func (r *Runner) Limits() Limits { return r.limits }

// Available reports whether this runner can parse. A runner with no command cannot: that is the
// wasm target's case, and it must degrade rather than pretend.
func (r *Runner) Available() bool { return r != nil && r.cmd.Path != "" }

// AllowFormat implements docparse.Parser.
func (r *Runner) AllowFormat(mediaType string) bool { return r.breaker.Allow(mediaType) }

// RecordFormat implements docparse.Parser.
func (r *Runner) RecordFormat(mediaType string, ok bool) { r.breaker.Record(mediaType, ok) }

// Breaker exposes the per-format breaker for the health channel.
func (r *Runner) Breaker() *Breaker { return r.breaker }

// Parse runs one document through one child, under every parent-enforced limit.
func (r *Runner) Parse(ctx context.Context, mediaType string, doc []byte) docparse.Result {
	lim := r.limits
	if !r.Available() {
		return docparse.Result{Cause: docparse.CauseUnavailable, Detail: parserStatusDetail(docparse.CauseUnavailable),
			Err: "no parser child is configured for this target"}
	}
	if int64(len(doc)) > lim.DeclaredCapBytes {
		return docparse.Result{Cause: docparse.CauseInputCap, Detail: parserStatusDetail(docparse.CauseInputCap),
			Err: fmt.Sprintf("document is %d bytes, over the parser's %d-byte cap", len(doc), lim.DeclaredCapBytes)}
	}
	if !r.sem.Acquire(ctx) {
		return docparse.Result{Cause: docparse.CauseUnavailable, Detail: parserStatusDetail(docparse.CauseUnavailable),
			Err: "the parser's bounded fan-out is saturated"}
	}
	defer r.sem.Release()

	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, lim.Timeout)
	defer cancel()

	cmd := exec.Command(r.cmd.Path, r.cmd.Args...)
	if len(r.cmd.Env) > 0 {
		cmd.Env = append(os.Environ(), r.cmd.Env...)
	}
	applySysProcAttr(cmd)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return failure(docparse.CauseCrash, "stdin pipe: "+err.Error(), start)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return failure(docparse.CauseCrash, "stdout pipe: "+err.Error(), start)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return failure(docparse.CauseCrash, "stderr pipe: "+err.Error(), start)
	}
	if err := cmd.Start(); err != nil {
		return failure(docparse.CauseCrash, "spawning the parser child: "+err.Error(), start)
	}

	// The job object is created and the child assigned before anything is written to it. A
	// failure to assign is reported, not ignored: an unenforced cap must not look enforced.
	enf, enfErr := newEnforcer(cmd.Process.Pid, lim.MemoryCapBytes, lim.ResidencyCapBytes)
	capEnforced := enfErr == nil

	breach := make(chan int64, 1)
	stopMonitor := make(chan struct{})
	monitorDone := make(chan struct{})
	go func() {
		defer close(monitorDone)
		if enf == nil || lim.ResidencyCapBytes <= 0 {
			return
		}
		ticker := time.NewTicker(lim.ResidencyPoll)
		defer ticker.Stop()
		for {
			select {
			case <-stopMonitor:
				return
			case <-ticker.C:
				if n, ok := enf.sample(); ok && n > lim.ResidencyCapBytes {
					select {
					case breach <- n:
					default:
					}
					return
				}
			}
		}
	}()

	writeErr := make(chan error, 1)
	go func() {
		payload, encErr := parser.EncodeRequest(parser.Header{
			MediaType: mediaType, DeclaredSize: int64(len(doc)),
		}, doc)
		if encErr != nil {
			writeErr <- encErr
			return
		}
		// protocol.WriteFrame is the one framing implementation; the child reads it back with
		// protocol.ReadFrameChecked, so the two cannot disagree about the header.
		err := protocol.WriteFrame(stdin, payload)
		_ = stdin.Close()
		writeErr <- err
	}()

	type outResult struct {
		b   []byte
		err error
	}
	outCh := make(chan outResult, 1)
	outCapBreach := make(chan struct{}, 1)
	go func() {
		b, err := readBounded(stdout, lim.OutputCapBytes)
		if errors.Is(err, errOutputCap) {
			select {
			case outCapBreach <- struct{}{}:
			default:
			}
		}
		outCh <- outResult{b, err}
	}()
	errCh := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(io.LimitReader(stderr, 4096))
		errCh <- strings.TrimSpace(string(b))
	}()

	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	var (
		killed bool
		cause  docparse.Cause
		detail string
	)
	timer := time.NewTimer(lim.Timeout)
	defer timer.Stop()

	select {
	case err := <-waitCh:
		if err != nil && !isExitZero(err) {
			cause = docparse.CauseCrash
			detail = "parser child exited: " + err.Error()
		}
	case n := <-breach:
		cause = docparse.CauseMemory
		detail = fmt.Sprintf("parser child reached %d bytes of commit charge, over the %d-byte residency cap", n, lim.ResidencyCapBytes)
		killed = true
		killChild(cmd, enf)
	case <-outCapBreach:
		// The child is trying to hand back more than the parent will read. Killing it is what
		// unblocks the write it is stuck in: without this the parent would wait out its own
		// timeout and report a timeout instead of §10's output cap.
		cause = docparse.CauseOutputCap
		detail = "the parser child wrote more than the parent's " + strconv.Itoa(lim.OutputCapBytes) + "-byte output cap"
		killed = true
		killChild(cmd, enf)
	case <-timer.C:
		cause = docparse.CauseTimeout
		detail = "parser child exceeded the " + lim.Timeout.String() + " wall-clock limit"
		killed = true
		killChild(cmd, enf)
	case <-ctx.Done():
		cause = docparse.CauseTimeout
		detail = "parser child cancelled: " + ctx.Err().Error()
		killed = true
		killChild(cmd, enf)
	}
	if killed {
		select {
		case <-waitCh:
		case <-time.After(lim.KillGrace):
			detail += "; the child did not reap within the kill grace"
		}
	}
	close(stopMonitor)
	<-monitorDone

	peak := int64(0)
	if enf != nil {
		peak = enf.peak()
		enf.close()
	}
	if !capEnforced {
		detail = joinDetail(detail, "the parent-enforced memory cap is not active: "+enfErr.Error())
	}

	res := docparse.Result{
		Duration:  time.Since(start),
		PeakBytes: peak,
		Detail:    parserStatusDetail(cause),
		Err:       detail,
	}
	if cause != docparse.CauseNone && cause != docparse.CauseOK {
		res.Cause = cause
		return res
	}
	if werr := <-writeErr; werr != nil {
		return failure(docparse.CauseMalformed, "writing the document to the parser child: "+werr.Error(), start)
	}
	out := <-outCh
	if out.err != nil {
		if errors.Is(out.err, errOutputCap) {
			// §10's output cap, enforced on the read side: the child produced more than the parent
			// will accept, so the result is a bounded failure rather than an allocation in the
			// host that must stay trusted to say `degraded` honestly.
			return failure(docparse.CauseOutputCap,
				"the parser child wrote more than the parent's "+strconv.Itoa(lim.OutputCapBytes)+"-byte output cap", start)
		}
		return failure(docparse.CauseMalformed, "reading the parser result: "+out.err.Error(), start)
	}
	parsed, err := parser.ReadResult(bytes.NewReader(out.b))
	if err != nil {
		return failure(docparse.CauseMalformed, "parser result is not a valid result frame: "+err.Error(), start)
	}
	if stderrText := <-errCh; stderrText != "" {
		parsed.Err = joinDetail(parsed.Err, "stderr: "+stderrText)
	}
	res = docparse.Result{
		Text:      parsed.Text,
		Truncated: parsed.Truncated,
		Duration:  time.Since(start),
		PeakBytes: peak,
		Detail:    parserStatusDetail(statusToCause(parsed.Status)),
		Err:       joinDetail(detail, parsed.Err),
		Cause:     statusToCause(parsed.Status),
	}
	return res
}

func frameBytes(payload []byte) []byte {
	var buf bytes.Buffer
	// protocol.WriteFrame is the one framing implementation; the child reads it back with
	// ReadFrameChecked, so parent and child cannot disagree about the header.
	if err := protocol.WriteFrame(&buf, payload); err != nil {
		return nil
	}
	return buf.Bytes()
}

// statusToCause maps the child's status code to the bounded cause set.
func statusToCause(s parser.Status) docparse.Cause {
	switch s {
	case parser.StatusOK:
		return docparse.CauseOK
	case parser.StatusInputOverCap:
		return docparse.CauseInputCap
	case parser.StatusOutputCap:
		return docparse.CauseOutputCap
	case parser.StatusDepthExceeded:
		return docparse.CauseDepthExceeded
	case parser.StatusUndecodable:
		return docparse.CauseUndecodable
	case parser.StatusUnsupported:
		return docparse.CauseUnsupported
	case parser.StatusInputShort, parser.StatusMalformed:
		return docparse.CauseMalformed
	default:
		return docparse.CauseCrash
	}
}

func parserStatusDetail(c docparse.Cause) protocol.Detail {
	return c.Detail()
}

func failure(c docparse.Cause, msg string, start time.Time) docparse.Result {
	return docparse.Result{Cause: c, Detail: c.Detail(), Err: msg, Duration: time.Since(start)}
}

func joinDetail(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	return a + "; " + b
}

func isExitZero(err error) bool {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode() == 0
	}
	return false
}

// readBounded reads at most max bytes and reports when the child tried to write more.
func readBounded(r io.Reader, max int) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, int64(max)+1))
	if err != nil {
		return b, err
	}
	if len(b) > max {
		return b[:max], errOutputCap
	}
	return b, nil
}

// killChild terminates the child by the strongest mechanism the platform has, then removes the
// process object so nothing is left to reap.
func killChild(cmd *exec.Cmd, enf enforcer) {
	if enf != nil {
		_ = enf.kill()
	}
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

// sortFormats orders the per-format rows so a health report is stable.
func sortFormats(rows []FormatStats) {
	sort.Slice(rows, func(i, j int) bool { return rows[i].Format < rows[j].Format })
}

var errOutputCap = errors.New("isolation: parser child output exceeded the parent's output cap")

// Semaphore is a counting semaphore with context cancellation, used for §10's bounded fan-out.
type Semaphore struct {
	ch chan struct{}
}

// NewSemaphore returns a semaphore permitting n concurrent holders.
func NewSemaphore(n int) *Semaphore {
	if n <= 0 {
		n = 1
	}
	return &Semaphore{ch: make(chan struct{}, n)}
}

// Acquire takes a slot, waiting until one frees or ctx is done.
func (s *Semaphore) Acquire(ctx context.Context) bool {
	select {
	case s.ch <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

// Release returns a slot.
func (s *Semaphore) Release() {
	select {
	case <-s.ch:
	default:
	}
}

// Breaker is §10's per-format failure count: "disables parses for a repeatedly failing format
// for a period, records a coverage gap per format, and leaves the user's submission unaffected".
type Breaker struct {
	mu          sync.Mutex
	maxFailures int
	window      time.Duration
	cooldown    time.Duration
	now         func() time.Time
	state       map[string]*formatState
}

type formatState struct {
	failures   []time.Time
	openUntil  time.Time
	total      uint64
	failed     uint64
	lastReason string
}

// NewBreaker builds a breaker: maxFailures inside window disables the format for cooldown.
func NewBreaker(maxFailures int, window, cooldown time.Duration, now func() time.Time) *Breaker {
	if now == nil {
		now = time.Now
	}
	if maxFailures <= 0 {
		maxFailures = 3
	}
	return &Breaker{maxFailures: maxFailures, window: window, cooldown: cooldown, now: now, state: map[string]*formatState{}}
}

// Allow reports whether this format may be parsed now.
func (b *Breaker) Allow(format string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	st := b.state[format]
	if st == nil || st.openUntil.IsZero() {
		return true
	}
	return !b.now().Before(st.openUntil)
}

// Record records one parse outcome. A success closes the breaker and clears the window.
func (b *Breaker) Record(format string, ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	st := b.state[format]
	if st == nil {
		st = &formatState{}
		b.state[format] = st
	}
	st.total++
	now := b.now()
	if ok {
		st.failures = nil
		st.openUntil = time.Time{}
		return
	}
	st.failed++
	st.failures = append(st.failures, now)
	cut := now.Add(-b.window)
	kept := st.failures[:0]
	for _, t := range st.failures {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	st.failures = kept
	if len(st.failures) >= b.maxFailures {
		st.openUntil = now.Add(b.cooldown)
	}
}

// FormatStats is the per-format coverage row of §15.3's "records a coverage gap per format".
type FormatStats struct {
	Format        string
	Total         uint64
	Failed        uint64
	DisabledUntil time.Time
}

// Stats returns the per-format counters, sorted by format.
func (b *Breaker) Stats() []FormatStats {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]FormatStats, 0, len(b.state))
	for f, st := range b.state {
		out = append(out, FormatStats{Format: f, Total: st.total, Failed: st.failed, DisabledUntil: st.openUntil})
	}
	sortFormats(out)
	return out
}
