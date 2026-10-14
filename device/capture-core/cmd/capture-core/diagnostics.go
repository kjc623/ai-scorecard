package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/drain"
	"github.com/shadow-ai-capture/device/capture-core/state"
	"github.com/shadow-ai-capture/device/protocol"
)

// Diagnostics on request. When the health response names a diagnostics request, the agent zips its
// health snapshot, its configuration summary, its service log (both generations) and the update
// installer's logs, and uploads the zip once. Nothing else in the state directory is read: not the
// keys, the credential, the spool or held content.

// maxDiagnosticsFileBytes bounds what one file contributes: a longer file is cut to its last bytes,
// where the recent events are.
const maxDiagnosticsFileBytes = 8 << 20

// diagnosticsUploader is the drain's POST /v1/diagnostics, behind a seam for tests.
type diagnosticsUploader interface {
	UploadDiagnostics(ctx context.Context, requestID string, bundle []byte) error
}

// diagnosticsFile is one file the bundle carries: generated content, or a file read from disk.
type diagnosticsFile struct {
	Name string
	Data []byte
	Path string
}

// diagnosticsStatus is the last answer in the health document, so an administrator on the device
// can see what was sent and when.
type diagnosticsStatus struct {
	LastRequest string     `json:"last_request,omitempty"`
	LastAnswer  string     `json:"last_answer,omitempty"` // uploaded | not_pending | error
	LastAt      *time.Time `json:"last_at,omitempty"`
	LastError   string     `json:"last_error,omitempty"`
	SizeBytes   int        `json:"size_bytes,omitempty"`
}

type diagnostics struct {
	upload diagnosticsUploader
	files  func() []diagnosticsFile
	log    *slog.Logger
	clock  func() time.Time

	// ctx ends a running answer at Stop.
	ctx    context.Context
	cancel context.CancelFunc

	mu       sync.Mutex
	running  bool
	answered string
	status   diagnosticsStatus
	wg       sync.WaitGroup
}

func newDiagnostics(upload diagnosticsUploader, files func() []diagnosticsFile, log *slog.Logger) *diagnostics {
	ctx, cancel := context.WithCancel(context.Background())
	return &diagnostics{upload: upload, files: files, log: log, clock: time.Now, ctx: ctx, cancel: cancel}
}

// Request answers requestID in the background unless an answer is already running, the request
// was already answered, or the service is stopping. A failed upload is tried again when the next
// health response names the request.
func (d *diagnostics) Request(requestID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.running || requestID == d.answered || d.ctx.Err() != nil {
		return
	}
	d.running = true
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		d.answer(d.ctx, requestID)
	}()
}

func (d *diagnostics) answer(ctx context.Context, requestID string) {
	now := d.clock().UTC()
	bundle, err := buildDiagnosticsBundle(d.files(), now)
	if err == nil {
		err = d.upload.UploadDiagnostics(ctx, requestID, bundle)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.running = false
	d.status = diagnosticsStatus{LastRequest: requestID, LastAt: &now, SizeBytes: len(bundle)}
	switch {
	case err == nil:
		d.answered = requestID
		d.status.LastAnswer = "uploaded"
		d.log.Info("diagnostics: uploaded", "request", requestID, "bytes", len(bundle))
	case errors.Is(err, drain.ErrDiagnosticsNotPending):
		d.answered = requestID
		d.status.LastAnswer = "not_pending"
		d.log.Warn("diagnostics: the request is no longer waiting; nothing was stored", "request", requestID)
	default:
		d.status.LastAnswer, d.status.LastError = "error", err.Error()
		d.log.Warn("diagnostics: not uploaded; tried again at the next health report", "request", requestID, "error", err)
	}
}

// Status is the last answer for the health document.
func (d *diagnostics) Status() diagnosticsStatus {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.status
}

// Stop ends a running answer and waits for it.
func (d *diagnostics) Stop() {
	d.mu.Lock()
	d.cancel()
	d.mu.Unlock()
	d.wg.Wait()
}

// diagnosticsManifestEntry is one file's line in manifest.json.
type diagnosticsManifestEntry struct {
	Name      string `json:"name"`
	Source    string `json:"source,omitempty"`
	Bytes     int    `json:"bytes"`
	Truncated bool   `json:"truncated,omitempty"`
	Error     string `json:"error,omitempty"`
}

// buildDiagnosticsBundle zips the files with a manifest naming each one, where it came from, and
// whether it was cut or could not be read. A file that does not exist is left out.
func buildDiagnosticsBundle(files []diagnosticsFile, now time.Time) ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	manifest := struct {
		AgentVersion string                     `json:"agent_version"`
		CollectedAt  time.Time                  `json:"collected_at"`
		Files        []diagnosticsManifestEntry `json:"files"`
	}{AgentVersion: version, CollectedAt: now, Files: []diagnosticsManifestEntry{}}

	for _, f := range files {
		entry := diagnosticsManifestEntry{Name: f.Name, Source: f.Path}
		data := f.Data
		if f.Path != "" {
			var err error
			data, entry.Truncated, err = readTail(f.Path, maxDiagnosticsFileBytes)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				entry.Error = err.Error()
				manifest.Files = append(manifest.Files, entry)
				continue
			}
		}
		entry.Bytes = len(data)
		w, err := zw.CreateHeader(&zip.FileHeader{Name: f.Name, Method: zip.Deflate, Modified: now})
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(data); err != nil {
			return nil, err
		}
		manifest.Files = append(manifest.Files, entry)
	}
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	w, err := zw.CreateHeader(&zip.FileHeader{Name: "manifest.json", Method: zip.Deflate, Modified: now})
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(raw); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	if buf.Len() > protocol.MaxDiagnosticsBundleBytes {
		return nil, fmt.Errorf("the diagnostics bundle is %d bytes, more than %d", buf.Len(), protocol.MaxDiagnosticsBundleBytes)
	}
	return buf.Bytes(), nil
}

// readTail reads at most limit bytes from the end of the file, and reports whether it was cut.
func readTail(path string, limit int64) ([]byte, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, false, err
	}
	if info.IsDir() {
		return nil, false, fmt.Errorf("%s is a directory", path)
	}
	cut := info.Size() > limit
	if cut {
		if _, err := f.Seek(-limit, io.SeekEnd); err != nil {
			return nil, false, err
		}
	}
	data, err := io.ReadAll(io.LimitReader(f, limit))
	return data, cut, err
}

// diagnosticsFiles is what the agent's bundle carries: the health snapshot as of now, the
// configuration summary --print-config gives, the service log, and the update's attempt record and
// each version's installer log.
func (s *service) diagnosticsFiles() []diagnosticsFile {
	var files []diagnosticsFile
	if raw, err := json.MarshalIndent(s.health.Snapshot(), "", "  "); err == nil {
		files = append(files, diagnosticsFile{Name: state.HealthFile, Data: raw})
	}
	var cfg bytes.Buffer
	if err := printConfig(s.cfg, &cfg); err != nil {
		fmt.Fprintf(&cfg, "error: %v\n", err)
	}
	files = append(files, diagnosticsFile{Name: "config.txt", Data: cfg.Bytes()})
	log := s.dir.Path(state.LogFile)
	files = append(files,
		diagnosticsFile{Name: state.LogFile, Path: log},
		diagnosticsFile{Name: state.LogFile + ".1", Path: log + ".1"},
	)
	update := s.dir.Path(updateDir)
	files = append(files, diagnosticsFile{Name: "update/attempt.json", Path: filepath.Join(update, "attempt.json")})
	entries, _ := os.ReadDir(update)
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, e := range entries {
		if e.IsDir() {
			files = append(files, diagnosticsFile{Name: "update/" + e.Name() + "/install.log", Path: filepath.Join(update, e.Name(), "install.log")})
		}
	}
	return files
}
