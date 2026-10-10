package graphsync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// GraphScope is the scope an app-only Graph token is requested for.
const GraphScope = "https://graph.microsoft.com/.default"

// DefaultGraphBaseURL is Microsoft Graph's global endpoint.
const DefaultGraphBaseURL = "https://graph.microsoft.com"

// TokenSource obtains an app-only access token for the vendor's multi-tenant app in a customer's
// Entra tenant. *entraapp.App implements it.
type TokenSource interface {
	Token(ctx context.Context, customerTenantID, scope string) (string, error)
}

// User is the slice of Graph's user the pull reads.
type User struct {
	ID                          string `json:"id"`
	UserPrincipalName           string `json:"userPrincipalName"`
	DisplayName                 string `json:"displayName"`
	Department                  string `json:"department"`
	AccountEnabled              *bool  `json:"accountEnabled"`
	UserType                    string `json:"userType"`
	OnPremisesDistinguishedName string `json:"onPremisesDistinguishedName"`
}

// Group is the slice of Graph's group the console offers for import.
type Group struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	Description string `json:"description"`
}

// GraphError is a Graph answer other than success. Code is Graph's machine-readable error code,
// the only part of an error body safe to log.
type GraphError struct {
	Status int
	Code   string
}

func (e *GraphError) Error() string {
	return fmt.Sprintf("graph answered %d (%s)", e.Status, e.Code)
}

// TokenError is the customer's tenant refusing the app a token: the app is not consented there,
// or the tenant id is wrong.
type TokenError struct{ Err error }

func (e *TokenError) Error() string { return "graphsync: graph token: " + e.Err.Error() }
func (e *TokenError) Unwrap() error { return e.Err }

// IsNotFound reports whether err is Graph's 404.
func IsNotFound(err error) bool {
	var g *GraphError
	return errors.As(err, &g) && g.Status == http.StatusNotFound
}

// IsConsent reports whether err means the customer has not granted the permission the call needs.
func IsConsent(err error) bool {
	var g *GraphError
	return errors.As(err, &g) && (g.Status == http.StatusUnauthorized || g.Status == http.StatusForbidden)
}

// Client reads users and groups from a customer's directory with the application permissions
// User.Read.All and GroupMember.Read.All.
type Client struct {
	Tokens TokenSource
	HTTP   *http.Client
	// BaseURL is DefaultGraphBaseURL; tests point it at a fake.
	BaseURL string
}

// NewClient builds the Graph client. A nil HTTP client gets a bounded default.
func NewClient(tokens TokenSource, client *http.Client) (*Client, error) {
	if tokens == nil {
		return nil, errors.New("graphsync: a token source is required")
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{Tokens: tokens, HTTP: client, BaseURL: DefaultGraphBaseURL}, nil
}

// maxPages bounds one listing, so a paging loop that never ends cannot hold a sync forever: 500
// pages of 999 is half a million objects.
const maxPages = 500

const userSelect = "id,userPrincipalName,displayName,department,accountEnabled,userType,onPremisesDistinguishedName"

// Users calls fn for every user in the customer's directory, a page at a time.
func (c *Client) Users(ctx context.Context, entraTenantID string, fn func(User) error) error {
	q := url.Values{"$select": {userSelect}, "$top": {"999"}}
	return pages(ctx, c, entraTenantID, "/v1.0/users?"+q.Encode(), nil, func(raw json.RawMessage) error {
		var u User
		if err := json.Unmarshal(raw, &u); err != nil {
			return fmt.Errorf("graphsync: a user is not a Graph user: %w", err)
		}
		return fn(u)
	})
}

// GroupMembers returns the object ids of the users in a group, through nested groups.
func (c *Client) GroupMembers(ctx context.Context, entraTenantID, groupID string) ([]string, error) {
	if !isGUID(groupID) {
		return nil, fmt.Errorf("graphsync: group id %q is not a GUID", groupID)
	}
	q := url.Values{"$select": {"id"}, "$top": {"999"}}
	var ids []string
	err := pages(ctx, c, entraTenantID, "/v1.0/groups/"+groupID+"/transitiveMembers/microsoft.graph.user?"+q.Encode(), nil, func(raw json.RawMessage) error {
		var m struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(raw, &m); err != nil {
			return fmt.Errorf("graphsync: a member is not a Graph object: %w", err)
		}
		if isGUID(strings.ToLower(m.ID)) {
			ids = append(ids, strings.ToLower(m.ID))
		}
		return nil
	})
	return ids, err
}

// Group reads one group.
func (c *Client) Group(ctx context.Context, entraTenantID, groupID string) (Group, error) {
	if !isGUID(groupID) {
		return Group{}, fmt.Errorf("graphsync: group id %q is not a GUID", groupID)
	}
	q := url.Values{"$select": {"id,displayName,description"}}
	var g Group
	err := c.get(ctx, entraTenantID, "/v1.0/groups/"+groupID+"?"+q.Encode(), nil, &g)
	return g, err
}

// maxSearchResults bounds a group search: the console shows a short list to pick from.
const maxSearchResults = 25

// SearchGroups returns groups whose name contains every word of query. $search is an advanced
// query, so the request carries ConsistencyLevel: eventual.
func (c *Client) SearchGroups(ctx context.Context, entraTenantID, query string) ([]Group, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return []Group{}, nil
	}
	// Graph's $search syntax quotes the clause; a quote in the text would end it early.
	query = strings.ReplaceAll(query, `"`, "")
	q := url.Values{
		"$search": {`"displayName:` + query + `"`},
		"$select": {"id,displayName,description"},
		"$top":    {fmt.Sprint(maxSearchResults)},
	}
	var body struct {
		Value []Group `json:"value"`
	}
	if err := c.get(ctx, entraTenantID, "/v1.0/groups?"+q.Encode(), http.Header{"ConsistencyLevel": {"eventual"}}, &body); err != nil {
		return nil, err
	}
	if body.Value == nil {
		body.Value = []Group{}
	}
	sort.Slice(body.Value, func(i, j int) bool {
		return strings.ToLower(body.Value[i].DisplayName) < strings.ToLower(body.Value[j].DisplayName)
	})
	return body.Value, nil
}

// pages follows @odata.nextLink from path, calling fn for each value.
func pages(ctx context.Context, c *Client, tid, path string, header http.Header, fn func(json.RawMessage) error) error {
	next := c.base() + path
	for page := 0; next != ""; page++ {
		if page == maxPages {
			return fmt.Errorf("graphsync: more than %d pages", maxPages)
		}
		// A nextLink is followed only on the Graph host the first request went to, so a response
		// cannot send the token elsewhere.
		if !strings.HasPrefix(next, c.base()+"/") {
			return fmt.Errorf("graphsync: a next page link left %s", c.base())
		}
		var body struct {
			Value    []json.RawMessage `json:"value"`
			NextLink string            `json:"@odata.nextLink"`
		}
		if err := c.getURL(ctx, tid, next, header, &body); err != nil {
			return err
		}
		for _, v := range body.Value {
			if err := fn(v); err != nil {
				return err
			}
		}
		next = body.NextLink
	}
	return nil
}

func (c *Client) base() string {
	b := strings.TrimRight(c.BaseURL, "/")
	if b == "" {
		b = DefaultGraphBaseURL
	}
	return b
}

func (c *Client) get(ctx context.Context, tid, path string, header http.Header, out any) error {
	return c.getURL(ctx, tid, c.base()+path, header, out)
}

func (c *Client) getURL(ctx context.Context, tid, endpoint string, header http.Header, out any) error {
	if !isGUID(strings.ToLower(tid)) {
		return fmt.Errorf("graphsync: the Entra tenant id %q is not a GUID", tid)
	}
	token, err := c.Tokens.Token(ctx, tid, GraphScope)
	if err != nil {
		return &TokenError{err}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("graphsync: build request: %w", err)
	}
	for k, v := range header {
		req.Header[k] = v
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("graphsync: graph request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return fmt.Errorf("graphsync: read graph response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return &GraphError{Status: resp.StatusCode, Code: graphErrorCode(body)}
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("graphsync: graph response is not JSON: %w", err)
	}
	return nil
}

func graphErrorCode(body []byte) string {
	var e struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil && e.Error.Code != "" {
		return e.Error.Code
	}
	return "no error code"
}

func isGUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range []byte(s) {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
				return false
			}
		}
	}
	return true
}
