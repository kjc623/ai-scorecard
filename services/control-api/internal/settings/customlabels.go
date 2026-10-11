package settings

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/shadow-ai-capture/control-api/internal/apierr"
	"github.com/shadow-ai-capture/control-api/internal/store"
)

// --- PUT /admin/v1/settings/custom-labels ----------------------------------------------------

// customLabelJSON is one custom label: its code, the name people see, and the four lists that mark
// a prompt with it. Every list is sent.
type customLabelJSON struct {
	Label          string   `json:"label"`
	DisplayName    string   `json:"display_name"`
	Words          []string `json:"words"`
	CustomerNames  []string `json:"customer_names"`
	FileExtensions []string `json:"file_extensions"`
	FileNames      []string `json:"file_names"`
}

func customLabelsJSON(labels []store.CustomLabel) []customLabelJSON {
	out := make([]customLabelJSON, 0, len(labels))
	for _, l := range labels {
		out = append(out, customLabelJSON{Label: l.Label, DisplayName: l.DisplayName, Words: listJSON(l.Words),
			CustomerNames: listJSON(l.CustomerNames), FileExtensions: listJSON(l.FileExtensions), FileNames: listJSON(l.FileNames)})
	}
	return out
}

var (
	customLabelPattern = regexp.MustCompile(`^custom_[a-z0-9_]{1,56}$`)
	// fileExtensionPattern is an extension without its leading dot, such as pdf or tar.gz.
	fileExtensionPattern = regexp.MustCompile(`^[a-z0-9]+(\.[a-z0-9]+)*$`)
)

// The longest display name, word or customer name, file extension and file name pattern, in
// characters.
const (
	maxDisplayName   = 80
	maxTerm          = 100
	maxFileExtension = 16
	maxFileName      = 255
)

// maxCustomLabelsBody holds a full list of labels, each with its most terms at their longest.
const maxCustomLabelsBody = 4 << 20

func (h *Handler) handlePutCustomLabels(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	// The list is required, so a client that sent none does not clear the tenant's labels.
	var req struct {
		CustomLabels *[]customLabelJSON `json:"custom_labels"`
	}
	if !h.decodeLimit(w, r, &req, maxCustomLabelsBody) {
		return
	}
	if req.CustomLabels == nil {
		h.fail(w, apierr.New(http.StatusBadRequest, apierr.CodeInvalidRequest, "custom_labels is required; send [] to remove every label"))
		return
	}
	labels, err := validCustomLabels(*req.CustomLabels)
	if err != nil {
		h.fail(w, err)
		return
	}
	now := h.cfg.Now().UTC()
	err = h.store.ReplaceCustomLabels(r.Context(), p.Tenant, labels,
		h.audit(p, "tenant.custom_labels.set", "tenant", p.Tenant, now, nil))
	switch {
	case errors.Is(err, store.ErrCustomLabelInUse):
		h.fail(w, apierr.New(http.StatusConflict, apierr.CodeCustomLabelInUse,
			"an enforcement rule names a label this list leaves out; remove the label from the rule first"))
		return
	case errors.Is(err, store.ErrUnknownTenant):
		h.fail(w, apierr.New(http.StatusForbidden, apierr.CodeUnknownTenant, "the tenant is unknown to this deployment"))
		return
	case err != nil:
		h.fail(w, apierr.Internal(fmt.Errorf("replace custom labels: %w", err)))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// validCustomLabels refuses every value the device's bundle validation or the database would
// refuse, naming the label and the field. Terms are trimmed and repeats dropped, case-insensitively;
// a file extension loses its leading dot and is lower-cased.
func validCustomLabels(in []customLabelJSON) ([]store.CustomLabel, error) {
	if len(in) > store.MaxCustomLabels {
		return nil, apierr.Detailed(http.StatusBadRequest, apierr.CodeInvalidRequest,
			fmt.Sprintf("at most %d custom labels are allowed", store.MaxCustomLabels), map[string]any{"max": store.MaxCustomLabels})
	}
	refuse := func(i int, field, message string) error {
		return apierr.Detailed(http.StatusBadRequest, apierr.CodeInvalidRequest,
			fmt.Sprintf("custom label %d: %s", i+1, message), map[string]any{"index": i, "field": field})
	}
	labels, names := map[string]bool{}, map[string]bool{}
	out := make([]store.CustomLabel, 0, len(in))
	for i, l := range in {
		if !customLabelPattern.MatchString(l.Label) {
			return nil, refuse(i, "label", "label must be custom_ followed by at most 56 lower-case letters, digits or '_'")
		}
		if labels[l.Label] {
			return nil, refuse(i, "label", fmt.Sprintf("label %q is used by an earlier label", l.Label))
		}
		labels[l.Label] = true
		name := strings.TrimSpace(l.DisplayName)
		if name == "" || utf8.RuneCountInString(name) > maxDisplayName || hasControl(name) {
			return nil, refuse(i, "display_name", fmt.Sprintf("display_name must hold 1 to %d characters", maxDisplayName))
		}
		if names[strings.ToLower(name)] {
			return nil, refuse(i, "display_name", fmt.Sprintf("display_name %q is used by an earlier label", name))
		}
		names[strings.ToLower(name)] = true

		c := store.CustomLabel{Label: l.Label, DisplayName: name}
		var err error
		if c.Words, err = terms(l.Words, func(t string) (string, string) { return t, textProblem(t) }); err != nil {
			return nil, refuse(i, "words", "words: "+err.Error())
		}
		if c.CustomerNames, err = terms(l.CustomerNames, func(t string) (string, string) { return t, textProblem(t) }); err != nil {
			return nil, refuse(i, "customer_names", "customer_names: "+err.Error())
		}
		if c.FileExtensions, err = terms(l.FileExtensions, fileExtension); err != nil {
			return nil, refuse(i, "file_extensions", "file_extensions: "+err.Error())
		}
		if c.FileNames, err = terms(l.FileNames, fileName); err != nil {
			return nil, refuse(i, "file_names", "file_names: "+err.Error())
		}
		n := len(c.Words) + len(c.CustomerNames) + len(c.FileExtensions) + len(c.FileNames)
		if n == 0 {
			return nil, refuse(i, "words", "a label needs at least one word, customer name, file extension or file name")
		}
		if n > store.MaxCustomLabelTerms {
			return nil, refuse(i, "words", fmt.Sprintf("a label holds at most %d terms across its lists", store.MaxCustomLabelTerms))
		}
		out = append(out, c)
	}
	return out, nil
}

// terms trims each value, drops blanks and case-insensitive repeats, and applies check, which
// returns the value as stored and a problem, "" when there is none.
func terms(in []string, check func(string) (string, string)) ([]string, error) {
	out := []string{}
	seen := map[string]bool{}
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		v, problem := check(v)
		if problem != "" {
			return nil, fmt.Errorf("%q %s", v, problem)
		}
		if k := strings.ToLower(v); !seen[k] {
			seen[k] = true
			out = append(out, v)
		}
	}
	return out, nil
}

func textProblem(t string) string {
	if utf8.RuneCountInString(t) > maxTerm || hasControl(t) {
		return fmt.Sprintf("must hold at most %d characters and no control characters", maxTerm)
	}
	if !strings.ContainsFunc(t, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) {
		return "must hold a letter or a digit"
	}
	return ""
}

func fileExtension(t string) (string, string) {
	t = strings.ToLower(strings.TrimPrefix(t, "."))
	if len(t) > maxFileExtension || !fileExtensionPattern.MatchString(t) {
		return t, fmt.Sprintf("must be an extension such as pdf or tar.gz, at most %d letters, digits and dots", maxFileExtension)
	}
	return t, ""
}

// fileName accepts a file name or a pattern with * and ? wildcards; no other character has a
// meaning, and a path separator or [ ] \ is refused so the device reads the pattern as written.
func fileName(t string) (string, string) {
	if utf8.RuneCountInString(t) > maxFileName || hasControl(t) || strings.ContainsAny(t, `/\[]`) {
		return t, fmt.Sprintf("must be a file name of at most %d characters without / \\ [ or ]", maxFileName)
	}
	if strings.Trim(t, "*?") == "" {
		return t, "must hold more than wildcards"
	}
	return t, ""
}

func hasControl(s string) bool {
	return strings.ContainsFunc(s, unicode.IsControl)
}
