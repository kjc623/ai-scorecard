package providers

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultChatGPTBaseURL is OpenAI's Compliance API host for ChatGPT Enterprise and Edu.
const DefaultChatGPTBaseURL = "https://api.chatgpt.com"

// chatgptPagesPerPass bounds one ChatGPT pass, in listing pages of up to 100 log files.
const chatgptPagesPerPass = 20

// ChatGPT reads a ChatGPT Enterprise workspace's conversation messages from OpenAI's Compliance
// Logs Platform: the workspace's CONVERSATION_MESSAGE log files, each an immutable, time-windowed
// JSONL file of message events, listed oldest first. The cursor is the end of the last window read
// (the listing's last_end_time). The key is a workspace Admin key with the conversation messages
// permission; the account is the workspace id. The platform keeps files for 30 days.
type ChatGPT struct {
	HTTP *http.Client
	// BaseURL is DefaultChatGPTBaseURL; tests point it at a fake.
	BaseURL string
	Now     func() time.Time
}

// NewChatGPT builds the adapter. A nil HTTP client gets a bounded default.
func NewChatGPT(client *http.Client) *ChatGPT {
	if client == nil {
		client = newHTTPClient()
	}
	return &ChatGPT{HTTP: client, BaseURL: DefaultChatGPTBaseURL, Now: time.Now}
}

// chatgptEvent is the slice of a CONVERSATION_MESSAGE event the adapter reads.
type chatgptEvent struct {
	EventID   string `json:"event_id"`
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
	Actor     struct {
		UserEmail string `json:"user_email"`
	} `json:"actor"`
	Message struct {
		ID        string `json:"id"`
		CreatedAt string `json:"created_at"`
		Author    struct {
			Type string `json:"type"`
		} `json:"author"`
		Content struct {
			Value *string `json:"value"`
		} `json:"content"`
	} `json:"message"`
}

// Pull implements Source.
func (g *ChatGPT) Pull(ctx context.Context, c Connection, emit func(Record) error) (string, bool, error) {
	after := c.Cursor
	if after == "" {
		after = g.now().UTC().Add(-InitialLookback).Format(time.RFC3339)
	}
	base := strings.TrimRight(g.BaseURL, "/") + "/v1/compliance/workspaces/" + url.PathEscape(c.Account) + "/logs"
	header := http.Header{"Authorization": {"Bearer " + c.Key}}
	cursor := ""
	for page := 0; page < chatgptPagesPerPass; page++ {
		q := url.Values{"event_type": {"CONVERSATION_MESSAGE"}, "after": {after}, "limit": {"100"}}
		var list struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
			HasMore     bool   `json:"has_more"`
			LastEndTime string `json:"last_end_time"`
		}
		if err := get(ctx, g.HTTP, base+"?"+q.Encode(), header, &list); err != nil {
			return cursor, false, err
		}
		for _, f := range list.Data {
			// The file is a redirect to storage; the client drops the key when it leaves the host.
			body, err := fetch(ctx, g.HTTP, base+"/"+url.PathEscape(f.ID), header)
			if err != nil {
				return cursor, false, err
			}
			if err := chatgptRecords(body, emit); err != nil {
				return cursor, false, err
			}
		}
		if list.LastEndTime != "" {
			after, cursor = list.LastEndTime, list.LastEndTime
		}
		if !list.HasMore {
			return cursor, false, nil
		}
	}
	return cursor, true, nil
}

// chatgptRecords emits the prompts of one log file: the messages a person wrote. A line that is
// not an event, or is another event type, an assistant's message or a message with no text, is
// passed over.
func chatgptRecords(file []byte, emit func(Record) error) error {
	sc := bufio.NewScanner(bytes.NewReader(file))
	sc.Buffer(make([]byte, 0, 64<<10), maxResponseBytes)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var e chatgptEvent
		if json.Unmarshal(line, &e) != nil || e.Type != "CONVERSATION_MESSAGE" ||
			e.Message.Author.Type != "user" || e.Message.Content.Value == nil {
			continue
		}
		id := e.Message.ID
		if id == "" {
			id = e.EventID
		}
		at := parseTime(e.Message.CreatedAt)
		if at.IsZero() {
			at = parseTime(e.Timestamp)
		}
		if err := emit(Record{ID: id, OccurredAt: at, App: "chatgpt_web", UPN: e.Actor.UserEmail, Text: *e.Message.Content.Value}); err != nil {
			return err
		}
	}
	return sc.Err()
}

func (g *ChatGPT) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now()
}

// parseTime reads an RFC 3339 time, with or without fractional seconds; zero when it is not one.
func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(s))
	if err != nil {
		return time.Time{}
	}
	return t
}
