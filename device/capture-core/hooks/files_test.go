package hooks_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/shadow-ai-capture/device/capture-core/hooks"
	"github.com/shadow-ai-capture/device/protocol"
)

// workspace is a working directory with files a prompt can name, and a home directory beside it.
func workspace(t *testing.T) (cwd, home string) {
	t.Helper()
	root := t.TempDir()
	cwd, home = filepath.Join(root, "project"), filepath.Join(root, "home")
	for name, body := range map[string]string{
		filepath.Join(cwd, "plan.md"):               "plan.",
		filepath.Join(cwd, "Q3 forecast.xlsx"):      "xls",
		filepath.Join(cwd, "src", "app.go"):         "package app",
		filepath.Join(cwd, "docs", "readme.txt"):    "docs",
		filepath.Join(home, "notes.txt"):            "notes",
		filepath.Join(cwd, "it's mine", "deck.pdf"): "%PDF",
	} {
		if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	return cwd, home
}

func file(path string, size int64) protocol.AttachmentDescriptor {
	return protocol.AttachmentDescriptor{Name: filepath.Base(path), Path: path, SizeBytes: size}
}

func parsePrompt(t *testing.T, tool, event string, input map[string]any) protocol.HookEvaluate {
	t.Helper()
	a, ok := hooks.Lookup(tool)
	if !ok {
		t.Fatalf("no adapter for %s", tool)
	}
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	ev, err := a.Parse(event, raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := ev.Validate(); err != nil {
		t.Fatalf("the parsed hook_evaluate is refused: %v", err)
	}
	return ev
}

// A prompt attaches the files it names: by Claude Code's @-mention, quoted, under the home
// directory, with a line reference, and by an absolute path. A directory, a URL, an MCP resource
// and a name that is not on disk attach nothing, and a file named twice is attached once.
func TestClaudeCodeAttachesTheFilesThePromptNames(t *testing.T) {
	cwd, home := workspace(t)
	prompt := `Compare @plan.md with "Q3 forecast.xlsx" and @src/app.go#L10-20 (see ~/notes.txt). ` +
		`Skip @docs, https://example.com/a.pdf, @github:repos/o/r/issues and missing.pdf; @plan.md again, ` +
		`then "` + filepath.Join(cwd, "it's mine", "deck.pdf") + `".`
	ev := parsePrompt(t, hooks.ClaudeCodeTool, hooks.ClaudeCodeUserPromptSubmit, map[string]any{
		"session_id": "s", "cwd": cwd, "prompt": prompt,
	})
	want := []protocol.AttachmentDescriptor{
		file(filepath.Join(cwd, "plan.md"), 5),
		file(filepath.Join(cwd, "Q3 forecast.xlsx"), 3),
		file(filepath.Join(cwd, "src", "app.go"), 11),
		file(filepath.Join(home, "notes.txt"), 5),
		file(filepath.Join(cwd, "it's mine", "deck.pdf"), 4),
	}
	if !reflect.DeepEqual(ev.Attachments, want) {
		t.Fatalf("attachments\n%+v\nwant\n%+v", ev.Attachments, want)
	}
	if ev.PromptText != prompt {
		t.Fatal("naming the files changed the prompt")
	}
}

// Codex's file search writes the path, quoted when it has a space, and a terminal escapes the
// space of a file dropped on it.
func TestCodexAttachesThePathsItsFileSearchWrites(t *testing.T) {
	cwd, _ := workspace(t)
	prompt := `summarise src/app.go and "Q3 forecast.xlsx"`
	want := []protocol.AttachmentDescriptor{file(filepath.Join(cwd, "src", "app.go"), 11), file(filepath.Join(cwd, "Q3 forecast.xlsx"), 3)}
	if runtime.GOOS != "windows" {
		prompt += " and " + strings.ReplaceAll(filepath.Join(cwd, "it's mine", "deck.pdf"), " ", `\ `)
		want = append(want, file(filepath.Join(cwd, "it's mine", "deck.pdf"), 4))
	}
	ev := parsePrompt(t, hooks.CodexTool, hooks.CodexUserPromptSubmit, map[string]any{
		"session_id": "s", "cwd": cwd, "prompt": prompt,
	})
	if !reflect.DeepEqual(ev.Attachments, want) {
		t.Fatalf("attachments\n%+v\nwant\n%+v", ev.Attachments, want)
	}
}

// Without a working directory a relative path names nothing.
func TestARelativePathNeedsAWorkingDirectory(t *testing.T) {
	workspace(t)
	ev := parsePrompt(t, hooks.CodexTool, hooks.CodexUserPromptSubmit, map[string]any{
		"session_id": "s", "prompt": "read plan.md",
	})
	if len(ev.Attachments) != 0 {
		t.Fatalf("attachments %+v, want none", ev.Attachments)
	}
}

// Cursor lists the files attached to the prompt: each is named and placed, with its size when it
// is on disk, and a rule is not a file the user attached. A path the text names joins them, against
// the first workspace root.
func TestCursorAttachesTheFilesItLists(t *testing.T) {
	cwd, _ := workspace(t)
	gone := filepath.Join(cwd, "gone.txt")
	ev := parsePrompt(t, hooks.CursorTool, hooks.CursorBeforeSubmitPrompt, map[string]any{
		"conversation_id": "c",
		"workspace_roots": []string{cwd},
		"prompt":          "use src/app.go and plan.md",
		"attachments": []map[string]string{
			{"type": "file", "file_path": filepath.Join(cwd, "plan.md")},
			{"type": "rule", "file_path": filepath.Join(cwd, ".cursor", "rules", "deploy.mdc")},
			{"type": "file", "file_path": gone},
			{"type": "file", "file_path": filepath.Join(cwd, "docs")},
		},
	})
	want := []protocol.AttachmentDescriptor{
		file(filepath.Join(cwd, "plan.md"), 5),
		file(gone, 0),
		file(filepath.Join(cwd, "src", "app.go"), 11),
	}
	if !reflect.DeepEqual(ev.Attachments, want) {
		t.Fatalf("attachments\n%+v\nwant\n%+v", ev.Attachments, want)
	}
}

// A prompt naming more files than a record holds attaches the first of them.
func TestAPromptAttachesAtMostARecordsWorth(t *testing.T) {
	cwd := t.TempDir()
	var words []string
	for i := range protocol.MaxAttachments + 5 {
		name := fmt.Sprintf("f%02d.txt", i)
		if err := os.WriteFile(filepath.Join(cwd, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		words = append(words, "@"+name)
	}
	ev := parsePrompt(t, hooks.ClaudeCodeTool, hooks.ClaudeCodeUserPromptSubmit, map[string]any{
		"session_id": "s", "cwd": cwd, "prompt": strings.Join(words, " "),
	})
	if len(ev.Attachments) != protocol.MaxAttachments || ev.Attachments[0].Name != "f00.txt" {
		t.Fatalf("%d attachments starting %+v, want the first %d", len(ev.Attachments), ev.Attachments[:1], protocol.MaxAttachments)
	}
}
