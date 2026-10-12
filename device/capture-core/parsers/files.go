package parsers

import (
	"net/url"
	"regexp"
	"runtime"
	"strings"
	"unicode"

	"github.com/shadow-ai-capture/device/capture-core/dedup"
	"github.com/shadow-ai-capture/device/protocol"
)

// URLFileName is the file name a file URL ends in (`https://host/docs/q3.pdf`,
// `gs://bucket/q3.pdf`): its last path segment when that has an extension. A URL that ends in an
// id rather than a name (`/v1beta/files/abc123`), a data URL and anything unparsable give "".
func URLFileName(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "data" || u.Opaque != "" {
		return ""
	}
	seg := u.Path[strings.LastIndexByte(u.Path, '/')+1:]
	if dot := strings.LastIndexByte(seg, '.'); dot <= 0 || dot == len(seg)-1 {
		return ""
	}
	return seg
}

// PathWord is a word of a prompt that reads as a path.
type PathWord struct {
	// Path is the word without its quotes, the brackets and punctuation of the sentence around it,
	// a leading `@` and a line reference.
	Path string
	// Mentioned is set when the word @-mentions the path, as Claude Code attaches a file.
	Mentioned bool
}

var (
	// lineSuffix is a line reference after a path: Claude Code's `#L10-20` or a compiler's `:42:7`.
	lineSuffix = regexp.MustCompile(`(#L\d+(-\d+)?|(:\d+){1,2})$`)
	// extension is a file name's extension: a dot after at least one character, then letters and
	// digits.
	extension = regexp.MustCompile(`.\.[A-Za-z0-9]{1,16}$`)
	// scheme is a URL's or an MCP resource's prefix (`https:`, `github:`), which a drive letter is not.
	scheme = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]+:`)
	// drive is a Windows path's drive.
	drive = regexp.MustCompile(`^[A-Za-z]:[\\/]`)
)

// PathWords is the words of a prompt that read as paths, in order: a word with a separator, under
// the home directory (`~`), or with a file extension. A quoted run at the start of a word is one
// word, as a terminal and Codex quote a path with spaces; outside Windows a backslash before a
// space keeps it in the word, as a terminal escapes a dropped file's path. A URL or an MCP resource
// is not a path.
func PathWords(prompt string) []PathWord {
	var out []PathWord
	for _, w := range promptWords(prompt) {
		mentioned := strings.HasPrefix(w, "@")
		p := strings.TrimPrefix(w, "@")
		p = strings.TrimLeft(p, "([{<")
		p = strings.TrimRight(p, ")]}>,;!?'\".")
		p = lineSuffix.ReplaceAllString(p, "")
		if p == "" || scheme.MatchString(p) {
			continue
		}
		if strings.ContainsAny(p, `/\`) || strings.HasPrefix(p, "~") || extension.MatchString(p) {
			out = append(out, PathWord{Path: p, Mentioned: mentioned})
		}
	}
	return out
}

func promptWords(prompt string) []string {
	var words []string
	var w strings.Builder
	flush := func() {
		if w.Len() > 0 {
			words = append(words, w.String())
			w.Reset()
		}
	}
	rs := []rune(prompt)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case (r == '"' || r == '\'') && (w.Len() == 0 || w.String() == "@"):
			end := indexRune(rs[i+1:], r)
			if end < 0 {
				w.WriteRune(r)
				continue
			}
			w.WriteString(string(rs[i+1 : i+1+end]))
			i += end + 1
		case unicode.IsSpace(r):
			flush()
		case r == '\\' && runtime.GOOS != "windows" && i+1 < len(rs) && rs[i+1] == ' ':
			w.WriteRune(' ')
			i++
		default:
			w.WriteRune(r)
		}
	}
	flush()
	return words
}

func indexRune(rs []rune, r rune) int {
	for i, c := range rs {
		if c == r {
			return i
		}
	}
	return -1
}

// MentionedFiles names the files a prompt @-mentions, from its text alone: each mention whose last
// segment has a file extension, with its path when the mention is absolute. A relative mention is
// named without a path, since the text does not carry the directory it is relative to.
func MentionedFiles(prompt string) []dedup.Attachment {
	var out []dedup.Attachment
	for _, w := range PathWords(prompt) {
		if !w.Mentioned || !extension.MatchString(baseName(w.Path)) {
			continue
		}
		if f, ok := FileAt(w.Path); ok {
			out = AppendFile(out, f)
		}
	}
	return out
}

// FileAt is the file a path names: its last segment, on either separator, and the path itself when
// it is absolute. false when the path ends in a separator.
func FileAt(path string) (dedup.Attachment, bool) {
	name := baseName(path)
	if name == "" {
		return dedup.Attachment{}, false
	}
	f := dedup.Attachment{Name: name}
	if strings.HasPrefix(path, "/") || strings.HasPrefix(path, `\\`) || drive.MatchString(path) {
		f.Path = path
	}
	return f, true
}

func baseName(path string) string {
	return path[strings.LastIndexAny(path, `/\`)+1:]
}

// AppendFile adds f unless a file of the same name and path is already there, up to the
// envelope's limit.
func AppendFile(files []dedup.Attachment, f dedup.Attachment) []dedup.Attachment {
	if len(files) == protocol.MaxAttachments {
		return files
	}
	for _, have := range files {
		if have.Name == f.Name && have.Path == f.Path {
			return files
		}
	}
	return append(files, f)
}
