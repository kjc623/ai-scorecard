package main

import (
	"context"
	"encoding/csv"
	"errors"
	"io"
	"log/slog"
	"os/exec"
	"runtime"
	"strconv"
	"strings"

	"github.com/shadow-ai-capture/device/capture-core/detect"
)

// Process enumeration is platform code, and on this host only one shape of it is available: the
// Windows `tasklist` command. It gives an image name and a PID — no code-signature subject, no
// version metadata, no loaded modules, no listening sockets and no compute signature.
//
// The consequence is stated rather than hidden: this enumerator can match §4.4's *candidate* rule
// (an inference-runtime signature in the signed seed set), so proc.detect produces coverage rows and
// daily rollups, but it can never produce evidence of use, so it never emits a `model_detection`.
// The missing evidence paths are the named gap: loaded modules, listening sockets and the coarse
// compute signature require platform APIs that are not implemented in this build.
//
// It is opt-in (--proc-detect) because a partial enumerator that silently under-reports is worse
// than a provider that is honestly absent.
type tasklistEnumerator struct {
	log *slog.Logger
}

// newProcessEnumerator returns an enumerator for this host, or nil when none is available.
func newProcessEnumerator(log *slog.Logger) detect.Enumerator {
	if runtime.GOOS != "windows" {
		return nil
	}
	if _, err := exec.LookPath("tasklist"); err != nil {
		return nil
	}
	return &tasklistEnumerator{log: log}
}

// Enumerate runs tasklist and parses its CSV output. A failure returns an error, which proc.detect
// turns into `degraded` with detail=enumeration_partial — never into an empty process table, which
// would read as "nothing is running".
func (t *tasklistEnumerator) Enumerate(ctx context.Context) ([]detect.ProcessInfo, error) {
	cmd := exec.CommandContext(ctx, "tasklist", "/FO", "CSV", "/NH")
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
			return nil, errors.New("tasklist: " + strings.TrimSpace(string(exitErr.Stderr)))
		}
		return nil, err
	}
	return parseTasklistCSV(strings.NewReader(string(out)))
}

// parseTasklistCSV parses `tasklist /FO CSV /NH`: "Image Name","PID","Session Name","Session#","Mem Usage".
// It is exported to the package for the selftest's benefit: the parser is the part worth testing,
// and the command is the part that is not available everywhere.
func parseTasklistCSV(r io.Reader) ([]detect.ProcessInfo, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	var procs []detect.ProcessInfo
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if len(rec) < 2 {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(rec[1]))
		if err != nil {
			continue
		}
		procs = append(procs, detect.ProcessInfo{
			PID:       pid,
			ImagePath: strings.TrimSpace(rec[0]),
		})
	}
	return procs, nil
}
