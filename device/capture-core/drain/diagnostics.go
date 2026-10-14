package drain

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/shadow-ai-capture/device/protocol"
)

// diagnosticsTimeout bounds one upload: the edge ends a request after 60 seconds.
const diagnosticsTimeout = 60 * time.Second

// ErrDiagnosticsNotPending is the server's statement that the request the upload answers is not
// waiting for this device: it is unknown, already answered or expired.
var ErrDiagnosticsNotPending = errors.New("drain: the diagnostics request is not waiting for this device")

// UploadDiagnostics performs POST /v1/diagnostics with the bundle that answers requestID. A
// refusal naming the request as not pending is ErrDiagnosticsNotPending; any other failure is
// returned for the caller to try again.
func (d *Drainer) UploadDiagnostics(ctx context.Context, requestID string, bundle []byte) error {
	if len(bundle) > protocol.MaxDiagnosticsBundleBytes {
		return fmt.Errorf("drain: the diagnostics bundle is %d bytes, more than %d", len(bundle), protocol.MaxDiagnosticsBundleBytes)
	}
	if !d.ready(ctx) {
		return errors.New("drain: not enrolled; diagnostics not uploaded")
	}
	ctx, cancel := context.WithTimeout(ctx, diagnosticsTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.client.base+protocol.DiagnosticsUploadPath, bytes.NewReader(bundle))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", protocol.DiagnosticsContentType)
	req.Header.Set(protocol.HeaderDiagnosticsRequest, requestID)
	// The transport, not the client: the client's timeout is sized for small API calls.
	resp, err := d.client.tr.RoundTrip(req)
	if err != nil {
		return &apiError{transport: true, cause: err}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	ae := classify(resp.StatusCode, body)
	if ae.code == protocol.ReasonDiagnosticsNotPending {
		return ErrDiagnosticsNotPending
	}
	return ae
}
