package providers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/html"

	"github.com/shadow-ai-capture/device/protocol/canon"

	"github.com/shadow-ai-capture/control-api/internal/graphsync"
)

// Bounds on one Copilot window: how far forward one pass reads, and how far back over the last
// window it starts again, since an interaction can be listed some time after it happened.
const (
	copilotMaxWindow = 24 * time.Hour
	copilotOverlap   = 30 * time.Minute
)

// TokenSource obtains an app-only access token for the vendor's Entra application in a customer's
// Entra tenant. *entraapp.App implements it.
type TokenSource interface {
	Token(ctx context.Context, customerTenantID, scope string) (string, error)
}

// People lists the people of a customer's Entra directory. *graphsync.Client implements it.
type People interface {
	Users(ctx context.Context, entraTenantID string, fn func(graphsync.User) error) error
}

// Copilot reads Microsoft 365 Copilot's interaction export from Microsoft Graph: the prompts each
// person sent Copilot in Microsoft 365 apps and Copilot Chat, a person at a time, since Graph has no
// tenant-wide listing. It reads with the vendor's Entra application and the application permission
// AiEnterpriseInteraction.Read.All, consented in the customer's tenant. Only a person with a
// Microsoft 365 Copilot licence has interactions; Graph refuses the others, and they are passed
// over. The cursor is the end of the last window read.
type Copilot struct {
	Tokens TokenSource
	People People
	HTTP   *http.Client
	// BaseURL is graphsync.DefaultGraphBaseURL; tests point it at a fake.
	BaseURL string
	Now     func() time.Time
}

// NewCopilot builds the adapter. A nil HTTP client gets a bounded default.
func NewCopilot(tokens TokenSource, people People, client *http.Client) (*Copilot, error) {
	if tokens == nil || people == nil {
		return nil, errors.New("providers: Copilot needs a token source and the directory's people")
	}
	if client == nil {
		client = newHTTPClient()
	}
	return &Copilot{Tokens: tokens, People: people, HTTP: client, BaseURL: graphsync.DefaultGraphBaseURL, Now: time.Now}, nil
}

// aiInteraction is the slice of Graph's aiInteraction the adapter reads.
type aiInteraction struct {
	ID              string `json:"id"`
	InteractionType string `json:"interactionType"`
	CreatedDateTime string `json:"createdDateTime"`
	Body            struct {
		ContentType string `json:"contentType"`
		Content     string `json:"content"`
	} `json:"body"`
	Attachments []struct {
		Name        string `json:"name"`
		ContentType string `json:"contentType"`
	} `json:"attachments"`
	// Links are the resources an interaction references: a file the prompt named (linkType "File")
	// as well as meetings, events and messages, which are not attachments.
	Links []struct {
		DisplayName string `json:"displayName"`
		LinkType    string `json:"linkType"`
	} `json:"links"`
}

// Pull implements Source.
func (a *Copilot) Pull(ctx context.Context, c Connection, emit func(Record) error) (string, bool, error) {
	now := a.now().UTC()
	start := parseTime(c.Cursor)
	if start.IsZero() {
		start = now.Add(-InitialLookback)
	} else {
		start = start.Add(-copilotOverlap)
	}
	end := now
	more := false
	if end.Sub(start) > copilotMaxWindow {
		end, more = start.Add(copilotMaxWindow), true
	}
	read, refused := 0, 0
	err := a.People.Users(ctx, c.EntraTenantID, func(u graphsync.User) error {
		oid := strings.ToLower(strings.TrimSpace(u.ID))
		if oid == "" || strings.EqualFold(u.UserType, "Guest") {
			return nil
		}
		err := a.person(ctx, c.EntraTenantID, oid, strings.TrimSpace(u.UserPrincipalName), start, end, emit)
		var ae *APIError
		if errors.As(err, &ae) && (ae.Status == http.StatusForbidden || ae.Status == http.StatusNotFound) {
			refused++
			return nil
		}
		if err == nil {
			read++
		}
		return err
	})
	if err != nil {
		return "", false, graphFailure(err)
	}
	// Graph refusing every person is the application's permission, not their licences.
	if read == 0 && refused > 0 {
		return "", false, &APIError{Status: http.StatusForbidden, Code: "no person's interactions could be read"}
	}
	return end.Format(time.RFC3339Nano), more, nil
}

// person emits one person's prompts created in [start, end).
func (a *Copilot) person(ctx context.Context, tid, oid, upn string, start, end time.Time, emit func(Record) error) error {
	q := url.Values{
		"$filter": {fmt.Sprintf("createdDateTime gt %s and createdDateTime lt %s",
			start.Format(time.RFC3339Nano), end.Format(time.RFC3339Nano))},
		"$top": {"100"},
	}
	base := strings.TrimRight(a.BaseURL, "/")
	next := base + "/v1.0/copilot/users/" + url.PathEscape(oid) + "/interactionHistory/getAllEnterpriseInteractions?" + q.Encode()
	for page := 0; next != ""; page++ {
		if page == maxPages || !sameOrigin(base, next) {
			return fmt.Errorf("providers: Graph paged past %d pages or off %s", maxPages, base)
		}
		token, err := a.Tokens.Token(ctx, tid, graphsync.GraphScope)
		if err != nil {
			return &TokenError{err}
		}
		var body struct {
			Value    []aiInteraction `json:"value"`
			NextLink string          `json:"@odata.nextLink"`
		}
		if err := get(ctx, a.HTTP, next, http.Header{"Authorization": {"Bearer " + token}, "Accept": {"application/json"}}, &body); err != nil {
			return err
		}
		for _, it := range body.Value {
			if it.InteractionType != "userPrompt" {
				continue
			}
			text := it.Body.Content
			if strings.EqualFold(it.Body.ContentType, "html") {
				text = htmlText(text)
			}
			// An interaction's id is unique within the person's history.
			if err := emit(Record{ID: oid + "/" + it.ID, OccurredAt: parseTime(it.CreatedDateTime), App: "m365_copilot",
				UPN: upn, ObjectID: oid, Text: text, Attachments: copilotAttachments(it)}); err != nil {
				return err
			}
		}
		next = body.NextLink
	}
	return nil
}

// copilotAttachments is the files the interaction sent or referenced: its attachments, and its
// links to files. Graph names each file but not its bytes, so every digest is unreadable. A file
// Graph lists both ways is one attachment; a link to anything but a file is not one.
func copilotAttachments(it aiInteraction) []canon.Attachment {
	var atts []canon.Attachment
	seen := make(map[string]bool)
	add := func(name, mediaType string) {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		atts = append(atts, canon.Attachment{Name: name, MediaType: mediaType, ContentDigest: canon.Unreadable})
	}
	for _, f := range it.Attachments {
		add(f.Name, f.ContentType)
	}
	for _, l := range it.Links {
		if strings.EqualFold(strings.TrimSpace(l.LinkType), "File") {
			add(l.DisplayName, "")
		}
	}
	return atts
}

// graphFailure is a failure of the people listing as the pass reports it.
func graphFailure(err error) error {
	var t *graphsync.TokenError
	var g *graphsync.GraphError
	switch {
	case errors.As(err, &t):
		return &TokenError{err}
	case errors.As(err, &g):
		return &APIError{Status: g.Status, Code: g.Code}
	}
	return err
}

// htmlText is the text of an HTML body, a line break for each block element.
func htmlText(s string) string {
	z := html.NewTokenizer(strings.NewReader(s))
	var b strings.Builder
	for {
		switch z.Next() {
		case html.ErrorToken:
			return b.String()
		case html.TextToken:
			b.Write(z.Text())
		case html.StartTagToken, html.EndTagToken, html.SelfClosingTagToken:
			name, _ := z.TagName()
			switch string(name) {
			case "br", "p", "div", "li", "tr", "h1", "h2", "h3", "h4", "h5", "h6":
				b.WriteByte('\n')
			}
		}
	}
}

func (a *Copilot) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}
