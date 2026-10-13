package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/shadow-ai-capture/device/protocol/canon"
)

// DefaultClaudeBaseURL is Anthropic's API host, which serves the Compliance API.
const DefaultClaudeBaseURL = "https://api.anthropic.com"

// anthropicVersion is the API version the requests are written to.
const anthropicVersion = "2023-06-01"

// claudeChatPagesPerPass bounds one Claude pass's walk of chats, in listing pages of up to 100.
const claudeChatPagesPerPass = 10

// claudeOverlap is how far before the latest update read the next walk starts: a chat or session
// becomes listable a few minutes after it changes, so one updated just before the last read may
// appear only later. A record read twice is recorded once.
const claudeOverlap = 10 * time.Minute

// Claude reads a Claude Enterprise organization's prompts from Anthropic's Compliance API: the
// organization's chats (claude.ai, the desktop and mobile apps), and its local sessions (Claude Code
// and Cowork), each walked by last update. A chat or session updated since the walk's start is read
// again whole, and only the messages written since then are emitted. The cursor holds where each
// walk resumes. The key is a Compliance Access Key with read:compliance_user_data; the account is the
// organization's id, which narrows both walks to it.
type Claude struct {
	HTTP *http.Client
	// BaseURL is DefaultClaudeBaseURL; tests point it at a fake.
	BaseURL string
	Now     func() time.Time
}

// NewClaude builds the adapter. A nil HTTP client gets a bounded default.
func NewClaude(client *http.Client) *Claude {
	if client == nil {
		client = newHTTPClient()
	}
	return &Claude{HTTP: client, BaseURL: DefaultClaudeBaseURL, Now: time.Now}
}

// claudeCursor is where each walk resumes: the updated_at it lists from.
type claudeCursor struct {
	Chats    string `json:"chats"`
	Sessions string `json:"sessions"`
}

type claudeUser struct {
	EmailAddress string `json:"email_address"`
}

// claudeBlock is one content block; only text blocks carry what a person wrote.
type claudeBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type claudeFile struct {
	Filename  string `json:"filename"`
	MimeType  string `json:"mime_type"`
	SizeBytes int64  `json:"size_bytes"`
}

type claudeMessage struct {
	ID        string        `json:"id"`
	Role      string        `json:"role"`
	CreatedAt string        `json:"created_at"`
	Content   []claudeBlock `json:"content"`
	Files     []claudeFile  `json:"files"`
}

// Pull implements Source.
func (a *Claude) Pull(ctx context.Context, c Connection, emit func(Record) error) (string, bool, error) {
	var cur claudeCursor
	if c.Cursor != "" {
		_ = json.Unmarshal([]byte(c.Cursor), &cur)
	}
	start := a.now().UTC().Add(-InitialLookback).Format(time.RFC3339)
	if cur.Chats == "" {
		cur.Chats = start
	}
	if cur.Sessions == "" {
		cur.Sessions = start
	}
	header := http.Header{"X-Api-Key": {c.Key}, "Anthropic-Version": {anthropicVersion}, "Accept": {"application/json"}}

	chats, moreChats, err := a.chats(ctx, c, header, cur.Chats, emit)
	if chats != "" {
		cur.Chats = chats
	}
	if err != nil {
		return a.encode(cur), false, err
	}
	sessions, moreSessions, err := a.sessions(ctx, c, header, cur.Sessions, emit)
	if sessions != "" {
		cur.Sessions = sessions
	}
	return a.encode(cur), moreChats || moreSessions, err
}

func (a *Claude) encode(cur claudeCursor) string {
	b, _ := json.Marshal(cur)
	return string(b)
}

// chats walks the organization's chats updated since from, oldest update first, and emits the
// messages a person wrote in each. It returns where the next walk starts.
func (a *Claude) chats(ctx context.Context, c Connection, header http.Header, from string, emit func(Record) error) (string, bool, error) {
	base := a.base() + "/v1/compliance/apps/chats"
	q := url.Values{"order_by": {"updated_at"}, "updated_at.gte": {from}, "limit": {"100"}}
	if c.Account != "" {
		q.Set("organization_ids[]", c.Account)
	}
	since, next := parseTime(from), ""
	for page := 0; page < claudeChatPagesPerPass; page++ {
		var list struct {
			Data []struct {
				ID        string     `json:"id"`
				UpdatedAt string     `json:"updated_at"`
				User      claudeUser `json:"user"`
			} `json:"data"`
			HasMore bool   `json:"has_more"`
			LastID  string `json:"last_id"`
		}
		if err := get(ctx, a.HTTP, base+"?"+q.Encode(), header, &list); err != nil {
			return next, false, err
		}
		for _, chat := range list.Data {
			if err := a.chatMessages(ctx, header, base+"/"+url.PathEscape(chat.ID)+"/messages", chat.User.EmailAddress, since, emit); err != nil {
				return next, false, err
			}
			next = resumeFrom(next, chat.UpdatedAt)
		}
		if !list.HasMore || list.LastID == "" {
			return next, false, nil
		}
		q.Set("after_id", list.LastID)
	}
	return next, true, nil
}

func (a *Claude) chatMessages(ctx context.Context, header http.Header, endpoint, email string, since time.Time, emit func(Record) error) error {
	q := url.Values{"limit": {"1000"}}
	for page := 0; page < maxPages; page++ {
		var body struct {
			Messages []claudeMessage `json:"chat_messages"`
			HasMore  bool            `json:"has_more"`
			LastID   string          `json:"last_id"`
		}
		if err := get(ctx, a.HTTP, endpoint+"?"+q.Encode(), header, &body); err != nil {
			return err
		}
		for _, m := range body.Messages {
			if err := emitClaude(m, "claude_web", email, since, emit); err != nil {
				return err
			}
		}
		if !body.HasMore || body.LastID == "" {
			return nil
		}
		q.Set("after_id", body.LastID)
	}
	return nil
}

// sessions walks the organization's local sessions updated since from and emits the messages a
// person wrote in each. The listing's order is not documented, and its page tokens last a day, so a
// walk reads every page before it moves its cursor.
func (a *Claude) sessions(ctx context.Context, c Connection, header http.Header, from string, emit func(Record) error) (string, bool, error) {
	base := a.base() + "/v1/compliance/apps/sessions/local"
	q := url.Values{"updated_at.gte": {from}, "limit": {"500"}}
	since, next := parseTime(from), ""
	for page := 0; page < maxPages; page++ {
		var list struct {
			Data []struct {
				ID               string     `json:"id"`
				OrganizationUUID string     `json:"organization_uuid"`
				ProductSurface   string     `json:"product_surface"`
				UpdatedAt        string     `json:"updated_at"`
				User             claudeUser `json:"user"`
			} `json:"data"`
			NextPage *string `json:"next_page"`
		}
		if err := get(ctx, a.HTTP, base+"?"+q.Encode(), header, &list); err != nil {
			return "", false, err
		}
		for _, s := range list.Data {
			if c.Account != "" && s.OrganizationUUID != "" && !strings.EqualFold(s.OrganizationUUID, c.Account) {
				continue
			}
			if err := a.sessionMessages(ctx, header, base+"/"+url.PathEscape(s.ID)+"/messages", claudeApp(s.ProductSurface), s.User.EmailAddress, since, emit); err != nil {
				return "", false, err
			}
			next = resumeFrom(next, s.UpdatedAt)
		}
		if list.NextPage == nil || *list.NextPage == "" {
			return next, false, nil
		}
		q.Set("page", *list.NextPage)
	}
	return next, false, nil
}

func (a *Claude) sessionMessages(ctx context.Context, header http.Header, endpoint, app, email string, since time.Time, emit func(Record) error) error {
	q := url.Values{"limit": {"1000"}}
	for page := 0; page < maxPages; page++ {
		var body struct {
			Data     []claudeMessage `json:"data"`
			NextPage *string         `json:"next_page"`
		}
		if err := get(ctx, a.HTTP, endpoint+"?"+q.Encode(), header, &body); err != nil {
			return err
		}
		for _, m := range body.Data {
			if err := emitClaude(m, app, email, since, emit); err != nil {
				return err
			}
		}
		if body.NextPage == nil || *body.NextPage == "" {
			return nil
		}
		q.Set("page", *body.NextPage)
	}
	return nil
}

// emitClaude emits a message a person wrote at or after since: its text blocks, and the files
// attached to it. A message of a person's that holds only tool results is the tool's, not theirs.
func emitClaude(m claudeMessage, app, email string, since time.Time, emit func(Record) error) error {
	at := parseTime(m.CreatedAt)
	if m.Role != "user" || at.Before(since) {
		return nil
	}
	var parts []string
	for _, b := range m.Content {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	var atts []canon.Attachment
	for _, f := range m.Files {
		atts = append(atts, canon.Attachment{Name: f.Filename, MediaType: f.MimeType, SizeBytes: f.SizeBytes, ContentDigest: canon.Unreadable})
	}
	if len(parts) == 0 && len(atts) == 0 {
		return nil
	}
	return emit(Record{ID: m.ID, OccurredAt: at, App: app, UPN: email,
		Text: strings.Join(parts, "\n"), Attachments: atts})
}

// claudeApp is the catalogued tool a local session's surface is: Claude Code, Cowork in Claude
// Desktop, and Claude otherwise.
func claudeApp(surface string) string {
	switch surface {
	case "claude_code":
		return "claude_code"
	case "cowork":
		return "claude_desktop"
	}
	return "claude_web"
}

// resumeFrom is the later of cur and claudeOverlap before updatedAt, as an RFC 3339 time.
func resumeFrom(cur, updatedAt string) string {
	t := parseTime(updatedAt)
	if t.IsZero() {
		return cur
	}
	t = t.Add(-claudeOverlap).UTC()
	if c := parseTime(cur); !c.IsZero() && !t.After(c) {
		return cur
	}
	return t.Format(time.RFC3339Nano)
}

func (a *Claude) base() string { return strings.TrimRight(a.BaseURL, "/") }

func (a *Claude) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}
