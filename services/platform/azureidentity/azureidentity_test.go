package azureidentity

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func endpoint(t *testing.T, calls *atomic.Int32, expiresOn string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("X-IDENTITY-HEADER") != "secret-header" {
			http.Error(w, "missing header", http.StatusBadRequest)
			return
		}
		q := r.URL.Query()
		if q.Get("api-version") != "2019-08-01" || q.Get("client_id") != "client-1" {
			http.Error(w, "bad query", http.StatusBadRequest)
			return
		}
		fmt.Fprintf(w, `{"access_token":"token-for-%s","expires_on":%s}`, q.Get("resource"), expiresOn)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestTokenIsFetchedPerResourceAndCached(t *testing.T) {
	var calls atomic.Int32
	expires := time.Now().Add(time.Hour).Unix()
	srv := endpoint(t, &calls, fmt.Sprintf(`"%d"`, expires))
	c := New(srv.URL, "secret-header", "client-1", nil)

	for range 3 {
		tok, err := c.Token(context.Background(), ResourcePostgres)
		if err != nil {
			t.Fatal(err)
		}
		if tok != "token-for-"+ResourcePostgres {
			t.Fatalf("token = %q", tok)
		}
	}
	if _, err := c.Token(context.Background(), ResourceStorage); err != nil {
		t.Fatal(err)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("endpoint called %d times, want one per resource", n)
	}
}

func TestTokenIsRefreshedBeforeExpiry(t *testing.T) {
	var calls atomic.Int32
	now := time.Now()
	srv := endpoint(t, &calls, fmt.Sprintf("%d", now.Add(10*time.Minute).Unix()))
	c := New(srv.URL, "secret-header", "client-1", nil)
	c.now = func() time.Time { return now }

	if _, err := c.Token(context.Background(), ResourceKeyVault); err != nil {
		t.Fatal(err)
	}
	c.now = func() time.Time { return now.Add(6 * time.Minute) } // inside the refresh window
	if _, err := c.Token(context.Background(), ResourceKeyVault); err != nil {
		t.Fatal(err)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("endpoint called %d times, want a refresh inside the skew", n)
	}
}

func TestRefusalIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no", http.StatusForbidden)
	}))
	defer srv.Close()
	if _, err := New(srv.URL, "h", "", nil).Token(context.Background(), ResourcePostgres); err == nil {
		t.Fatal("a refused token request returned no error")
	}
}

func TestFromEnvironmentRequiresTheEndpoint(t *testing.T) {
	t.Setenv("IDENTITY_ENDPOINT", "")
	t.Setenv("IDENTITY_HEADER", "")
	if _, err := FromEnvironment(); err == nil {
		t.Fatal("FromEnvironment succeeded without a managed identity")
	}
}
