package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Bounds on the providers' answers.
const (
	maxResponseBytes = 64 << 20
	// maxRateRetries is how many times one request waits out a 429 before the pass gives up.
	maxRateRetries = 3
	// maxRetryAfter caps how long one wait lasts, whatever the provider asks.
	maxRetryAfter = time.Minute
	// maxPages bounds one paged listing, so a paging loop that never ends cannot hold a pass.
	maxPages = 500
)

// newHTTPClient is the bounded client an adapter uses when none is given.
func newHTTPClient() *http.Client { return &http.Client{Timeout: 60 * time.Second} }

// get sends a GET with header to endpoint and decodes a 200 answer, JSON, into out.
func get(ctx context.Context, client *http.Client, endpoint string, header http.Header, out any) error {
	body, err := fetch(ctx, client, endpoint, header)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("providers: response is not the expected JSON: %w", err)
	}
	return nil
}

// fetch sends a GET with header to endpoint and returns the body of a 200 answer. A 429 is waited
// out, as long as its Retry-After says and no longer than maxRetryAfter, up to maxRateRetries times.
// A redirect is followed; the client drops the Authorization header when it leaves the host.
func fetch(ctx context.Context, client *http.Client, endpoint string, header http.Header) ([]byte, error) {
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, fmt.Errorf("providers: build request: %w", err)
		}
		for k, v := range header {
			req.Header[k] = v
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("providers: request: %w", err)
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("providers: read response: %w", err)
		}
		if resp.StatusCode == http.StatusTooManyRequests && attempt < maxRateRetries {
			if err := wait(ctx, retryAfter(resp.Header.Get("Retry-After"))); err != nil {
				return nil, err
			}
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return nil, &APIError{Status: resp.StatusCode, Code: errorCode(body)}
		}
		return body, nil
	}
}

func retryAfter(v string) time.Duration {
	if s, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && s >= 0 {
		return min(time.Duration(s)*time.Second, maxRetryAfter)
	}
	if t, err := http.ParseTime(v); err == nil {
		return min(max(time.Until(t), 0), maxRetryAfter)
	}
	return 5 * time.Second
}

func wait(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// errorCode is the machine-readable code of an error body: Graph's and OpenAI's error.code,
// Anthropic's error.type.
func errorCode(body []byte) string {
	var e struct {
		Error struct {
			Code string `json:"code"`
			Type string `json:"type"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil {
		if e.Error.Code != "" {
			return e.Error.Code
		}
		if e.Error.Type != "" {
			return e.Error.Type
		}
	}
	return "no error code"
}

// sameOrigin reports whether next is on base's scheme and host, so a link a provider returns
// cannot send the credential elsewhere.
func sameOrigin(base, next string) bool {
	return strings.HasPrefix(next, strings.TrimRight(base, "/")+"/")
}
