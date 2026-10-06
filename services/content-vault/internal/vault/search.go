package vault

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"
	"unicode"

	"github.com/shadow-ai-capture/content-vault/internal/store"
)

const (
	maxSearchResults = 50
	defaultResults   = 20
	maxSnippetRunes  = 240
	maxQueryRunes    = 256
	// fuzzyThreshold is pg_trgm's default similarity threshold.
	fuzzyThreshold = 0.3
)

// SearchRequest is one analyst search.
type SearchRequest struct {
	TenantID  string
	Principal string
	SessionID string
	// Form is terms (default), substring or fuzzy.
	Form          store.SearchForm
	Query         string
	Limit         int
	CaseReference string
	Filters       store.SearchFilters
	// Cursor is the opaque position a later page resumes from; empty for the first page.
	Cursor string
}

// SearchResult is one page of bounded snippets.
type SearchResult struct {
	Hits       []store.SearchHit
	Tier       store.SearchTier
	UnitKinds  []string
	NextCursor string // empty on the last page
}

// Search runs one search under the tenant's content search tier. The audit row, which records the
// terms and filters, is written in the transaction that serves the results.
//
// The tier decides what can be searched: full_text searches prompt text and attachment names,
// attachment_names searches attachment names only, and disabled refuses. The substring and fuzzy
// forms match attachment names only.
func (s *Service) Search(ctx context.Context, req SearchRequest) (SearchResult, error) {
	if req.Form == "" {
		req.Form = store.FormTerms
	}
	text, err := searchText(req.Form, req.Query)
	if err != nil {
		return SearchResult{}, err
	}
	var cursor *store.SearchCursor
	if req.Cursor != "" {
		if cursor, err = decodeCursor(req.Cursor); err != nil {
			return SearchResult{}, err
		}
	}
	limit := req.Limit
	if limit <= 0 {
		limit = defaultResults
	}
	limit = min(limit, maxSearchResults)
	now := s.now().UTC()

	var res SearchResult
	err = s.store.InTenant(ctx, req.TenantID, func(tx store.Tx) error {
		t, err := tenant(ctx, tx)
		if err != nil {
			return err
		}
		if t.Status == "closed" || !t.ReadEnabled {
			return deny(ReasonRetrievalDisabled, "content reads are disabled for tenant %s", t.TenantID)
		}
		unitKind, err := unitKindFor(t.ContentSearch, req.Form)
		if err != nil {
			return err
		}
		detail := map[string]any{
			"form": string(req.Form), "terms": text, "unit_kind": unitKind, "limit": limit,
			"tier": string(t.ContentSearch),
		}
		if f := filterDetail(req.Filters); len(f) > 0 {
			detail["filters"] = f
		}
		if req.SessionID != "" {
			detail["sid"] = req.SessionID
		}
		if err := tx.AppendAudit(ctx, store.AuditEntry{
			ActorType: "user", ActorID: req.Principal, Action: ActionSearch, ObjectType: "search_text",
			CaseReference: req.CaseReference, Detail: detail, OccurredAt: now,
		}); err != nil {
			return err
		}
		// One row more than the page, so whether another page exists is known without a second query.
		hits, err := tx.Search(ctx, store.SearchQuery{
			Form: req.Form, Text: text, UnitKind: unitKind, Limit: limit + 1, MinSimilar: fuzzyThreshold,
			Filters: req.Filters, Cursor: cursor, Now: now,
		})
		if err != nil {
			return err
		}
		res = SearchResult{Tier: t.ContentSearch, UnitKinds: unitKinds(t.ContentSearch)}
		if len(hits) > limit {
			hits = hits[:limit]
			last := hits[len(hits)-1]
			res.NextCursor = encodeCursor(store.SearchCursor{
				ReceivedAt: last.ReceivedAt, SubmissionID: last.SubmissionID, UnitKind: last.UnitKind, UnitIndex: last.UnitIndex,
			})
		}
		for i := range hits {
			hits[i].Snippet = bound(hits[i].Snippet)
		}
		res.Hits = hits
		return nil
	})
	return res, err
}

// unitKindFor maps the tier and the form to the unit kind searched; "" means both.
func unitKindFor(tier store.SearchTier, form store.SearchForm) (string, error) {
	if tier != store.SearchFullText && tier != store.SearchAttachmentNames {
		return "", deny(ReasonSearchDisabled, "content search is disabled for this tenant")
	}
	switch form {
	case store.FormSubstring, store.FormFuzzy:
		return store.UnitAttachmentName, nil
	case store.FormTerms:
		if tier == store.SearchFullText {
			return "", nil
		}
		return store.UnitAttachmentName, nil
	}
	return "", deny(ReasonInvalidRequest, "search form %q is not terms, substring or fuzzy", form)
}

func unitKinds(tier store.SearchTier) []string {
	switch tier {
	case store.SearchFullText:
		return []string{store.UnitPromptBody, store.UnitAttachmentName}
	case store.SearchAttachmentNames:
		return []string{store.UnitAttachmentName}
	}
	return nil
}

// searchText turns what the analyst typed into the statement's parameter.
//
// For the terms form it is tsquery text built from sanitised terms: letters, digits and
// underscores only, joined with &, a quoted phrase joined with <->. That removes every tsquery
// operator, so to_tsquery cannot fail and an analyst cannot inject one. For substring it is the
// text with LIKE's metacharacters escaped; for fuzzy, the text itself.
func searchText(form store.SearchForm, query string) (string, error) {
	raw := strings.TrimSpace(query)
	switch {
	case raw == "":
		return "", deny(ReasonInvalidRequest, "an empty search is refused rather than returning the whole index")
	case len([]rune(raw)) > maxQueryRunes:
		return "", deny(ReasonInvalidRequest, "the search text is longer than %d characters", maxQueryRunes)
	}
	switch form {
	case store.FormSubstring:
		return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(raw), nil
	case store.FormFuzzy:
		return raw, nil
	case store.FormTerms:
	default:
		return "", deny(ReasonInvalidRequest, "search form %q is not terms, substring or fuzzy", form)
	}
	var parts []string
	for _, term := range splitTerms(raw) {
		var words []string
		for _, w := range strings.Fields(term) {
			if w = sanitiseTerm(w); w != "" {
				words = append(words, w)
			}
		}
		if len(words) > 0 {
			parts = append(parts, strings.Join(words, " <-> "))
		}
	}
	if len(parts) == 0 {
		return "", deny(ReasonInvalidRequest, "the search text has no searchable term")
	}
	return strings.Join(parts, " & "), nil
}

func sanitiseTerm(w string) string {
	var b strings.Builder
	for _, r := range w {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
			b.WriteRune(unicode.ToLower(r))
		}
	}
	return b.String()
}

// splitTerms splits on whitespace, keeping double-quoted phrases together and dropping bare
// and/or/not.
func splitTerms(q string) []string {
	var out []string
	var cur strings.Builder
	inQuote := false
	flush := func() {
		t := strings.TrimSpace(cur.String())
		cur.Reset()
		if t == "" || !inQuote && (strings.EqualFold(t, "and") || strings.EqualFold(t, "or") || strings.EqualFold(t, "not")) {
			return
		}
		out = append(out, t)
	}
	for _, r := range q {
		switch {
		case r == '"':
			flush()
			inQuote = !inQuote
		case unicode.IsSpace(r) && !inQuote:
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return out
}

// bound truncates a snippet so a search never returns an unbounded window of content.
func bound(s string) string {
	r := []rune(s)
	if len(r) <= maxSnippetRunes {
		return s
	}
	return string(r[:maxSnippetRunes]) + "…"
}

func filterDetail(f store.SearchFilters) map[string]any {
	out := map[string]any{}
	for k, v := range map[string]string{"subject": f.Subject, "tool": f.Tool, "device": f.Device, "mode": f.Mode} {
		if v != "" {
			out[k] = v
		}
	}
	if !f.ReceivedFrom.IsZero() {
		out["received_from"] = f.ReceivedFrom.UTC().Format(time.RFC3339)
	}
	if !f.ReceivedTo.IsZero() {
		out["received_to"] = f.ReceivedTo.UTC().Format(time.RFC3339)
	}
	return out
}

// cursorJSON is the wire shape of a page cursor: the ordering key of the last hit returned.
type cursorJSON struct {
	T time.Time `json:"t"`
	S string    `json:"s"`
	K string    `json:"k"`
	I int       `json:"i"`
}

func encodeCursor(c store.SearchCursor) string {
	b, _ := json.Marshal(cursorJSON{T: c.ReceivedAt.UTC(), S: c.SubmissionID, K: c.UnitKind, I: c.UnitIndex})
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeCursor(s string) (*store.SearchCursor, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	var c cursorJSON
	if err != nil || json.Unmarshal(b, &c) != nil {
		return nil, deny(ReasonSearchCursorInvalid, "the page cursor is not one this service issued")
	}
	if _, err := parseUUID(c.S); err != nil || c.T.IsZero() || (c.K != store.UnitPromptBody && c.K != store.UnitAttachmentName) || c.I < 0 {
		return nil, deny(ReasonSearchCursorInvalid, "the page cursor is missing its ordering key")
	}
	return &store.SearchCursor{ReceivedAt: c.T, SubmissionID: c.S, UnitKind: c.K, UnitIndex: c.I}, nil
}
