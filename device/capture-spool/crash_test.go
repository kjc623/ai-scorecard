package spool

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/protocol"
)

// The crash tests run a *real child process*, let it get part-way through a write, and kill
// it. The child is this test binary re-executed with an environment variable set, which is
// what lets it install the write interposition below: a signal cannot be delivered from
// outside a process at a chosen instruction, but a process can be told to stop there and
// then be killed.
//
// On Windows Process.Kill is TerminateProcess, the platform's uncatchable kill: no deferred
// function runs, no buffer is flushed. On POSIX it is SIGKILL. Either way the process dies
// with bytes already handed to the kernel still on disk, and bytes it was in the middle of
// writing left as a torn frame. That is the failure this test exists to reproduce.
const (
	childEnvSpool   = "CAPTURE_SPOOL_CHILD_DIR"
	childEnvKey     = "CAPTURE_SPOOL_CHILD_KEY"
	childEnvMode    = "CAPTURE_SPOOL_CHILD_MODE"
	childEnvMarker  = "CAPTURE_SPOOL_CHILD_MARKER"
	childEnvPortion = "CAPTURE_SPOOL_CHILD_PORTION"
)

const (
	modeHold       = "hold"
	modeMidWrite   = "midwrite"
	modeInFlight   = "inflight"
	modeDropCommit = "dropcommit"
	modeFoldCrash  = "foldcrash"
)

func TestMain(m *testing.M) {
	if dir := os.Getenv(childEnvSpool); dir != "" {
		os.Exit(crashChild(dir))
	}
	os.Exit(m.Run())
}

// crashChild is the child half of the crash test. It never returns: the parent kills it.
func crashChild(spoolDir string) int {
	fail := func(format string, args ...any) int {
		fmt.Fprintf(os.Stderr, "crash child: "+format+"\n", args...)
		return 2
	}
	keyPath := os.Getenv(childEnvKey)
	marker := os.Getenv(childEnvMarker)
	mode := os.Getenv(childEnvMode)
	portion := os.Getenv(childEnvPortion)

	kp, err := NewFileKeyProvider(keyPath, spoolDir)
	if err != nil {
		return fail("key provider: %v", err)
	}
	cfg := Config{Dir: spoolDir, Keys: kp, SyncEvery: -1}
	switch mode {
	case modeDropCommit:
		// Five records fit; the sixth forces an eviction, whose tombstone is the frame the
		// write interposition stops in.
		cfg.Bounds = Bounds{MaxEntries: 5}
	case modeFoldCrash:
		// Small bound and small segments, so a segment dies and is folded early.
		cfg.Bounds = Bounds{MaxEntries: 4}
		cfg.SegmentBytes = 1024
	}
	sp, err := Open(cfg)
	if err != nil {
		return fail("open: %v", err)
	}

	if mode == modeFoldCrash {
		// The process is stopped between the counter fold and the segment unlink, which is
		// the only window in which a counter could be counted twice. The marker carries the
		// counter's value at that instant so the parent can demand the same number back.
		testReclaimBarrier = func(seg *segment, droppedTotal uint64) {
			signalChildValue(marker, strconv.FormatUint(droppedTotal, 10))
			blockForever()
		}
		for i := 0; i < 1000; i++ {
			if _, err := sp.Append(testEntry(i)); err != nil {
				return fail("append %d: %v", i, err)
			}
		}
		return fail("the reclaim barrier never fired")
	}

	for i := 0; i < 5; i++ {
		if _, err := sp.Append(testEntry(i)); err != nil {
			return fail("append %d: %v", i, err)
		}
	}

	switch mode {
	case modeHold:
		signalChild(marker)
	case modeInFlight:
		seqs := make([]uint64, 0, 5)
		for i := 0; i < 5; i++ {
			seqs = append(seqs, uint64(i))
		}
		if err := sp.MarkInFlight(seqs); err != nil {
			return fail("mark in flight: %v", err)
		}
		signalChild(marker)
	case modeMidWrite, modeDropCommit:
		cut := portion
		if cut == "" {
			cut = "half"
		}
		testWriteHalf = blockingWrite(marker, cut)
		if _, err := sp.Append(testEntry(99)); err != nil {
			return fail("append: %v", err)
		}
		return fail("the interrupted write returned; the test expected to be killed")
	default:
		return fail("unknown mode %q", mode)
	}
	blockForever()
	return 0
}

const (
	portionHalf   = "half"
	portionHeader = "header"
	portionFull   = "full"
)

// blockingWrite writes a prefix of the frame, tells the parent it is there, and blocks
// forever. The parent kills the process at exactly this point, so what is on disk is a
// genuinely torn frame rather than a simulated one.
func blockingWrite(marker, portion string) func(*os.File, []byte) (int, error) {
	return func(f *os.File, frame []byte) (int, error) {
		cut := len(frame)
		switch portion {
		case portionHalf:
			cut = len(frame) / 2
		case portionHeader:
			cut = 8
		case portionFull:
			cut = len(frame)
		}
		n, err := f.Write(frame[:cut])
		if err != nil {
			return n, err
		}
		signalChild(marker)
		blockForever()
		return n, nil
	}
}

func signalChild(marker string) {
	signalChildValue(marker, "ready")
}

func signalChildValue(marker, value string) {
	if err := os.WriteFile(marker, []byte(value), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "crash child: writing marker: %v\n", err)
	}
}

func blockForever() {
	for {
		time.Sleep(time.Hour)
	}
}

// --- the parent half ---------------------------------------------------------------------

type crashRun struct {
	root     string
	spoolDir string
	keyPath  string
}

func newCrashRun(t *testing.T) *crashRun {
	t.Helper()
	root := t.TempDir()
	return &crashRun{
		root:     root,
		spoolDir: filepath.Join(root, "spool"),
		keyPath:  filepath.Join(root, "keys", "spool.key"),
	}
}

type childProc struct {
	cmd     *exec.Cmd
	log     *os.File
	logPath string
	marker  string
}

// spawn starts the child, waits until it has reached the interesting moment, and returns it
// still running.
func (c *crashRun) spawn(t *testing.T, mode, portion string) *childProc {
	t.Helper()
	marker := filepath.Join(c.root, "marker-"+mode+"-"+portion)
	cmd := exec.Command(os.Args[0], "-test.run=TestNoSuchTestExists")
	cmd.Env = append(os.Environ(),
		childEnvSpool+"="+c.spoolDir,
		childEnvKey+"="+c.keyPath,
		childEnvMode+"="+mode,
		childEnvMarker+"="+marker,
		childEnvPortion+"="+portion,
	)
	logPath := filepath.Join(c.root, fmt.Sprintf("child-%s-%s.log", mode, portion))
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatalf("creating the child log: %v", err)
	}
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		logFile.Close()
		t.Fatalf("starting the child process: %v", err)
	}
	deadline := time.Now().Add(60 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			logFile.Close()
			t.Fatalf("the child never reached the write it was supposed to be killed in; its output:\n%s", mustRead(t, logPath))
		}
		time.Sleep(5 * time.Millisecond)
	}
	return &childProc{cmd: cmd, log: logFile, logPath: logPath, marker: marker}
}

// markerValue reads what the child wrote when it reached the point of interest.
func (p *childProc) markerValue(t *testing.T) string {
	t.Helper()
	return strings.TrimSpace(string(mustRead(t, p.marker)))
}

// kill terminates the child with the platform's uncatchable kill and waits for it to die.
func (p *childProc) kill(t *testing.T) {
	t.Helper()
	if err := p.cmd.Process.Kill(); err != nil {
		t.Fatalf("killing the child: %v", err)
	}
	_ = p.cmd.Wait() // non-nil after a kill, and expected
	p.log.Close()
	// The child's own failure path exits with code 2. Anything else means it really was
	// terminated from outside, which is the failure being tested.
	if code := p.cmd.ProcessState.ExitCode(); code == 2 {
		t.Fatalf("the child exited through its own error path (code 2) instead of being killed; its output:\n%s", mustRead(t, p.logPath))
	}
}

func (c *crashRun) open(t *testing.T) *Spool {
	t.Helper()
	return c.openWith(t)
}

// openWith reopens the spool with the same bounds the child ran under; bounds are
// bundle-driven data, so a restart is not required to use the same ones.
func (c *crashRun) openWith(t *testing.T, tweak ...func(*Config)) *Spool {
	t.Helper()
	kp, err := NewFileKeyProvider(c.keyPath, c.spoolDir)
	if err != nil {
		t.Fatalf("key provider: %v", err)
	}
	cfg := Config{Dir: c.spoolDir, Keys: kp, SyncEvery: -1}
	for _, f := range tweak {
		f(&cfg)
	}
	sp, err := Open(cfg)
	if err != nil {
		t.Fatalf("reopening the spool after the kill: %v", err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	return sp
}

// The acceptance test: a process killed in the middle of a write leaves a readable spool in
// which no torn record is counted as delivered, as pending, or as dropped.
func TestCrashKillMidWriteLeavesNoTornRecord(t *testing.T) {
	for _, portion := range []string{portionHalf, portionHeader} {
		t.Run(portion, func(t *testing.T) {
			run := newCrashRun(t)
			run.spawn(t, modeMidWrite, portion).kill(t)

			sp := run.open(t)
			st := sp.Extended()
			if st.Recovery.TornBytes <= 0 {
				t.Fatalf("TornBytes = %d, want the torn frame to be reported", st.Recovery.TornBytes)
			}
			if st.Recovery.TornSegments != 1 {
				t.Fatalf("TornSegments = %d, want 1", st.Recovery.TornSegments)
			}
			if st.Depth != 5 {
				t.Fatalf("Depth = %d, want 5: the torn record must not appear in the queue", st.Depth)
			}
			if st.DroppedTotal != 0 || st.DeliveredTotal != 0 || st.RejectedTotal != 0 {
				t.Fatalf("a torn record was counted: dropped %d delivered %d rejected %d",
					st.DroppedTotal, st.DeliveredTotal, st.RejectedTotal)
			}

			entries, err := sp.Peek(0)
			if err != nil {
				t.Fatalf("Peek: %v", err)
			}
			if len(entries) != 5 {
				t.Fatalf("Peek returned %d records, want the 5 complete ones", len(entries))
			}
			for i, e := range entries {
				if e.Seq != uint64(i) {
					t.Fatalf("record %d has seq %d, want %d", i, e.Seq, i)
				}
				if want := fmt.Sprintf("SECRET-PROMPT-MARKER-%d", i); !strings.Contains(string(e.Payload), want) {
					t.Fatalf("record %d payload %q does not contain %q", i, e.Payload, want)
				}
				if strings.Contains(string(e.Payload), "MARKER-99") {
					t.Fatalf("the record whose write was interrupted is in the queue")
				}
			}

			// The log is usable again: the torn tail was truncated rather than left in
			// place for the next append to follow.
			if _, err := sp.Append(testEntry(50)); err != nil {
				t.Fatalf("Append after recovery: %v", err)
			}
			if err := sp.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}

			// And the truncation is durable: a second open finds a clean tail and the new
			// record.
			sp2 := run.open(t)
			st2 := sp2.Extended()
			if st2.Recovery.TornBytes != 0 {
				t.Fatalf("TornBytes = %d on the second open, want 0", st2.Recovery.TornBytes)
			}
			if st2.Depth != 6 {
				t.Fatalf("Depth = %d on the second open, want 6", st2.Depth)
			}
		})
	}
}

// Crash between the counter fold and the segment unlink — the one window in which the
// counter design could count the same tombstones twice. A real process is stopped there, the
// count it reported at that instant is written to the marker, and reopening must reproduce
// that number exactly.
func TestCrashBetweenTheCounterFoldAndTheUnlink(t *testing.T) {
	run := newCrashRun(t)
	child := run.spawn(t, modeFoldCrash, "")
	atCrash, err := strconv.ParseUint(child.markerValue(t), 10, 64)
	if err != nil {
		t.Fatalf("the child did not report a drop counter at the crash point: %v", err)
	}
	if atCrash == 0 {
		t.Fatal("the child reached the reclaim barrier with a zero drop counter, so the test proves nothing")
	}
	child.kill(t)

	sp := run.open(t)
	if got := sp.DroppedTotal(); got != atCrash {
		t.Fatalf("DroppedTotal = %d after reopening, want %d: the counters of a folded-but-not-unlinked segment were counted twice",
			got, atCrash)
	}
	// The folded segment may still be on disk with an id at or below the watermark; its
	// records must still be readable, and its counters must not be added again.
	st := sp.Extended()
	if st.Watermark == 0 {
		t.Fatal("the crash left no watermark, so the window under test was not reached")
	}
	if st.Depth != st.Pending {
		t.Fatalf("depth %d does not match pending %d", st.Depth, st.Pending)
	}

	// A second open, and a fresh append that forces another eviction, keep the count exact.
	if err := sp.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	sp2 := run.openWith(t, func(c *Config) {
		c.Bounds = Bounds{MaxEntries: 4}
		c.SegmentBytes = 1024
	})
	if got := sp2.DroppedTotal(); got != atCrash {
		t.Fatalf("DroppedTotal = %d on the second open, want %d", got, atCrash)
	}
	if _, err := sp2.Append(testEntry(4242)); err != nil {
		t.Fatalf("Append after recovery: %v", err)
	}
	if got := sp2.DroppedTotal(); got != atCrash+1 {
		before := sp2.Extended()
		t.Fatalf("DroppedTotal = %d after one more append at the bound, want %d (depth %d pending %d in-flight %d watermark %d segments %d bounds %+v)",
			got, atCrash+1, before.Depth, before.Pending, before.InFlight, before.Watermark, before.Segments, before.Bounds)
	}
}

// In-flight is not a delivery. A process killed after handing records to a delivery attempt
// must leave them pending, and the retry must be visible as a retry.
func TestCrashAfterMarkInFlightReturnsRecordsToPending(t *testing.T) {
	run := newCrashRun(t)
	run.spawn(t, modeInFlight, "").kill(t)

	sp := run.open(t)
	st := sp.Extended()
	if st.Recovery.InFlightResetToPending != 5 {
		t.Fatalf("InFlightResetToPending = %d, want 5", st.Recovery.InFlightResetToPending)
	}
	if st.Depth != 5 || st.Pending != 5 || st.InFlight != 0 {
		t.Fatalf("depth %d (pending %d, in-flight %d), want 5/5/0", st.Depth, st.Pending, st.InFlight)
	}
	entries, err := sp.Peek(0)
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	if len(entries) != 5 {
		t.Fatalf("Peek returned %d records, want 5", len(entries))
	}
	for i, e := range entries {
		if e.State != protocol.SpoolPending {
			t.Fatalf("record %d state %q, want pending", i, e.State)
		}
		if e.Attempts != 1 {
			t.Fatalf("record %d attempts %d, want 1: the interrupted attempt must stay visible", i, e.Attempts)
		}
	}
	// They can be handed out again, and the attempt counter advances.
	seqs := make([]uint64, 0, 5)
	for _, e := range entries {
		seqs = append(seqs, e.Seq)
	}
	if err := sp.MarkInFlight(seqs); err != nil {
		t.Fatalf("MarkInFlight after recovery: %v", err)
	}
	if got, _ := sp.Peek(0); len(got) != 0 {
		t.Fatalf("Peek returned %d records after MarkInFlight, want 0", len(got))
	}
}

// Crash mid-drop (§12.2): the tombstone is the commit. The child wrote the whole tombstone
// frame and was killed before applying it, so on reopen the drop must be counted — exactly
// once, not zero times and not twice.
func TestCrashWithTheTombstoneWrittenCountsTheDropExactlyOnce(t *testing.T) {
	run := newCrashRun(t)
	run.spawn(t, modeDropCommit, portionFull).kill(t)

	sp := run.open(t)
	st := sp.Extended()
	if st.DroppedTotal != 1 {
		t.Fatalf("DroppedTotal = %d, want 1: a written tombstone is a committed drop", st.DroppedTotal)
	}
	if st.Depth != 4 {
		t.Fatalf("Depth = %d, want 4", st.Depth)
	}
	entries, err := sp.Peek(0)
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	want := []uint64{1, 2, 3, 4}
	if len(entries) != len(want) {
		t.Fatalf("Peek returned %d records, want %d", len(entries), len(want))
	}
	for i, e := range entries {
		if e.Seq != want[i] {
			t.Fatalf("record %d has seq %d, want %d: the drop must remove the oldest", i, e.Seq, want[i])
		}
	}
	if err := sp.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// The counter is not incremented again by replaying the same tombstone.
	sp2 := run.open(t)
	if got := sp2.DroppedTotal(); got != 1 {
		t.Fatalf("DroppedTotal = %d after a second open, want 1: the tombstone was counted twice", got)
	}
	if got := sp2.Depth(); got != 4 {
		t.Fatalf("Depth = %d after a second open, want 4", got)
	}
}
