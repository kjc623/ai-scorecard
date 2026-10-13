package providers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// chatgptFixture is two pages of log files, the first file a redirect to storage as the platform
// serves it.
func chatgptFixture(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var seen []string
	files := map[string]string{
		"eclf_1": strings.Join([]string{
			`{"event_id":"ev-1","type":"CONVERSATION_MESSAGE","timestamp":"2026-10-12T10:00:00.5Z","actor":{"user_email":"ada@contoso.example"},` +
				`"message":{"id":"m-1","created_at":"2026-10-12T10:00:00.243000Z","author":{"type":"user","client_type":"desktop_web"},"content":{"type":"text","value":"Draft the Q3 note"}},"conversation":{"id":"c-1"}}`,
			`{"event_id":"ev-2","type":"CONVERSATION_MESSAGE","actor":{"user_email":"ada@contoso.example"},` +
				`"message":{"id":"m-2","created_at":"2026-10-12T10:00:05Z","author":{"type":"assistant"},"content":{"type":"text","value":"Here it is"}}}`,
			`{"event_id":"ev-3","type":"CONVERSATION_MESSAGE","actor":{"user_email":"ada@contoso.example"},` +
				`"message":{"id":"m-3","created_at":"2026-10-12T10:01:00Z","author":{"type":"user"},"content":{"type":"image","value":null}}}`,
			``,
			`not json`,
		}, "\n"),
		"eclf_2": `{"event_id":"ev-4","type":"CONVERSATION_MESSAGE","timestamp":"2026-10-12T11:00:00Z","actor":{"user_email":"grace@contoso.example"},` +
			`"message":{"author":{"type":"user"},"content":{"value":"Second file"}}}`,
	}
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("GET /v1/compliance/workspaces/ws-1/logs", func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization")+" "+r.URL.RawQuery)
		if r.URL.Query().Get("event_type") != "CONVERSATION_MESSAGE" {
			http.Error(w, "type", http.StatusBadRequest)
			return
		}
		switch r.URL.Query().Get("after") {
		case "2026-10-12T00:00:00Z":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"id": "eclf_1"}}, "has_more": true, "last_end_time": "2026-10-12T10:30:00Z"})
		case "2026-10-12T10:30:00Z":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"id": "eclf_2"}}, "has_more": false, "last_end_time": "2026-10-12T11:30:00Z"})
		default:
			http.Error(w, "after", http.StatusBadRequest)
		}
	})
	mux.HandleFunc("GET /v1/compliance/workspaces/ws-1/logs/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer key-1" {
			http.Error(w, "auth", http.StatusUnauthorized)
			return
		}
		if r.PathValue("id") == "eclf_1" {
			http.Redirect(w, r, srv.URL+"/storage/eclf_1", http.StatusFound)
			return
		}
		_, _ = w.Write([]byte(files[r.PathValue("id")]))
	})
	mux.HandleFunc("GET /storage/{id}", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(files[r.PathValue("id")]))
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &seen
}

func pull(t *testing.T, src Source, c Connection) ([]Record, string, bool, error) {
	t.Helper()
	var got []Record
	next, more, err := src.Pull(context.Background(), c, func(r Record) error {
		got = append(got, r)
		return nil
	})
	return got, next, more, err
}

func TestChatGPTReadsWhatPeopleWroteFromEveryLogFile(t *testing.T) {
	srv, seen := chatgptFixture(t)
	g := NewChatGPT(srv.Client())
	g.BaseURL = srv.URL
	got, next, more, err := pull(t, g, Connection{Account: "ws-1", Key: "key-1", Cursor: "2026-10-12T00:00:00Z"})
	if err != nil || more || next != "2026-10-12T11:30:00Z" {
		t.Fatalf("next %q more %v err %v", next, more, err)
	}
	if len(got) != 2 {
		t.Fatalf("%d records: %+v", len(got), got)
	}
	want := Record{ID: "m-1", OccurredAt: time.Date(2026, 10, 12, 10, 0, 0, 243000000, time.UTC), App: "chatgpt_web", UPN: "ada@contoso.example", Text: "Draft the Q3 note"}
	if got[0].ID != want.ID || !got[0].OccurredAt.Equal(want.OccurredAt) || got[0].App != want.App || got[0].UPN != want.UPN || got[0].Text != want.Text {
		t.Fatalf("first record %+v", got[0])
	}
	// A message with no id is known by its event, and with no time of its own by the event's.
	if got[1].ID != "ev-4" || !got[1].OccurredAt.Equal(time.Date(2026, 10, 12, 11, 0, 0, 0, time.UTC)) {
		t.Fatalf("second record %+v", got[1])
	}
	if len(*seen) != 2 || !strings.HasPrefix((*seen)[0], "Bearer key-1 ") {
		t.Fatalf("listings %v", *seen)
	}
}

func TestChatGPTStartsAWeekBackAndReportsARefusedKey(t *testing.T) {
	var after string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		after = r.URL.Query().Get("after")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":"invalid_api_key","message":"secret detail"}}`))
	}))
	defer srv.Close()
	g := NewChatGPT(srv.Client())
	g.BaseURL = srv.URL
	g.Now = func() time.Time { return time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC) }
	_, next, _, err := pull(t, g, Connection{Account: "ws-1", Key: "bad"})
	if after != "2026-10-06T12:00:00Z" {
		t.Fatalf("first pass read from %q", after)
	}
	var ae *APIError
	if next != "" || !errors.As(err, &ae) || ae.Status != 401 || ae.Code != "invalid_api_key" || failureCode(ChatGPTEnterprise, err) != ErrCredentialRefused {
		t.Fatalf("next %q err %v", next, err)
	}
	if strings.Contains(err.Error(), "secret detail") {
		t.Fatal("the error carries the provider's message")
	}
}

func TestARateLimitIsWaitedOut(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`{"data":[],"has_more":false}`))
	}))
	defer srv.Close()
	g := NewChatGPT(srv.Client())
	g.BaseURL = srv.URL
	if _, _, _, err := pull(t, g, Connection{Account: "ws-1", Key: "k", Cursor: "2026-10-12T00:00:00Z"}); err != nil || calls != 2 {
		t.Fatalf("err %v after %d calls", err, calls)
	}
}
