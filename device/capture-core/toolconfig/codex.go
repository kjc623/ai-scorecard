package toolconfig

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/shadow-ai-capture/device/capture-core/hooks"
	"github.com/shadow-ai-capture/device/capture-core/state"
	"github.com/shadow-ai-capture/device/protocol"
)

// CodexTool is the Codex CLI's endpoint.tools key, and its backup folder's name.
const CodexTool = "codex"

// CodexFingerprint is the Codex CLI's catalog fingerprint.
const CodexFingerprint = "app:codex"

// codexHookTimeout is the hook's timeout in seconds, Codex's unit. Codex starts the command through
// the user's shell (PowerShell, or cmd), so it allows for the shell's start-up as well as the hook's
// own 400 ms.
const codexHookTimeout = 5

// codexArgs is what follows the quoted executable in the agent's hook command.
const codexArgs = " --hook " + hooks.CodexTool + " " + hooks.CodexUserPromptSubmit

// codexCommand is the agent's hook command line. Codex hands it to PowerShell (-Command) or to cmd
// (/c), and only a nested cmd runs a quoted path followed by arguments in both.
func codexCommand(exe string) string { return `cmd /c "` + exe + `"` + codexArgs }

// isCodexCommand reports whether a command line runs the agent's hook, from wherever the agent is or
// was installed.
func isCodexCommand(cmd string) bool {
	quoted, ok := strings.CutPrefix(cmd, `cmd /c "`)
	if !ok {
		return false
	}
	exe, args, ok := strings.Cut(quoted, `"`)
	return ok && args == codexArgs && isCaptureCore(exe)
}

// codexGroup is the agent's UserPromptSubmit matcher group.
func codexGroup(exe string) map[string]any {
	return map[string]any{"hooks": []any{map[string]any{
		"type":    "command",
		"command": codexCommand(exe),
		"timeout": int64(codexHookTimeout),
	}}}
}

// isCodexGroup reports whether a matcher group is the agent's: every handler in it runs the agent's
// hook. A group holding a customer's handler too is the customer's.
func isCodexGroup(g any) bool {
	m, ok := g.(map[string]any)
	if !ok {
		return false
	}
	list, ok := m["hooks"].([]any)
	if !ok || len(list) == 0 {
		return false
	}
	for _, h := range list {
		hm, ok := h.(map[string]any)
		if !ok || hm["type"] != "command" {
			return false
		}
		cmd, _ := hm["command"].(string)
		if !isCodexCommand(cmd) {
			return false
		}
	}
	return true
}

// Codex is the Codex CLI's system requirements file, the administrator's layer that a user's
// configuration cannot loosen. The agent owns its UserPromptSubmit matcher group under hooks, the
// hooks feature pinned on (a user could otherwise switch hooks off) and allow_managed_hooks_only;
// every other key and group is the customer's. Hooks declared here run whatever a user's hook
// settings say. A rewrite drops the file's comments, which Remove puts back with the original
// bytes. Only the hook fields of a Desired apply.
type Codex struct {
	path      string
	backup    backup
	files     managedFiles
	installed func() bool
}

// NewCodexWriter returns the writer for the requirements file at path, or at the platform's system
// location when path is empty. installed reports whether the Codex CLI is installed; nil reports
// that it is not. Its backup is kept in dir.
func NewCodexWriter(dir state.Dir, path string, installed func() bool) *Codex {
	if path == "" {
		path = codexRequirementsPath()
	}
	if installed == nil {
		installed = func() bool { return false }
	}
	return &Codex{
		path:      path,
		backup:    backupFor(dir, CodexTool),
		files:     systemFiles{},
		installed: installed,
	}
}

// Path implements Writer.
func (c *Codex) Path() string { return c.path }

// Installed implements Writer.
func (c *Codex) Installed() bool { return c.installed() }

// parseCodex reads a requirements file into a document whose arrays are []any, and checks the parts
// the agent merges into: hooks and features are tables, and hooks.UserPromptSubmit is an array of
// tables. An empty file is an empty document.
func parseCodex(data []byte) (map[string]any, error) {
	doc := map[string]any{}
	if _, err := toml.NewDecoder(bytes.NewReader(data)).Decode(&doc); err != nil {
		return nil, errors.New("toolconfig: the requirements file is not TOML")
	}
	doc = normalizeTOML(doc).(map[string]any)
	for _, key := range []string{"hooks", "features"} {
		if v, ok := doc[key]; ok {
			if _, ok := v.(map[string]any); !ok {
				return nil, fmt.Errorf("toolconfig: the requirements file's %s is not a table", key)
			}
		}
	}
	if h, ok := doc["hooks"].(map[string]any); ok {
		if v, ok := h[hooks.CodexUserPromptSubmit]; ok {
			list, ok := v.([]any)
			if !ok {
				return nil, errors.New("toolconfig: the requirements file's hooks.UserPromptSubmit is not an array")
			}
			for _, g := range list {
				if _, ok := g.(map[string]any); !ok {
					return nil, errors.New("toolconfig: the requirements file's hooks.UserPromptSubmit is not an array of tables")
				}
			}
		}
	}
	return doc, nil
}

// normalizeTOML turns the decoder's []map[string]any (an array of tables) into []any, as an inline
// array decodes, so a document compares equal however its arrays were written.
func normalizeTOML(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, e := range t {
			t[k] = normalizeTOML(e)
		}
		return t
	case []map[string]any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = normalizeTOML(e)
		}
		return out
	case []any:
		for i, e := range t {
			t[i] = normalizeTOML(e)
		}
		return t
	default:
		return v
	}
}

// encodeCodex writes a document as TOML.
func encodeCodex(doc map[string]any) ([]byte, error) {
	var b bytes.Buffer
	enc := toml.NewEncoder(&b)
	enc.Indent = ""
	if err := enc.Encode(doc); err != nil {
		return nil, fmt.Errorf("toolconfig: encoding the requirements file: %w", err)
	}
	return b.Bytes(), nil
}

// codexDoc parses the original file; one that was absent is empty.
func (o original) codexDoc() (map[string]any, error) {
	if !o.Present {
		return map[string]any{}, nil
	}
	return parseCodex(o.Content)
}

func (c *Codex) read() (map[string]any, []byte, bool, error) {
	if c.path == "" {
		return nil, nil, false, errors.New("toolconfig: the Codex CLI has no system requirements location on this platform")
	}
	raw, present, err := c.files.read(c.path)
	if err != nil {
		return nil, nil, false, fmt.Errorf("toolconfig: reading %s: %w", c.path, err)
	}
	doc, err := parseCodex(raw)
	if err != nil {
		return nil, nil, false, err
	}
	return doc, raw, present, nil
}

// tomlTable returns doc[key] as a table, a new one when it is absent.
func tomlTable(doc map[string]any, key string) map[string]any {
	if t, ok := doc[key].(map[string]any); ok {
		return t
	}
	return map[string]any{}
}

// putTOMLTable stores t at doc[key], or takes the key out when t is empty and orig has no such key.
func putTOMLTable(doc, orig map[string]any, key string, t map[string]any) {
	if _, had := orig[key]; len(t) == 0 && !had {
		delete(doc, key)
		return
	}
	doc[key] = t
}

// restoreKey gives t[key] orig's value, or takes it out when orig has none.
func restoreKey(t, orig map[string]any, key string) {
	if v, ok := orig[key]; ok {
		t[key] = v
	} else {
		delete(t, key)
	}
}

// mergeCodex puts the agent's UserPromptSubmit group where an earlier one was, or else after the
// customer's, pins the hooks feature on and sets allow_managed_hooks_only as d asks, or, without
// d.Hooks, takes the group out and gives the two keys the original's values. An array, a table or a
// key the agent leaves as the original lacked it goes.
func mergeCodex(doc, orig map[string]any, d Desired) {
	h := tomlTable(doc, "hooks")
	origHooks := tomlTable(orig, "hooks")
	list, _ := h[hooks.CodexUserPromptSubmit].([]any)
	var out []any
	placed := false
	for _, g := range list {
		if !isCodexGroup(g) {
			out = append(out, g)
			continue
		}
		if d.Hooks && !placed {
			out = append(out, codexGroup(d.HookCommand))
			placed = true
		}
	}
	if d.Hooks && !placed {
		out = append(out, codexGroup(d.HookCommand))
	}
	if _, had := origHooks[hooks.CodexUserPromptSubmit]; len(out) == 0 && !had {
		delete(h, hooks.CodexUserPromptSubmit)
	} else {
		if out == nil {
			out = []any{}
		}
		h[hooks.CodexUserPromptSubmit] = out
	}
	putTOMLTable(doc, orig, "hooks", h)

	f := tomlTable(doc, "features")
	if d.Hooks {
		f["hooks"] = true
	} else {
		restoreKey(f, tomlTable(orig, "features"), "hooks")
	}
	putTOMLTable(doc, orig, "features", f)

	if d.Hooks && d.ManagedOnly {
		doc["allow_managed_hooks_only"] = true
	} else {
		restoreKey(doc, orig, "allow_managed_hooks_only")
	}
}

// Apply implements Writer. A file that is not TOML, or whose hooks or features is not a table or
// whose hooks.UserPromptSubmit is not an array of tables, is left as it is.
func (c *Codex) Apply(d Desired) error {
	if d.Hooks && d.HookCommand == "" {
		return errors.New("toolconfig: the agent's executable is unknown, so its hooks cannot be declared")
	}
	doc, raw, present, err := c.read()
	if err != nil {
		return err
	}
	before, err := parseCodex(raw)
	if err != nil {
		return err
	}
	orig, kept, err := c.backup.load()
	if err != nil {
		return err
	}
	if !kept {
		orig = original{Present: present, Content: raw}
	}
	origDoc, err := orig.codexDoc()
	if err != nil {
		return err
	}
	mergeCodex(doc, origDoc, d)
	if err := c.backup.takeOnce(original{Present: present, Content: raw}); err != nil {
		return err
	}
	if (present && reflect.DeepEqual(doc, before)) || (!present && len(doc) == 0) {
		return nil
	}
	out, err := encodeCodex(doc)
	if err != nil {
		return err
	}
	if err := c.files.write(c.path, out); err != nil {
		return fmt.Errorf("toolconfig: writing %s: %w", c.path, err)
	}
	return nil
}

// Holds implements Writer.
func (c *Codex) Holds(d Desired) (bool, error) {
	doc, _, present, err := c.read()
	if err != nil || !present {
		return false, err
	}
	if !d.Hooks {
		return true, nil
	}
	want := codexGroup(d.HookCommand)
	found := false
	list, _ := tomlTable(doc, "hooks")[hooks.CodexUserPromptSubmit].([]any)
	for _, g := range list {
		found = found || reflect.DeepEqual(g, want)
	}
	if !found || tomlTable(doc, "features")["hooks"] != true {
		return false, nil
	}
	return !d.ManagedOnly || doc["allow_managed_hooks_only"] == true, nil
}

// Remove implements Writer. The agent's group is taken out and the keys it set get the backup's
// values; an array or a table left as the backup had it (absent) goes too. When the result holds
// what the backup does, the backup's bytes are written back as they were, comments included; a file
// that did not exist before and holds nothing else is deleted. The backup is dropped once the file
// is restored.
func (c *Codex) Remove() error {
	orig, ok, err := c.backup.load()
	if err != nil || !ok {
		return err
	}
	doc, raw, present, err := c.read()
	if err != nil {
		return err
	}
	if !present {
		return c.backup.drop()
	}
	origDoc, err := orig.codexDoc()
	if err != nil {
		return err
	}
	mergeCodex(doc, origDoc, Desired{})

	var out []byte
	switch {
	case orig.Present && reflect.DeepEqual(doc, origDoc):
		out = orig.Content
	case !orig.Present && len(doc) == 0:
		if err := c.files.remove(c.path); err != nil {
			return fmt.Errorf("toolconfig: deleting %s: %w", c.path, err)
		}
		return c.backup.drop()
	default:
		if out, err = encodeCodex(doc); err != nil {
			return err
		}
	}
	if !bytes.Equal(out, raw) {
		if err := c.files.write(c.path, out); err != nil {
			return fmt.Errorf("toolconfig: writing %s: %w", c.path, err)
		}
	}
	return c.backup.drop()
}

// codexTool describes the Codex CLI to its provider: hooks, with a managed-only setting. Its OTel
// export is not configured by this provider, so its OTel switch does nothing here.
var codexTool = tool{
	key:         CodexTool,
	collector:   protocol.CollectorToolConfigCodex,
	fingerprint: CodexFingerprint,
	supported:   codexSupported,
	managedOnly: true,
}

// NewCodex returns the tool_config_codex collector over w.
func NewCodex(w Writer, cfg Config) *Provider { return newProvider(codexTool, w, cfg) }
