package providers

import (
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/shadow-ai-capture/device/protocol/canon"
)

// perplexityEvent is one Perplexity Enterprise audit log event, with the parts the intake reads.
type perplexityEvent struct {
	UUID      string `json:"uuid"`
	Timestamp string `json:"timestamp"`
	EventType string `json:"event_type"`
	UserEmail string `json:"user_email"`
	Metadata  struct {
		QueryStr    string `json:"query_str"`
		Query       string `json:"query"`
		Attachments []struct {
			FileName string `json:"file_name"`
		} `json:"attachments"`
	} `json:"metadata"`
}

// DecodePerplexityEvent reads one event of a Perplexity Enterprise organization's audit log, which
// Perplexity posts to the webhook its admin sets. A query event is a person's prompt, with the
// names of the files they attached; every other event (sign-ins, answers, settings) is not.
func DecodePerplexityEvent(entry []byte) ([]Record, error) {
	var e perplexityEvent
	if err := json.Unmarshal(entry, &e); err != nil {
		return nil, err
	}
	if e.UUID == "" || e.EventType == "" {
		return nil, errors.New("providers: the event is not a Perplexity audit log event")
	}
	if e.EventType != "query" {
		return nil, nil
	}
	text := e.Metadata.QueryStr
	if text == "" {
		text = e.Metadata.Query
	}
	var atts []canon.Attachment
	for _, a := range e.Metadata.Attachments {
		if strings.TrimSpace(a.FileName) != "" {
			atts = append(atts, canon.Attachment{Name: a.FileName, ContentDigest: canon.Unreadable})
		}
	}
	return []Record{{ID: e.UUID, OccurredAt: perplexityTime(e.Timestamp), App: "perplexity_web",
		UPN: e.UserEmail, Text: text, Attachments: atts}}, nil
}

// perplexityTime reads an event's time, which Perplexity writes with or without its UTC offset.
func perplexityTime(s string) time.Time {
	if t := parseTime(s); !t.IsZero() {
		return t
	}
	t, err := time.Parse("2006-01-02T15:04:05.999999999", strings.TrimSpace(s))
	if err != nil {
		return time.Time{}
	}
	return t
}
