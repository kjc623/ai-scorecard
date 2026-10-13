package providers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func claudeFixture(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var queries []string
	mux := http.NewServeMux()
	auth := func(w http.ResponseWriter, r *http.Request) bool {
		queries = append(queries, r.URL.Path+"?"+r.URL.RawQuery)
		if r.Header.Get("X-Api-Key") != "sk-ant-api01-k" || r.Header.Get("Anthropic-Version") != anthropicVersion {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`))
			return false
		}
		return true
	}
	write := func(w http.ResponseWriter, v any) { _ = json.NewEncoder(w).Encode(v) }
	mux.HandleFunc("GET /v1/compliance/apps/chats", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		q := r.URL.Query()
		if q.Get("order_by") != "updated_at" || q.Get("organization_ids[]") != "org-1" {
			http.Error(w, "query", http.StatusBadRequest)
			return
		}
		if q.Get("after_id") == "" {
			write(w, map[string]any{"data": []map[string]any{
				{"id": "chat_1", "updated_at": "2026-10-12T10:00:00Z", "user": map[string]string{"email_address": "ada@contoso.example"}},
			}, "has_more": true, "last_id": "chat_1"})
			return
		}
		write(w, map[string]any{"data": []map[string]any{
			{"id": "chat_2", "updated_at": "2026-10-12T12:00:00Z", "user": map[string]string{"email_address": "grace@contoso.example"}},
		}, "has_more": false, "last_id": "chat_2"})
	})
	mux.HandleFunc("GET /v1/compliance/apps/chats/{id}/messages", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		switch r.PathValue("id") {
		case "chat_1":
			write(w, map[string]any{"chat_messages": []map[string]any{
				// Written before the walk's start: an earlier pass read it.
				{"id": "msg_0", "role": "user", "created_at": "2026-10-11T23:00:00Z", "content": []map[string]any{{"type": "text", "text": "old"}}},
				{"id": "msg_1", "role": "user", "created_at": "2026-10-12T09:59:00Z",
					"content": []map[string]any{{"type": "text", "text": "Review this contract"}, {"type": "text", "text": "for Contoso"}},
					"files":   []map[string]any{{"filename": "msa.pdf", "mime_type": "application/pdf", "size_bytes": 12345}}},
				{"id": "msg_2", "role": "assistant", "created_at": "2026-10-12T09:59:30Z", "content": []map[string]any{{"type": "text", "text": "Sure"}}},
			}, "has_more": false})
		case "chat_2":
			write(w, map[string]any{"chat_messages": []map[string]any{
				{"id": "msg_3", "role": "user", "created_at": "2026-10-12T11:59:00Z", "content": []map[string]any{{"type": "text", "text": "Hello"}}},
			}, "has_more": false})
		}
	})
	mux.HandleFunc("GET /v1/compliance/apps/sessions/local", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		next := "p2"
		data := []map[string]any{
			{"id": "clls_1", "organization_uuid": "org-1", "product_surface": "claude_code", "updated_at": "2026-10-12T13:00:00Z", "user": map[string]string{"email_address": "ada@contoso.example"}},
			{"id": "clls_x", "organization_uuid": "org-other", "product_surface": "claude_code", "updated_at": "2026-10-12T13:00:00Z", "user": map[string]string{"email_address": "eve@other.example"}},
		}
		if r.URL.Query().Get("page") == "p2" {
			data = []map[string]any{{"id": "clls_2", "organization_uuid": "org-1", "product_surface": "cowork", "updated_at": "2026-10-12T11:00:00Z", "user": map[string]string{"email_address": "grace@contoso.example"}}}
			write(w, map[string]any{"data": data, "next_page": nil})
			return
		}
		write(w, map[string]any{"data": data, "next_page": next})
	})
	mux.HandleFunc("GET /v1/compliance/apps/sessions/local/{id}/messages", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		msgs := map[string][]map[string]any{
			"clls_1": {
				{"id": "clsm_1", "role": "user", "created_at": "2026-10-12T12:58:00Z", "content": []map[string]any{{"type": "text", "text": "fix the failing test"}}},
				{"id": "clsm_2", "role": "user", "created_at": "2026-10-12T12:58:30Z", "content": []map[string]any{{"type": "tool_result", "tool_use_id": "toolu_1"}}},
			},
			"clls_2": {{"id": "clsm_3", "role": "user", "created_at": "2026-10-12T10:59:00Z", "content": []map[string]any{{"type": "text", "text": "tidy my desktop"}}}},
			"clls_x": {{"id": "clsm_x", "role": "user", "created_at": "2026-10-12T12:00:00Z", "content": []map[string]any{{"type": "text", "text": "not ours"}}}},
		}
		write(w, map[string]any{"session": map[string]string{"id": r.PathValue("id")}, "data": msgs[r.PathValue("id")], "next_page": nil})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &queries
}

func TestClaudeReadsChatsAndLocalSessions(t *testing.T) {
	srv, queries := claudeFixture(t)
	a := NewClaude(srv.Client())
	a.BaseURL = srv.URL
	cursor := `{"chats":"2026-10-12T00:00:00Z","sessions":"2026-10-12T00:00:00Z"}`
	got, next, more, err := pull(t, a, Connection{Account: "org-1", Key: "sk-ant-api01-k", Cursor: cursor})
	if err != nil || more {
		t.Fatalf("more %v err %v", more, err)
	}
	var ids []string
	for _, r := range got {
		ids = append(ids, r.ID+"@"+r.App+"/"+r.UPN)
	}
	want := "msg_1@claude_web/ada@contoso.example msg_3@claude_web/grace@contoso.example " +
		"clsm_1@claude_code/ada@contoso.example clsm_3@claude_desktop/grace@contoso.example"
	if strings.Join(ids, " ") != want {
		t.Fatalf("records\n %s\nwant\n %s", strings.Join(ids, " "), want)
	}
	if got[0].Text != "Review this contract\nfor Contoso" || len(got[0].Attachments) != 1 ||
		got[0].Attachments[0].Name != "msa.pdf" || got[0].Attachments[0].SizeBytes != 12345 {
		t.Fatalf("first record %+v", got[0])
	}
	var cur claudeCursor
	if err := json.Unmarshal([]byte(next), &cur); err != nil {
		t.Fatalf("cursor %q: %v", next, err)
	}
	// Each walk resumes a little before the latest update it read.
	if cur.Chats != "2026-10-12T11:50:00Z" || cur.Sessions != "2026-10-12T12:50:00Z" {
		t.Fatalf("cursor %+v", cur)
	}
	if !strings.Contains((*queries)[0], "updated_at.gte=2026-10-12T00%3A00%3A00Z") {
		t.Fatalf("first listing %q", (*queries)[0])
	}
}

func TestClaudeStartsAWeekBackAndReportsARefusedKey(t *testing.T) {
	srv, queries := claudeFixture(t)
	a := NewClaude(srv.Client())
	a.BaseURL = srv.URL
	a.Now = func() time.Time { return time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC) }
	_, _, _, err := pull(t, a, Connection{Account: "org-1", Key: "wrong"})
	if failureCode(ClaudeEnterprise, err) != ErrCredentialRefused || !strings.Contains(err.Error(), "authentication_error") {
		t.Fatalf("err %v", err)
	}
	if !strings.Contains((*queries)[0], "updated_at.gte=2026-10-06T12%3A00%3A00Z") {
		t.Fatalf("first listing %q", (*queries)[0])
	}
}

func TestResumeFromNeverMovesBack(t *testing.T) {
	if got := resumeFrom("2026-10-12T12:00:00Z", "2026-10-12T12:05:00Z"); got != "2026-10-12T12:00:00Z" {
		t.Fatalf("got %q", got)
	}
	if got := resumeFrom("", "not a time"); got != "" {
		t.Fatalf("got %q", got)
	}
}
