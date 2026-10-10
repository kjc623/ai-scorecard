package graphsync

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const entraTenant = "5ac3954b-f7e1-47af-bbab-2ca027d1948f"

type staticTokens struct {
	err   error
	asked []string
}

func (s *staticTokens) Token(_ context.Context, tid, scope string) (string, error) {
	s.asked = append(s.asked, tid+" "+scope)
	if s.err != nil {
		return "", s.err
	}
	return "graph-token", nil
}

func fakeGraph(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) (*Client, *staticTokens) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer graph-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	tokens := &staticTokens{}
	c, err := NewClient(tokens, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	c.BaseURL = srv.URL
	return c, tokens
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func TestUsersFollowsEveryPage(t *testing.T) {
	var base string
	c, tokens := fakeGraph(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1.0/users" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.URL.Query().Get("page") == "" {
			if sel := r.URL.Query().Get("$select"); !strings.Contains(sel, "onPremisesDistinguishedName") {
				t.Errorf("$select = %q", sel)
			}
			writeJSON(w, map[string]any{
				"value":           []any{map[string]any{"id": "a", "userPrincipalName": "a@contoso.com"}},
				"@odata.nextLink": base + "/v1.0/users?page=2",
			})
			return
		}
		writeJSON(w, map[string]any{"value": []any{map[string]any{"id": "b", "userPrincipalName": "b@contoso.com", "accountEnabled": false}}})
	})
	base = c.BaseURL
	var got []User
	if err := c.Users(context.Background(), entraTenant, func(u User) error { got = append(got, u); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[1].UserPrincipalName != "b@contoso.com" || got[1].AccountEnabled == nil || *got[1].AccountEnabled {
		t.Fatalf("users = %+v", got)
	}
	if tokens.asked[0] != entraTenant+" "+GraphScope {
		t.Fatalf("token asked for %v", tokens.asked)
	}
}

func TestANextLinkToAnotherHostIsRefused(t *testing.T) {
	c, _ := fakeGraph(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"value": []any{}, "@odata.nextLink": "https://attacker.example/v1.0/users?page=2"})
	})
	err := c.Users(context.Background(), entraTenant, func(User) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "left") {
		t.Fatalf("err = %v", err)
	}
}

func TestConsentAndTokenFailuresAreNamed(t *testing.T) {
	c, tokens := fakeGraph(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		writeJSON(w, map[string]any{"error": map[string]any{"code": "Authorization_RequestDenied", "message": "secret detail"}})
	})
	err := c.Users(context.Background(), entraTenant, func(User) error { return nil })
	if !IsConsent(err) || strings.Contains(err.Error(), "secret detail") {
		t.Fatalf("err = %v", err)
	}
	tokens.err = errors.New("AADSTS7000229")
	err = c.Users(context.Background(), entraTenant, func(User) error { return nil })
	var te *TokenError
	if !errors.As(err, &te) {
		t.Fatalf("err = %v, want a TokenError", err)
	}
	if err := c.Users(context.Background(), "contoso.com", func(User) error { return nil }); err == nil {
		t.Fatal("a tenant that is not a GUID was used")
	}
}

func TestGroupMembersAndSearch(t *testing.T) {
	c, _ := fakeGraph(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/transitiveMembers/microsoft.graph.user"):
			writeJSON(w, map[string]any{"value": []any{
				map[string]any{"id": "6F9619FF-8B86-D011-B42D-00C04FC964FF"},
				map[string]any{"id": "not-a-guid"},
			}})
		case r.URL.Path == "/v1.0/groups":
			if r.Header.Get("ConsistencyLevel") != "eventual" {
				t.Errorf("search without ConsistencyLevel")
			}
			if s := r.URL.Query().Get("$search"); s != `"displayName:sales team"` {
				t.Errorf("$search = %q", s)
			}
			writeJSON(w, map[string]any{"value": []any{
				map[string]any{"id": "2", "displayName": "sales west"},
				map[string]any{"id": "1", "displayName": "Sales East"},
			}})
		default:
			w.WriteHeader(http.StatusNotFound)
			writeJSON(w, map[string]any{"error": map[string]any{"code": "Request_ResourceNotFound"}})
		}
	})
	ids, err := c.GroupMembers(context.Background(), entraTenant, "26118915-6090-4610-87a4-49d0767e9fde")
	if err != nil || len(ids) != 1 || ids[0] != "6f9619ff-8b86-d011-b42d-00c04fc964ff" {
		t.Fatalf("members = %v, %v", ids, err)
	}
	if _, err := c.GroupMembers(context.Background(), entraTenant, "../users"); err == nil {
		t.Fatal("a group id that is not a GUID reached the URL")
	}
	found, err := c.SearchGroups(context.Background(), entraTenant, `sales "team`)
	if err != nil || len(found) != 2 || found[0].DisplayName != "Sales East" {
		t.Fatalf("search = %+v, %v", found, err)
	}
	if _, err := c.Group(context.Background(), entraTenant, "26118915-6090-4610-87a4-49d0767e9fde"); !IsNotFound(err) {
		t.Fatalf("group err = %v, want not found", err)
	}
}

func TestOrgUnit(t *testing.T) {
	for dn, want := range map[string]string{
		"CN=Ada Lovelace,OU=Sales,DC=contoso,DC=com":    "OU=Sales,DC=contoso,DC=com",
		`CN=Lovelace\, Ada,OU=Sales,DC=contoso,DC=com`:  "OU=Sales,DC=contoso,DC=com",
		"CN=Ada, OU=Research,OU=Labs,DC=contoso,DC=com": "OU=Research,OU=Labs,DC=contoso,DC=com",
		"CN=Ada":   "",
		"":         "",
		`CN=a\\,b`: "b",
	} {
		if got := OrgUnit(dn); got != want {
			t.Errorf("OrgUnit(%q) = %q, want %q", dn, got, want)
		}
	}
}
