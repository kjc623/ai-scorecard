package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/drain"
)

func unzip(t *testing.T, raw []byte) map[string][]byte {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]byte{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		out[f.Name] = b
	}
	return out
}

// TestDiagnosticsBundle zips generated and on-disk files with a manifest: a file that does not exist
// is left out, a long file is cut to its tail, and a path that cannot be read is named with its error.
func TestDiagnosticsBundle(t *testing.T) {
	dir := t.TempDir()
	short := filepath.Join(dir, "short.log")
	if err := os.WriteFile(short, []byte("one\ntwo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	long := filepath.Join(dir, "long.log")
	body := append(bytes.Repeat([]byte("a"), maxDiagnosticsFileBytes), []byte("the end")...)
	if err := os.WriteFile(long, body, 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	raw, err := buildDiagnosticsBundle([]diagnosticsFile{
		{Name: "health.json", Data: []byte(`{"ok":true}`)},
		{Name: "short.log", Path: short},
		{Name: "long.log", Path: long},
		{Name: "missing.log", Path: filepath.Join(dir, "missing.log")},
		{Name: "dir", Path: dir},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	files := unzip(t, raw)
	if string(files["health.json"]) != `{"ok":true}` || string(files["short.log"]) != "one\ntwo\n" {
		t.Fatalf("files: %q", files)
	}
	if got := files["long.log"]; len(got) != maxDiagnosticsFileBytes || !bytes.HasSuffix(got, []byte("the end")) {
		t.Fatalf("long.log is %d bytes ending %q; want its last %d bytes", len(got), got[len(got)-7:], maxDiagnosticsFileBytes)
	}
	if _, ok := files["missing.log"]; ok {
		t.Fatal("a missing file is in the bundle")
	}
	var manifest struct {
		CollectedAt time.Time                  `json:"collected_at"`
		Files       []diagnosticsManifestEntry `json:"files"`
	}
	if err := json.Unmarshal(files["manifest.json"], &manifest); err != nil {
		t.Fatal(err)
	}
	if !manifest.CollectedAt.Equal(now) || len(manifest.Files) != 4 {
		t.Fatalf("manifest: %+v", manifest)
	}
	byName := map[string]diagnosticsManifestEntry{}
	for _, e := range manifest.Files {
		byName[e.Name] = e
	}
	if !byName["long.log"].Truncated || byName["short.log"].Truncated || byName["short.log"].Bytes != 8 {
		t.Fatalf("manifest entries: %+v", byName)
	}
	if !strings.Contains(byName["dir"].Error, "directory") {
		t.Fatalf("an unreadable path is not named with its error: %+v", byName["dir"])
	}
}

type fakeDiagnosticsUpload struct {
	mu    sync.Mutex
	calls []string
	errs  []error
	done  chan struct{}
}

func (f *fakeDiagnosticsUpload) UploadDiagnostics(_ context.Context, requestID string, bundle []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, requestID)
	var err error
	if len(f.errs) > 0 {
		err, f.errs = f.errs[0], f.errs[1:]
	}
	if _, zerr := zip.NewReader(bytes.NewReader(bundle), int64(len(bundle))); zerr != nil {
		err = zerr
	}
	f.done <- struct{}{}
	return err
}

func (f *fakeDiagnosticsUpload) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// TestDiagnosticsAnswersEachRequestOnce uploads a request once: a request that is named again after
// it was uploaded, or refused as no longer waiting, is not sent again, and a failed upload is tried
// again the next time the request is named.
func TestDiagnosticsAnswersEachRequestOnce(t *testing.T) {
	up := &fakeDiagnosticsUpload{done: make(chan struct{}, 8), errs: []error{errors.New("edge unreachable"), nil, drain.ErrDiagnosticsNotPending}}
	d := newDiagnostics(up, func() []diagnosticsFile { return []diagnosticsFile{{Name: "health.json", Data: []byte("{}")}} },
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer d.Stop()
	settle := func() {
		t.Helper()
		select {
		case <-up.done:
		case <-time.After(5 * time.Second):
			t.Fatal("no upload")
		}
		d.wg.Wait()
	}

	d.Request("r1")
	settle()
	if st := d.Status(); st.LastAnswer != "error" || st.LastError != "edge unreachable" || st.LastRequest != "r1" {
		t.Fatalf("after a failed upload: %+v", st)
	}
	d.Request("r1")
	settle()
	if st := d.Status(); st.LastAnswer != "uploaded" || st.SizeBytes == 0 {
		t.Fatalf("after the retry: %+v", st)
	}
	d.Request("r1")
	d.Request("r2")
	settle()
	if st := d.Status(); st.LastAnswer != "not_pending" || st.LastRequest != "r2" {
		t.Fatalf("after a refusal: %+v", st)
	}
	d.Request("r2")
	if got := up.Calls(); strings.Join(got, ",") != "r1,r1,r2" {
		t.Fatalf("uploads %v; want r1 twice (one failure, one retry) and r2 once", got)
	}
}

// TestDiagnosticsStopsWithTheService refuses a request once the service is stopping.
func TestDiagnosticsStopsWithTheService(t *testing.T) {
	up := &fakeDiagnosticsUpload{done: make(chan struct{}, 1)}
	d := newDiagnostics(up, func() []diagnosticsFile { return nil }, slog.New(slog.NewTextHandler(io.Discard, nil)))
	d.Stop()
	d.Request("r1")
	d.Stop()
	if got := up.Calls(); len(got) != 0 {
		t.Fatalf("uploads after Stop: %v", got)
	}
}
