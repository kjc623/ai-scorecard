package classify

import (
	"slices"
	"strings"
	"unicode"

	"golang.org/x/text/cases"

	"github.com/shadow-ai-capture/device/classifier-host/norm"
	"github.com/shadow-ai-capture/device/protocol"
)

// customLabels returns the custom labels of which text holds a term, in request order and once
// each. Text and terms are compared as sequences of words (runs of letters and digits), case-folded,
// with the term normalised as the text was: a term matches whole words only, and the punctuation and
// spacing between its words do not matter. expired is checked once per word of text.
func customLabels(text string, labels []protocol.CustomLabelTerms, expired func() bool) ([]protocol.Label, bool) {
	if len(labels) == 0 {
		return nil, true
	}
	fold := cases.Fold()
	// phrases holds, by its first word, each term as words with the labels it marks.
	type phrase struct {
		words  []string
		labels []string
	}
	phrases := map[string][]*phrase{}
	for _, l := range labels {
		for _, t := range l.Terms {
			res, err := norm.Normalise("text/plain", []byte(t), norm.DefaultLimits())
			if err != nil {
				continue
			}
			w := words(fold.String(res.Text))
			if len(w) == 0 {
				continue
			}
			i := slices.IndexFunc(phrases[w[0]], func(p *phrase) bool { return slices.Equal(p.words, w) })
			if i < 0 {
				phrases[w[0]] = append(phrases[w[0]], &phrase{words: w})
				i = len(phrases[w[0]]) - 1
			}
			if p := phrases[w[0]][i]; !slices.Contains(p.labels, l.Label) {
				p.labels = append(p.labels, l.Label)
			}
		}
	}
	found := map[string]bool{}
	tw := words(fold.String(text))
	for i, w := range tw {
		if expired() {
			return nil, false
		}
		for _, p := range phrases[w] {
			if len(tw)-i >= len(p.words) && slices.Equal(tw[i:i+len(p.words)], p.words) {
				for _, l := range p.labels {
					found[l] = true
				}
			}
		}
	}
	var out []protocol.Label
	for _, l := range labels {
		if found[l.Label] && !slices.ContainsFunc(out, func(x protocol.Label) bool { return x.Class == l.Label }) {
			out = append(out, protocol.Label{Class: l.Label, Score: protocol.CustomLabelScore})
		}
	}
	return out, true
}

// words splits s into its runs of letters and digits.
func words(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}
