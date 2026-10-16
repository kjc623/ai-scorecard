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

	"github.com/shadow-ai-capture/device/protocol/canon"

	"github.com/shadow-ai-capture/control-api/internal/graphsync"
)

type fakeTokens struct{ err error }

func (f fakeTokens) Token(_ context.Context, tid, scope string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return "tok-" + tid + "-" + scope, nil
}

type fakePeople struct {
	users []graphsync.User
	err   error
}

func (f fakePeople) Users(_ context.Context, _ string, fn func(graphsync.User) error) error {
	if f.err != nil {
		return f.err
	}
	for _, u := range f.users {
		if err := fn(u); err != nil {
			return err
		}
	}
	return nil
}

const (
	oidAda    = "6f9619ff-8b86-d011-b42d-00c04fc964ff"
	oidGrace  = "26118915-6090-4610-87a4-49d0767e9fde"
	oidGuest  = "aaaaaaaa-6090-4610-87a4-49d0767e9fde"
	entraTID  = "061ea7c2-cc15-418b-bfc0-9f6a7d98011c"
	copilotAt = "2026-10-13T12:00:00Z"
)

func copilotFixture(t *testing.T, licensed map[string]bool) (*httptest.Server, *[]string) {
	t.Helper()
	var filters []string
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok-"+entraTID+"-"+graphsync.GraphScope {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		parts := strings.Split(r.URL.Path, "/")
		if len(parts) < 5 || parts[2] != "copilot" || parts[3] != "users" {
			http.NotFound(w, r)
			return
		}
		oid := parts[4]
		if !licensed[oid] {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"code":"Forbidden","message":"no licence"}}`))
			return
		}
		filters = append(filters, r.URL.Query().Get("$filter"))
		if r.URL.Query().Get("$skiptoken") == "" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"@odata.nextLink": srv.URL + r.URL.Path + "?$skiptoken=2",
				"value": []map[string]any{
					{"id": "1732148356886", "interactionType": "userPrompt", "createdDateTime": "2026-10-13T09:19:16.886Z",
						"appClass": "IPM.SkypeTeams.Message.Copilot.BizChat",
						"from":     map[string]any{"user": map[string]string{"id": oid}},
						"body":     map[string]string{"contentType": "text", "content": "What is on my radar?"}},
					{"id": "1732148357313", "interactionType": "aiResponse", "createdDateTime": "2026-10-13T09:19:17.313Z",
						"body": map[string]string{"contentType": "html", "content": "<p>Nothing</p>"}},
				},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"value": []map[string]any{
			{"id": "1732148399000", "interactionType": "userPrompt", "createdDateTime": "2026-10-13T09:20:00Z",
				"body":        map[string]string{"contentType": "html", "content": "<p>Summarise <b>Q3</b> &amp; plan</p><p>for Contoso</p>"},
				"attachments": []map[string]any{{"name": "Q3.xlsx", "contentType": "reference"}, {"name": nil, "contentType": "application/vnd.microsoft.card.adaptive"}},
				"links":       []map[string]any{{"displayName": "OnCall DRI Handbook-v3.pdf", "linkType": "File"}, {"displayName": "Q3 review", "linkType": "Meeting"}}},
		}})
	}))
	t.Cleanup(srv.Close)
	return srv, &filters
}

func newTestCopilot(t *testing.T, srv *httptest.Server, people People, tokens TokenSource) *Copilot {
	t.Helper()
	a, err := NewCopilot(tokens, people, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	a.BaseURL = srv.URL
	a.Now = func() time.Time { return parseTime(copilotAt) }
	return a
}

var directory = fakePeople{users: []graphsync.User{
	{ID: strings.ToUpper(oidAda), UserPrincipalName: "ada@contoso.example"},
	{ID: oidGrace, UserPrincipalName: "grace@contoso.example"},
	{ID: oidGuest, UserPrincipalName: "eve_other#EXT#@contoso.example", UserType: "Guest"},
}}

func TestCopilotReadsEachLicensedPersonsPrompts(t *testing.T) {
	srv, filters := copilotFixture(t, map[string]bool{oidAda: true})
	a := newTestCopilot(t, srv, directory, fakeTokens{})
	got, next, more, err := pull(t, a, Connection{EntraTenantID: entraTID, Cursor: "2026-10-13T06:00:00Z"})
	if err != nil || more || next != "2026-10-13T12:00:00Z" {
		t.Fatalf("next %q more %v err %v", next, more, err)
	}
	if len(got) != 2 {
		t.Fatalf("%d records: %+v", len(got), got)
	}
	if got[0].ID != oidAda+"/1732148356886" || got[0].App != "m365_copilot" || got[0].UPN != "ada@contoso.example" ||
		got[0].ObjectID != oidAda || got[0].Text != "What is on my radar?" {
		t.Fatalf("first record %+v", got[0])
	}
	if got[1].Text != "\nSummarise Q3 & plan\n\nfor Contoso\n" || len(got[1].Attachments) != 2 ||
		got[1].Attachments[0].Name != "Q3.xlsx" || got[1].Attachments[1].Name != "OnCall DRI Handbook-v3.pdf" {
		t.Fatalf("second record %q %+v", got[1].Text, got[1].Attachments)
	}
	// The window starts a little before the cursor, since Graph lists an interaction late.
	if len(*filters) == 0 || (*filters)[0] != "createdDateTime gt 2026-10-13T05:30:00Z and createdDateTime lt 2026-10-13T12:00:00Z" {
		t.Fatalf("filters %v", *filters)
	}
}

func TestCopilotReadsABacklogADayAtATime(t *testing.T) {
	srv, filters := copilotFixture(t, map[string]bool{oidAda: true})
	a := newTestCopilot(t, srv, directory, fakeTokens{})
	_, next, more, err := pull(t, a, Connection{EntraTenantID: entraTID})
	if err != nil || !more || next != "2026-10-07T12:00:00Z" {
		t.Fatalf("next %q more %v err %v", next, more, err)
	}
	if (*filters)[0] != "createdDateTime gt 2026-10-06T12:00:00Z and createdDateTime lt 2026-10-07T12:00:00Z" {
		t.Fatalf("filter %q", (*filters)[0])
	}
}

func TestCopilotRefusedForEveryoneIsMissingConsent(t *testing.T) {
	srv, _ := copilotFixture(t, nil)
	_, _, _, err := pull(t, newTestCopilot(t, srv, directory, fakeTokens{}), Connection{EntraTenantID: entraTID})
	if failureCode(Microsoft365Copilot, err) != ErrConsentMissing {
		t.Fatalf("err %v", err)
	}
	_, _, _, err = pull(t, newTestCopilot(t, srv, directory, fakeTokens{err: errors.New("AADSTS65001")}), Connection{EntraTenantID: entraTID})
	if failureCode(Microsoft365Copilot, err) != ErrConsentMissing {
		t.Fatalf("token refused: %v", err)
	}
	people := fakePeople{err: &graphsync.GraphError{Status: http.StatusServiceUnavailable, Code: "serviceNotAvailable"}}
	_, _, _, err = pull(t, newTestCopilot(t, srv, people, fakeTokens{}), Connection{EntraTenantID: entraTID})
	if failureCode(Microsoft365Copilot, err) != ErrProviderFailed {
		t.Fatalf("directory unavailable: %v", err)
	}
}

func TestCopilotAttachmentsCombineForLinksAndCountEachFileOnce(t *testing.T) {
	var it aiInteraction
	const fixture = `{
		"attachments": [{"name": "Q3.xlsx", "contentType": "reference"}, {"contentType": "application/vnd.microsoft.card.adaptive"}],
		"links": [
			{"displayName": "Q3.xlsx", "linkType": "File"},
			{"displayName": "Plan.pdf", "linkType": "File"},
			{"displayName": "Q3 review", "linkType": "Meeting"}
		]
	}`
	if err := json.Unmarshal([]byte(fixture), &it); err != nil {
		t.Fatal(err)
	}
	atts := copilotAttachments(it)
	if len(atts) != 2 {
		t.Fatalf("%d attachments: %+v", len(atts), atts)
	}
	if atts[0].Name != "Q3.xlsx" || atts[0].MediaType != "reference" || atts[0].ContentDigest != canon.Unreadable {
		t.Fatalf("first attachment %+v", atts[0])
	}
	if atts[1].Name != "Plan.pdf" || atts[1].MediaType != "" || atts[1].ContentDigest != canon.Unreadable {
		t.Fatalf("second attachment %+v", atts[1])
	}
}

func TestCopilotFollowsNoLinkOffGraph(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"value":[],"@odata.nextLink":"https://attacker.example/steal"}`))
	}))
	defer srv.Close()
	a := newTestCopilot(t, srv, fakePeople{users: []graphsync.User{{ID: oidAda}}}, fakeTokens{})
	if _, _, _, err := pull(t, a, Connection{EntraTenantID: entraTID}); err == nil || !strings.Contains(err.Error(), "off") {
		t.Fatalf("err %v", err)
	}
}
