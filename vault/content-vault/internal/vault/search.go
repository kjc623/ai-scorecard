package vault

import (
	"context"
	"fmt"
	"strings"
	"unicode"

	"github.com/shadow-ai-capture/device/protocol"

	"github.com/shadow-ai-capture/content-vault/internal/store"
)

// SearchRequest is one analyst search (docs/04 §15.3).
//
// The tenant comes from the authenticated principal and never from the body (docs/02 §12), and the
// scope is what the signed policy bundle narrows the tenant's ceiling by: "a tenant cannot enable
// full_text for everything; it enables it for named scopes, and a scope that is not named carries
// disabled" (§6.3).
type SearchRequest struct {
	TenantID  string
	Principal string
	Scope     string
	Form      store.SearchForm
	Query     string
	Limit     int
	// CaseReference is optional for a search and required for a retrieval: C16 gates full-content
	// retrieval, not bounded snippets (§6.3's honest consequence 3).
	CaseReference string
}

// SearchResult is bounded snippets plus the counts the audit row records.
type SearchResult struct {
	State     string
	Hits      []store.SearchHit
	UnitKinds []string
	Effective store.SearchTier
	Truncated bool
}

// Search executes one search under the tenant's ceiling, the bundle's scope narrowing, the closed
// form set, and an audit row written in the same transaction that serves it.
func (s *Service) Search(ctx context.Context, req SearchRequest) (SearchResult, error) {
	now := s.opts.Now().UTC()
	tenant, err := s.loadTenant(ctx, req.TenantID)
	if err != nil {
		return SearchResult{}, err
	}
	if !tenant.ReadEnabled {
		return SearchResult{}, Denialf(DenyRetrievalDisabled, "reads are disabled for tenant %s", tenant.TenantID)
	}
	// ADR 0014's invariant, at the service boundary: even if the constraint is missing from the
	// database this service is pointed at, a customer_held tenant never has its prompt text
	// indexed, because an index over plaintext is plaintext-derived (06 §5.6).
	if tenant.ContentSearch == store.SearchFullText && tenant.KeyCustody == store.CustodyCustomerHeld {
		return SearchResult{}, Denialf(DenyKeyCustodySearchConflict,
			"tenant %s is customer_held and full_text together, which ADR 0014 makes unrepresentable; refusing to search rather than reading an index that must not exist",
			tenant.TenantID)
	}
	if tenant.ContentSearch == store.SearchFullText && tenant.CeilingMode != protocol.ModeM3 {
		return SearchResult{}, Denialf(DenySearchTierRequiresM3,
			"full_text requires M3 for the searched scope and tenant %s has ceiling %s", tenant.TenantID, tenant.CeilingMode)
	}

	effective, inBundle := s.effectiveTier(tenant, req.Scope)
	switch {
	case !inBundle:
		// §6.3: "a scope that is not named carries disabled". The distinction between "the bundle
		// never named this scope" and "the bundle named it as disabled" is the difference between a
		// configuration that is silent and one that is deliberate, and an operator acts on them
		// differently.
		return SearchResult{}, Denialf(DenySearchTierNotInScope,
			"the signed bundle does not name scope %q for tenant %s, and an unnamed scope carries disabled", req.Scope, tenant.TenantID)
	case effective == store.SearchDisabled:
		return SearchResult{}, Denialf(DenySearchDisabled, "content search is disabled for tenant %s", tenant.TenantID)
	}
	if !req.Form.Valid() {
		return SearchResult{}, fmt.Errorf("vault: search form %q is outside the closed set {terms, substring, fuzzy}", req.Form)
	}

	unitKind, err := s.unitKindFor(effective, req.Form)
	if err != nil {
		return SearchResult{}, err
	}
	query, err := s.buildQuery(req)
	if err != nil {
		return SearchResult{}, err
	}

	limit := req.Limit
	if limit <= 0 || limit > s.opts.MaxSearchResults {
		limit = s.opts.MaxSearchResults
	}
	q := store.SearchQuery{
		TenantID: tenant.TenantID, Form: req.Form, Text: query, UnitKind: unitKind,
		Limit: limit, MinSimilar: s.opts.FuzzyThreshold,
	}

	// Audit before serve, in the same transaction: §6.3 requires "no audit row, no results", and
	// the failure mode it prevents is a query used as a confirmation oracle, which produces its
	// signal precisely when it returns nothing.
	hits, err := s.opts.Store.SearchAudited(ctx, q, store.AuditEntry{
		TenantID: tenant.TenantID, ActorType: "user", ActorID: req.Principal,
		Action: ActionSearch, ObjectType: "search_text", ObjectID: req.Scope,
		CaseReference: req.CaseReference,
		Detail: map[string]any{
			// The query terms are themselves subject-level data (§6.3): an audit log recording
			// that an analyst searched for a name is a record of an investigation into that
			// person, so it belongs in the audited, hash-chained trail and nowhere else.
			"form": string(req.Form), "terms": query, "scope": req.Scope,
			"unit_kind": unitKind, "limit": limit, "effective_tier": string(effective),
		},
		OccurredAt: now,
	})
	if err != nil {
		return SearchResult{}, Denialf(DenyAuditUnavailable, "the search audit row could not be committed, so the search fails closed: %v", err)
	}

	out := make([]store.SearchHit, 0, len(hits))
	for _, h := range hits {
		h.Snippet = bound(h.Snippet, s.opts.MaxSnippetChars)
		out = append(out, h)
	}
	return SearchResult{
		State:     "available",
		Hits:      out,
		UnitKinds: unitKinds(effective),
		Effective: effective,
		Truncated: len(hits) >= limit,
	}, nil
}

// effectiveTier applies the bundle's narrowing: the tenant tier is a ceiling, and a scope the
// bundle does not name carries disabled. The second return value distinguishes "not named" from
// "named as disabled", because only the caller can explain the former.
func (s *Service) effectiveTier(tenant store.Tenant, scope string) (store.SearchTier, bool) {
	bundle, ok := s.opts.ScopeTiers[scope]
	if !ok {
		return store.SearchDisabled, false
	}
	if bundle.Rank() > tenant.ContentSearch.Rank() {
		// The bundle may not raise a scope above the tenant's ceiling. This is a defect in the
		// signed bundle, and taking the lower value is the fail-closed reading.
		return tenant.ContentSearch, true
	}
	return bundle, true
}

// unitKindFor maps the tier and the form to the one unit kind the SQL statements will consider.
// The substring and fuzzy forms are served by the partial trigram index, which exists for
// attachment names only (docs/04 §15.2), so they are filenames-only by construction.
func (s *Service) unitKindFor(tier store.SearchTier, form store.SearchForm) (string, error) {
	switch form {
	case store.FormSubstring, store.FormFuzzy:
		if tier.Rank() < store.SearchAttachmentNames.Rank() {
			return "", Denialf(DenySearchUnitNotPermitted,
				"the %s form searches attachment names, which needs the attachment_names tier; this scope holds %q", form, tier)
		}
		return store.UnitAttachmentName, nil
	case store.FormTerms:
		switch tier {
		case store.SearchFullText:
			return "", nil // both kinds: prompt bodies and filenames
		case store.SearchAttachmentNames:
			return store.UnitAttachmentName, nil
		default:
			return "", Denialf(DenySearchDisabled, "the terms form needs at least the attachment_names tier")
		}
	default:
		return "", fmt.Errorf("vault: search form %q is outside the closed set", form)
	}
}

func unitKinds(tier store.SearchTier) []string {
	switch tier {
	case store.SearchFullText:
		return []string{store.UnitPromptBody, store.UnitAttachmentName}
	case store.SearchAttachmentNames:
		return []string{store.UnitAttachmentName}
	default:
		return nil
	}
}

// buildQuery turns an analyst's match expression into the parameter the statement receives.
//
// The transformation depends on the form, and conflating the two was a real defect caught by the
// tests: the terms form needs a *tsquery text*, while the substring and fuzzy forms are not tsquery
// syntax at all — a filename like `Q3-contract.pdf` is matched by `ILIKE` and by trigram
// similarity, and running it through the term sanitiser would strip the hyphen and search for
// something nobody typed.
//
// For the terms form the sanitiser is a closed transformation, and that is the point: to_tsquery()
// raises a syntax error on malformed input, and a search must not be a way to make the database
// raise. Terms are restricted to letters, digits and underscores — which also removes every tsquery
// operator, so an analyst cannot inject `!`, `&`, `<->` or a subquery-shaped string into the query
// text — joined with `&`, with a quoted phrase becoming a `<->` sequence.
//
// For the other two forms the text is trimmed, length-capped and lower-cased, which is what makes
// the in-memory double and PostgreSQL agree: `ILIKE` is case-insensitive by definition, and the
// fuzzy statement compares `lower(body)` with `lower($2)`.
func (s *Service) buildQuery(req SearchRequest) (string, error) {
	raw := strings.TrimSpace(req.Query)
	if raw == "" {
		return "", Denialf(DenySearchDisabled, "an empty search is refused rather than returning the whole index")
	}
	if len([]rune(raw)) > 256 {
		return "", fmt.Errorf("vault: search text is %d characters, over the 256-character cap", len([]rune(raw)))
	}
	if req.Form == store.FormSubstring || req.Form == store.FormFuzzy {
		return strings.ToLower(raw), nil
	}
	terms := splitTerms(raw)
	if len(terms) == 0 {
		return "", fmt.Errorf("vault: search text carries no term that survives sanitisation")
	}
	parts := make([]string, 0, len(terms))
	for _, t := range terms {
		words := strings.Fields(t)
		sane := make([]string, 0, len(words))
		for _, w := range words {
			w = sanitiseTerm(w)
			if w != "" {
				sane = append(sane, w)
			}
		}
		if len(sane) == 0 {
			continue
		}
		if len(sane) == 1 {
			parts = append(parts, sane[0])
			continue
		}
		// A phrase: adjacent words in order.
		parts = append(parts, strings.Join(sane, " <-> "))
	}
	if len(parts) == 0 {
		return "", fmt.Errorf("vault: search text carries no term that survives sanitisation")
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

// splitTerms splits on whitespace, keeping double-quoted phrases together.
func splitTerms(q string) []string {
	var out []string
	var cur strings.Builder
	inQuote := false
	flush := func() {
		t := strings.TrimSpace(cur.String())
		cur.Reset()
		if t == "" {
			return
		}
		if !inQuote && (strings.EqualFold(t, "and") || strings.EqualFold(t, "or") || strings.EqualFold(t, "not")) {
			return
		}
		out = append(out, t)
	}
	for _, r := range q {
		switch {
		case r == '"':
			if inQuote {
				flush()
			}
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

// bound truncates a snippet to the configured ceiling and marks the truncation, so a caller cannot
// receive an unbounded window of content from a search (§6.3: bounded highlighted snippets).
func bound(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}
