// .integration/repro/inv4 - independent INV-4 probe: an append-only check with a real crash.
//
// The component's own crash tests live inside device/capture-spool (crash_test.go, in-package).
// This harness is deliberately outside it: it appends records with the exported API, kills a
// second process while that process is appending, reopens the spool, and proves that the records
// written before the crash are byte-identical and that the crash left no unreadable state.
//
// Run: cd .integration/repro/inv4 && go run .
// Exit 0 when every assertion holds, 1 otherwise.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/shadow-ai-capture/device/capture-spool"
	"github.com/shadow-ai-capture/device/protocol"
)

var failures []string

func check(condition bool, format string, args ...any) {
	message := fmt.Sprintf(format, args...)
	if !condition {
		failures = append(failures, message)
		fmt.Printf("  FAIL  %s\n", message)
		return
	}
	fmt.Printf("  ok    %s\n", message)
}

var key = []byte("0123456789abcdef0123456789abcdef")

func open(dir string) (*spool.Spool, error) {
	kp, err := spool.NewMemoryKeyProvider(key)
	if err != nil {
		return nil, err
	}
	// No bound for this probe: DefaultBounds() is 25,000 entries and *evicts the oldest* by
	// design, which would remove the committed records the append-only assertion is about.
	// Zero means "no bound of this kind" (spool.go:41-46).
	return spool.Open(spool.Config{Dir: dir, Keys: kp, SyncEvery: -1, Bounds: spool.Bounds{}})
}

func entry(i int, tag string) protocol.Entry {
	return protocol.Entry{
		ClientID:          fmt.Sprintf("%s-client-%d", tag, i),
		Kind:              protocol.KindPrompt,
		Route:             protocol.RouteProxyTLS,
		CollectionMode:    protocol.ModeM1,
		ToolFingerprint:   "tool-1",
		OccurredAt:        time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Second),
		MonotonicOffsetMS: int64(i),
		DedupKey:          fmt.Sprintf("sha256:%064x", i),
		Payload:           []byte(fmt.Sprintf(`{"schema_version":"1.0","event_id":"%s-%d","marker":"%s"}`, tag, i, tag)),
		State:             protocol.SpoolPending,
	}
}

func digest(entries []protocol.Entry) string {
	h := sha256.New()
	for _, e := range entries {
		fmt.Fprintf(h, "%d:%d:%s;", e.Seq, len(e.Payload), e.Payload)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func main() {
	if len(os.Args) > 2 && os.Args[1] == "-child" {
		child(os.Args[2])
		return
	}

	dir, err := os.MkdirTemp("", "inv4-spool-")
	if err != nil {
		fmt.Println("tempdir:", err)
		os.Exit(2)
	}
	defer os.RemoveAll(dir)
	fmt.Printf("INV-4: spool dir %s\n", dir)

	// 1. Three committed records, cleanly closed.
	sp, err := open(dir)
	if err != nil {
		check(false, "first open: %v", err)
		os.Exit(1)
	}
	for i := 0; i < 3; i++ {
		if _, err := sp.Append(entry(i, "before")); err != nil {
			check(false, "append %d: %v", i, err)
		}
	}
	if err := sp.Close(); err != nil {
		check(false, "close: %v", err)
	}
	fmt.Println("  appended and closed 3 records")

	// 2. Snapshot what must never change.
	sp, err = open(dir)
	if err != nil {
		check(false, "reopen before crash: %v", err)
		os.Exit(1)
	}
	before, err := sp.Peek(100)
	if err != nil {
		check(false, "peek before crash: %v", err)
	}
	beforeDigest := digest(before)
	fmt.Printf("  before crash: depth=%d digest=%s\n", len(before), beforeDigest)
	check(len(before) == 3, "the three committed records are readable before the crash (%d)", len(before))
	_ = sp.Close()

	// 3. A second process appends until it is killed mid-write.
	self, err := os.Executable()
	if err != nil {
		check(false, "os.Executable: %v", err)
		os.Exit(1)
	}
	cmd := exec.Command(self, "-child", dir)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		check(false, "start child: %v", err)
		os.Exit(1)
	}
	time.Sleep(300 * time.Millisecond)
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	fmt.Println("  child killed while appending")

	// 4. Reopen: the crash must not have left unreadable state, and must not have rewritten a
	//    single byte of what was already committed.
	sp, err = open(dir)
	if err != nil {
		check(false, "reopen after crash: %v", err)
		os.Exit(1)
	}
	after, err := sp.Peek(100000)
	if err != nil {
		check(false, "peek after crash: %v", err)
	}
	afterDigest := digest(after)
	fmt.Printf("  after crash:  depth=%d digest=%s\n", len(after), afterDigest)
	check(true, "the spool reopened after a killed writer (writer lock released by the crash)")
	check(len(after) >= 3, "at least the three committed records survived (%d)", len(after))

	prefix := after
	if len(prefix) > 3 {
		prefix = prefix[:3]
	}
	check(digest(prefix) == beforeDigest,
		"the first three records are byte-identical after the crash (append-only: nothing was rewritten)")
	same := len(after) >= 3
	for i := 0; i < 3 && i < len(after); i++ {
		if !bytes.Equal(after[i].Payload, before[i].Payload) || after[i].Seq != before[i].Seq {
			same = false
		}
	}
	check(same, "each pre-crash record kept its sequence and payload bytes")

	// Any record the child committed before dying must be whole: Peek already refused to return
	// a torn record (it would have errored), so every returned record is a complete one.
	childEntries := 0
	for _, e := range after {
		if bytes.Contains(e.Payload, []byte("during")) {
			childEntries++
		}
	}
	fmt.Printf("  child records that reached the spool whole: %d\n", childEntries)
	check(childEntries > 0, "the killed child did get records onto the spool before dying (%d)", childEntries)
	_ = sp.Close()

	// 5. Reopen once more: a clean close after the crash must not change the picture either.
	sp, err = open(dir)
	if err != nil {
		check(false, "final reopen: %v", err)
	} else {
		final, perr := sp.Peek(100000)
		check(perr == nil, "final read after reopen: %v", perr)
		if perr == nil {
			check(digest(final) == afterDigest, "a second reopen returns the same bytes (idempotent recovery)")
		}
		_ = sp.Close()
	}

	fmt.Println()
	if len(failures) > 0 {
		fmt.Printf("INV-4: %d assertion(s) FAILED\n", len(failures))
		os.Exit(1)
	}
	fmt.Println("INV-4: every assertion held")
}

func child(dir string) {
	sp, err := open(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "child open:", err)
		os.Exit(2)
	}
	for i := 0; i < 5000; i++ {
		if _, err := sp.Append(entry(i, "during")); err != nil {
			fmt.Fprintln(os.Stderr, "child append:", err)
			os.Exit(2)
		}
	}
	// Hold the spool open without closing it, so the parent kills a live writer rather than
	// waiting for a clean shutdown.
	select {}
}

var _ = filepath.Join
