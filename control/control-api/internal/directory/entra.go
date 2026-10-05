package directory

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// GraphSource reads users from Microsoft Entra ID through Microsoft Graph. It authenticates as a
// registered application with the client-credentials grant; the customer registers the application,
// grants it User.Read.All (application), and the sync holds only that application's secret.
//
// The mapping from the endpoint's user reference to a directory identity is a chosen attribute:
// UserRefAttribute names the directory field whose value equals the device's configured `--user-ref`
// (default `onPremisesSamAccountName`). The attribute is added to the Graph `$select` so it actually
// comes back, which is why it is a setting and not a constant. A directory user whose chosen
// attribute is empty cannot be joined to the wire and is counted as skipped, not written with a
// blank key.
//
// Only the attributes ops.user_dim holds are read: the object id, display name, department, the user
// reference attribute, an optional population attribute, and accountEnabled for the status. Manager
// is not read: whether manager_ref may be used is still open (docs/risks/Q2-organisational-dimension
// .md, closing note 3), so this provider leaves it NULL rather than fetching a reporting line the
// product has not been cleared to keep.
type GraphSource struct {
	TenantID     string
	ClientID     string
	ClientSecret string
	// UserRefAttribute is the directory field whose value is the device's user reference.
	UserRefAttribute string
	// PopulationAttribute, when set, names a directory field copied into ops.user_dim.population.
	PopulationAttribute string
	// LoginBaseURL and GraphBaseURL exist so a test can point both at one httptest server; empty
	// means the public Microsoft endpoints.
	LoginBaseURL string
	GraphBaseURL string
	HTTPClient   *http.Client
}

// Name implements Source.
func (s *GraphSource) Name() string { return "entra" }

func (s *GraphSource) loginBase() string {
	if s.LoginBaseURL != "" {
		return strings.TrimRight(s.LoginBaseURL, "/")
	}
	return "https://login.microsoftonline.com"
}

func (s *GraphSource) graphBase() string {
	if s.GraphBaseURL != "" {
		return strings.TrimRight(s.GraphBaseURL, "/")
	}
	return "https://graph.microsoft.com/v1.0"
}

func (s *GraphSource) client() *http.Client {
	if s.HTTPClient != nil {
		return s.HTTPClient
	}
	return &http.Client{Timeout: 60 * time.Second}
}

func (s *GraphSource) userRefAttribute() string {
	if s.UserRefAttribute != "" {
		return s.UserRefAttribute
	}
	return "onPremisesSamAccountName"
}

// accessToken redeems the application's client credentials for a Graph token. The secret appears in
// the request body and nowhere else; the error never includes the response body, which can echo it.
func (s *GraphSource) accessToken(ctx context.Context) (string, error) {
	if s.TenantID == "" || s.ClientID == "" || s.ClientSecret == "" {
		return "", fmt.Errorf("directory: entra needs a tenant id, a client id and a client secret")
	}
	form := url.Values{
		"client_id":     {s.ClientID},
		"client_secret": {s.ClientSecret},
		"scope":         {"https://graph.microsoft.com/.default"},
		"grant_type":    {"client_credentials"},
	}
	endpoint := fmt.Sprintf("%s/%s/oauth2/v2.0/token", s.loginBase(), url.PathEscape(s.TenantID))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("directory: build token request: %w", err)
	}
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	res, err := s.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("directory: token request: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("directory: token endpoint answered %d", res.StatusCode)
	}
	var body struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&body); err != nil {
		return "", fmt.Errorf("directory: decode token response: %w", err)
	}
	if body.AccessToken == "" {
		return "", fmt.Errorf("directory: token response carried no access_token")
	}
	return body.AccessToken, nil
}

// List implements Source. It follows Microsoft's own @odata.nextLink paging so a directory larger
// than one page is read completely; an error part-way is returned as an error, never as a short
// list, because the synchroniser retires every row a short list omits.
func (s *GraphSource) List(ctx context.Context) ([]User, error) {
	token, err := s.accessToken(ctx)
	if err != nil {
		return nil, err
	}
	selects := []string{"id", "displayName", "department", "accountEnabled", s.userRefAttribute()}
	if s.PopulationAttribute != "" {
		selects = append(selects, s.PopulationAttribute)
	}
	// de-duplicate while preserving order, so a population attribute equal to the user-ref
	// attribute does not repeat in the query.
	seen := map[string]bool{}
	unique := selects[:0]
	for _, name := range selects {
		if name != "" && !seen[name] {
			seen[name] = true
			unique = append(unique, name)
		}
	}
	q := url.Values{"$select": {strings.Join(unique, ",")}, "$top": {"999"}}
	next := fmt.Sprintf("%s/users?%s", s.graphBase(), q.Encode())

	var users []User
	for page := 0; next != ""; page++ {
		if page > 1000 {
			return nil, fmt.Errorf("directory: graph paging did not terminate after 1000 pages")
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, next, nil)
		if err != nil {
			return nil, fmt.Errorf("directory: build users request: %w", err)
		}
		req.Header.Set("authorization", "Bearer "+token)
		res, err := s.client().Do(req)
		if err != nil {
			return nil, fmt.Errorf("directory: users request: %w", err)
		}
		body, readErr := io.ReadAll(io.LimitReader(res.Body, 16<<20))
		res.Body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("directory: read users response: %w", readErr)
		}
		if res.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("directory: graph users answered %d: %s", res.StatusCode, graphError(body))
		}
		var doc struct {
			Value    []map[string]any `json:"value"`
			NextLink string           `json:"@odata.nextLink"`
		}
		if err := json.Unmarshal(body, &doc); err != nil {
			return nil, fmt.Errorf("directory: decode users response: %w", err)
		}
		for _, obj := range doc.Value {
			users = append(users, s.user(obj))
		}
		next = doc.NextLink
	}
	return users, nil
}

func (s *GraphSource) user(obj map[string]any) User {
	status := StatusActive
	if enabled, ok := obj["accountEnabled"].(bool); ok && !enabled {
		status = StatusInactive
	}
	return User{
		UserRef:     field(obj, s.userRefAttribute()),
		DirectoryID: field(obj, "id"),
		DisplayName: field(obj, "displayName"),
		Department:  field(obj, "department"),
		Population:  field(obj, s.PopulationAttribute),
		Status:      status,
	}
}

// field reads a possibly dotted path from a Graph object and returns it as a trimmed string. A
// missing or non-string value is empty, never the Go representation of it.
func field(obj map[string]any, path string) string {
	if path == "" {
		return ""
	}
	parts := strings.Split(path, ".")
	var cur any = obj
	for _, part := range parts {
		m, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur, ok = m[part]
		if !ok {
			return ""
		}
	}
	if s, ok := cur.(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

// graphError extracts Graph's error message without echoing a body that could carry tenant data.
func graphError(body []byte) string {
	var doc struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &doc); err == nil && doc.Error.Code != "" {
		return doc.Error.Code
	}
	return "unrecognised error body"
}
