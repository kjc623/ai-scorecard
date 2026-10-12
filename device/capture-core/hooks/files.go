package hooks

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/shadow-ai-capture/device/protocol"
)

// statBudget bounds the time a hook spends looking files up, out of its 400 ms: a path on a slow
// or unreachable share is left out rather than holding the prompt.
const statBudget = 50 * time.Millisecond

// maxPathWords bounds the words of one prompt looked up on disk.
const maxPathWords = 64

// promptFiles is the files a prompt names that are on disk: each word of the prompt that reads as
// a path (`@src/app.go`, `"C:\Shared Docs\q3.xlsx"`, `~/notes.md`, `report.pdf`), resolved against
// the working directory, that names a regular file. A path in the prompt is how Claude Code (`@`)
// and Codex (whose file search writes the path) attach a file, and how a terminal pastes one
// dropped on it. Directories, URLs and words that name nothing on disk are left out.
func promptFiles(prompt, cwd string) []protocol.AttachmentDescriptor {
	var paths []string
	seen := map[string]bool{}
	for _, w := range promptWords(prompt) {
		p := resolvePath(pathWord(w), cwd)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		paths = append(paths, p)
		if len(paths) == maxPathWords {
			break
		}
	}
	return statFiles(paths, statBudget, false)
}

// statFiles describes each of paths that is a regular file, in order, for as long as budget lasts.
// A file is never read, only looked up. A path that cannot be looked up is left out, or with
// attached set (the tool says it attached the file) described by its name and path alone.
func statFiles(paths []string, budget time.Duration, attached bool) []protocol.AttachmentDescriptor {
	if len(paths) == 0 {
		return nil
	}
	var mu sync.Mutex
	var out []protocol.AttachmentDescriptor
	done := make(chan struct{})
	go func() {
		defer close(done)
		for _, p := range paths {
			var size int64
			info, err := os.Stat(p)
			switch {
			case err == nil && !info.Mode().IsRegular():
				continue
			case err == nil:
				size = info.Size()
			case !attached:
				continue
			}
			d, ok := describeFile(p, size)
			if !ok {
				continue
			}
			mu.Lock()
			out = append(out, d)
			full := len(out) == protocol.MaxAttachments
			mu.Unlock()
			if full {
				return
			}
		}
	}()
	timer := time.NewTimer(budget)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	}
	mu.Lock()
	defer mu.Unlock()
	return append([]protocol.AttachmentDescriptor(nil), out...)
}

// describeFile is the descriptor of the file at path, false when its name or path is over what the
// envelope admits.
func describeFile(path string, size int64) (protocol.AttachmentDescriptor, bool) {
	name := filepath.Base(path)
	if name == "" || name == "." || name == string(filepath.Separator) ||
		utf8.RuneCountInString(name) > protocol.MaxAttachmentNameRunes ||
		utf8.RuneCountInString(path) > protocol.MaxAttachmentPathRunes {
		return protocol.AttachmentDescriptor{}, false
	}
	return protocol.AttachmentDescriptor{Name: name, Path: path, SizeBytes: max(size, 0)}, true
}

// promptWords splits a prompt at whitespace. A quoted run at the start of a word is one word, quotes
// removed, as a terminal and Codex quote a path with spaces; outside Windows a backslash before a
// space keeps it in the word, as a terminal escapes a dropped file's path.
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

var (
	// lineSuffix is a line reference after a path: Claude Code's `#L10-20` or a compiler's `:42:7`.
	lineSuffix = regexp.MustCompile(`(#L\d+(-\d+)?|(:\d+){1,2})$`)
	// extension is a file name's extension: a dot after at least one character, then letters and
	// digits.
	extension = regexp.MustCompile(`.\.[A-Za-z0-9]{1,16}$`)
	// scheme is a URL's or an MCP resource's prefix (`https:`, `github:`), which a drive letter is not.
	scheme = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]+:`)
)

// pathWord is the path a word holds, "" when it does not read as one: the word without a leading
// `@`, the brackets and punctuation of the sentence around it, and a line reference.
func pathWord(w string) string {
	w = strings.TrimPrefix(w, "@")
	w = strings.TrimLeft(w, "([{<")
	w = strings.TrimRight(w, ")]}>,;!?'\".")
	w = lineSuffix.ReplaceAllString(w, "")
	if w == "" || scheme.MatchString(w) {
		return ""
	}
	if strings.ContainsAny(w, `/\`) || strings.HasPrefix(w, "~") || extension.MatchString(w) {
		return w
	}
	return ""
}

// resolvePath is the absolute, clean form of p: under the home directory for `~`, and under cwd
// when relative. A relative path with no working directory to resolve it against is "".
func resolvePath(p, cwd string) string {
	switch {
	case p == "":
		return ""
	case p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`):
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		p = filepath.Join(home, p[1:])
	case filepath.IsAbs(p):
	case cwd == "":
		return ""
	default:
		p = filepath.Join(cwd, p)
	}
	return filepath.Clean(p)
}

// mergeFiles is a followed by those of b at paths a does not name, up to the envelope's limit.
func mergeFiles(a, b []protocol.AttachmentDescriptor) []protocol.AttachmentDescriptor {
	out := append([]protocol.AttachmentDescriptor(nil), a...)
	for _, d := range b {
		if len(out) == protocol.MaxAttachments {
			break
		}
		dup := false
		for _, have := range out {
			if have.Path == d.Path {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, d)
		}
	}
	return out
}
