package drain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/shadow-ai-capture/device/protocol"
)

// ReportHealth sends one report to POST /v1/health over the device's authenticated transport. It is
// the periodic heartbeat: an idle device still says it is alive, so an absence of events is never
// read as a dead device. A failure is returned for the health channel to record; the next tick is
// the retry.
func (d *Drainer) ReportHealth(ctx context.Context, req protocol.HealthRequest) (protocol.HealthResponse, error) {
	if err := req.Validate(); err != nil {
		return protocol.HealthResponse{}, err
	}
	if !d.ready(ctx) {
		return protocol.HealthResponse{}, errors.New("drain: not enrolled; health report not sent")
	}
	raw, err := json.Marshal(req)
	if err != nil {
		return protocol.HealthResponse{}, fmt.Errorf("drain: encoding health report: %w", err)
	}
	resp, body, err := d.client.do(ctx, http.MethodPost, "/v1/health", raw, map[string]string{"Content-Type": "application/json"})
	if err != nil {
		return protocol.HealthResponse{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return protocol.HealthResponse{}, classify(resp.StatusCode, body)
	}
	var out protocol.HealthResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return protocol.HealthResponse{}, fmt.Errorf("drain: decoding health response: %w", err)
	}
	return out, nil
}
