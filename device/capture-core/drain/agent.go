package drain

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/shadow-ai-capture/device/protocol"
)

// downloadTimeout bounds one package request. The edge ends a response after 60 seconds, so a slow
// link takes several requests, each resuming where the last one stopped.
const downloadTimeout = 90 * time.Second

var (
	// ErrNoAgentRelease is the server's statement that it has no agent release to offer.
	ErrNoAgentRelease = errors.New("drain: the deployment offers no agent release")
	// ErrRangeRefused means the server did not resume at the offset asked for: the partial download
	// is not continued and must start again.
	ErrRangeRefused = errors.New("drain: the agent package was not resumed at the offset asked for")
)

// FetchAgentRelease performs GET /v1/agent/release and returns the signed statement, unverified:
// the caller holds the key. A 404 with the error envelope is ErrNoAgentRelease.
func (d *Drainer) FetchAgentRelease(ctx context.Context) ([]byte, error) {
	if !d.ready(ctx) {
		return nil, errors.New("drain: not enrolled; agent release not fetched")
	}
	resp, body, err := d.client.do(ctx, http.MethodGet, protocol.AgentReleasePath, nil, map[string]string{"Accept": "application/json"})
	if err != nil {
		return nil, err
	}
	switch {
	case resp.StatusCode == http.StatusOK:
		return body, nil
	case resp.StatusCode == http.StatusNotFound && classify(resp.StatusCode, body).code != "":
		return nil, ErrNoAgentRelease
	default:
		return nil, classify(resp.StatusCode, body)
	}
}

// DownloadAgentPackage appends GET /v1/agent/package to w, starting at offset (an HTTP range), and
// returns how many bytes it wrote; a partial write is returned with its error, so the next call
// resumes. It reads at most limit bytes past offset.
func (d *Drainer) DownloadAgentPackage(ctx context.Context, w io.Writer, offset, limit int64) (int64, error) {
	if !d.ready(ctx) {
		return 0, errors.New("drain: not enrolled; agent package not downloaded")
	}
	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.client.base+protocol.AgentPackagePath, nil)
	if err != nil {
		return 0, err
	}
	if offset > 0 {
		req.Header.Set("Range", "bytes="+strconv.FormatInt(offset, 10)+"-")
	}
	// The transport, not the client: the client's timeout is sized for API calls.
	resp, err := d.client.tr.RoundTrip(req)
	if err != nil {
		return 0, &apiError{transport: true, cause: err}
	}
	defer resp.Body.Close()
	want := http.StatusOK
	if offset > 0 {
		want = http.StatusPartialContent
	}
	if resp.StatusCode != want {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
		if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusRequestedRangeNotSatisfiable {
			return 0, fmt.Errorf("%w (status %d, offset %d)", ErrRangeRefused, resp.StatusCode, offset)
		}
		return 0, classify(resp.StatusCode, body)
	}
	n, err := io.Copy(w, io.LimitReader(resp.Body, limit))
	if err != nil {
		return n, &apiError{transport: true, cause: err}
	}
	return n, nil
}
