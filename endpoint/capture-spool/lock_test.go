package spool

import (
	"errors"
	"testing"
)

// One writer (§3.4, §12). A second open of the same directory is refused while the first
// holds it, and succeeds once it is released.
func TestSecondOpenInTheSameProcessIsRefused(t *testing.T) {
	dir := t.TempDir()
	sp := openTest(t, dir)
	if _, err := sp.Append(testEntry(0)); err != nil {
		t.Fatalf("Append: %v", err)
	}

	if _, err := Open(Config{Dir: dir, Keys: testKey(t), SyncEvery: -1}); !errors.Is(err, ErrWriterActive) {
		t.Fatalf("second Open returned %v, want ErrWriterActive", err)
	}

	if err := sp.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	sp2 := openTest(t, dir)
	if got := sp2.Depth(); got != 1 {
		t.Fatalf("Depth = %d after reopening, want 1", got)
	}
}

// The lock is an operating-system lock, so a killed process releases it. There is no stale
// lock file to detect and no window in which two processes both believe they hold it: the
// child below is killed for real and the parent then opens the same spool.
func TestKilledHolderReleasesTheWriterLock(t *testing.T) {
	run := newCrashRun(t)
	child := run.spawn(t, modeHold, "")

	kp, err := NewFileKeyProvider(run.keyPath, run.spoolDir)
	if err != nil {
		t.Fatalf("key provider: %v", err)
	}
	if _, err := Open(Config{Dir: run.spoolDir, Keys: kp, SyncEvery: -1}); !errors.Is(err, ErrWriterActive) {
		t.Fatalf("Open while the child holds the lock returned %v, want ErrWriterActive", err)
	}

	child.kill(t)

	sp := run.open(t)
	if got := sp.Depth(); got != 5 {
		t.Fatalf("Depth = %d after the holder was killed, want 5", got)
	}
}

// A spool whose directory cannot be created is refused rather than half-opened.
func TestOpenRequiresADirectoryAndAKey(t *testing.T) {
	if _, err := Open(Config{Dir: "", Keys: testKey(t)}); err == nil {
		t.Fatal("Open accepted an empty directory")
	}
	if _, err := Open(Config{Dir: t.TempDir()}); err == nil {
		t.Fatal("Open accepted a nil key provider")
	}
}
