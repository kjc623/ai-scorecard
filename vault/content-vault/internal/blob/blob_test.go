package blob_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shadow-ai-capture/content-vault/internal/blob"
)

// The reader presents the credential and reads the object the endpoint serves.
func TestHTTPReaderPresentsTheCredential(t *testing.T) {
	var gotAuth, gotVersion string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotVersion = r.Header.Get("x-ms-version")
		if r.URL.Path != "/tenants/t/objects/o" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte("ciphertext"))
	}))
	defer srv.Close()

	r := &blob.HTTPReader{Endpoint: srv.URL, Credential: blob.StaticBearer{Token: "lab-token"}}
	got, err := r.Get(context.Background(), "tenants/t/objects/o")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != "ciphertext" {
		t.Fatalf("Get = %q, want the stored bytes", got)
	}
	if gotAuth != "Bearer lab-token" {
		t.Errorf("Authorization = %q, want the static bearer", gotAuth)
	}
	if gotVersion == "" {
		t.Error("the reader sent no x-ms-version; Azure Blob requires one")
	}
}

// A storage layer that refuses the credential is a distinct error, not an empty object.
func TestHTTPReaderRefusesAnUnauthorizedRead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no credential", http.StatusForbidden)
	}))
	defer srv.Close()

	r := &blob.HTTPReader{Endpoint: srv.URL, Credential: blob.StaticBearer{Token: "wrong"}}
	_, err := r.Get(context.Background(), "objects/one")
	if !errors.Is(err, blob.ErrUnauthorized) {
		t.Fatalf("Get error = %v, want ErrUnauthorized", err)
	}
}

// An absent object is its own error, so a retention sweep and a credential problem are not confused.
func TestHTTPReaderReportsAnAbsentObject(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	}))
	defer srv.Close()
	r := &blob.HTTPReader{Endpoint: srv.URL, Credential: blob.StaticBearer{Token: "t"}}
	if _, err := r.Get(context.Background(), "gone"); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("Get error = %v, want ErrNotFound", err)
	}
}

// No endpoint is "the read cannot be made", not "the object is empty".
func TestHTTPReaderWithoutAnEndpoint(t *testing.T) {
	r := &blob.HTTPReader{}
	if _, err := r.Get(context.Background(), "objects/one"); !errors.Is(err, blob.ErrNoEndpoint) {
		t.Fatalf("Get error = %v, want ErrNoEndpoint", err)
	}
}

// A managed identity fetches once, serves from cache, and refreshes when the token nears expiry.
func TestManagedIdentityFetchesCachesAndRefreshes(t *testing.T) {
	var hits int64
	var wantResource string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		if r.Header.Get("Metadata") != "true" {
			t.Errorf("the IMDS request needs the Metadata header, got %q", r.Header.Get("Metadata"))
		}
		wantResource = r.URL.Query().Get("resource")
		expires := time.Now().Add(time.Hour).Unix()
		fmt.Fprintf(w, `{"access_token":"tok-%d","expires_on":"%d"}`, atomic.LoadInt64(&hits), expires)
	}))
	defer srv.Close()

	clock := time.Now()
	mi := &blob.ManagedIdentity{
		Endpoint: srv.URL, Resource: "https://storage.azure.com/",
		Now: func() time.Time { return clock },
	}
	req, _ := http.NewRequest(http.MethodGet, "https://acct.blob.core.windows.net/c/o", nil)
	if err := mi.Authorize(context.Background(), req); err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if got := req.Header.Get("Authorization"); !strings.HasPrefix(got, "Bearer tok-") {
		t.Fatalf("Authorization = %q, want a bearer token", got)
	}
	if wantResource != "https://storage.azure.com/" {
		t.Errorf("resource = %q, want the storage scope", wantResource)
	}

	// A second call inside the window uses the cache: no second fetch.
	req2, _ := http.NewRequest(http.MethodGet, "https://acct.blob.core.windows.net/c/o", nil)
	if err := mi.Authorize(context.Background(), req2); err != nil {
		t.Fatal(err)
	}
	if n := atomic.LoadInt64(&hits); n != 1 {
		t.Fatalf("the identity was asked %d times, want the cached token reused", n)
	}

	// Move the clock past the refresh skew: the next call fetches a fresh token.
	clock = clock.Add(59 * time.Minute)
	req3, _ := http.NewRequest(http.MethodGet, "https://acct.blob.core.windows.net/c/o", nil)
	if err := mi.Authorize(context.Background(), req3); err != nil {
		t.Fatal(err)
	}
	if n := atomic.LoadInt64(&hits); n != 2 {
		t.Fatalf("the identity was asked %d times, want a refresh near expiry", n)
	}
}
