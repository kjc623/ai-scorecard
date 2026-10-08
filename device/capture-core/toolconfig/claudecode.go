package toolconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/shadow-ai-capture/device/capture-core/state"
	"github.com/shadow-ai-capture/device/protocol"
)

// ClaudeCodeTool is Claude Code's endpoint.tools key, and its backup folder's name.
const ClaudeCodeTool = "claude_code"

// ClaudeCodeFingerprint is Claude Code's catalog fingerprint, which its collection mode resolves for.
const ClaudeCodeFingerprint = "app:claude_code"

// The environment variables the agent owns in the managed settings' env object. Claude Code reads
// an env variable from the highest managed source that sets it, so a user's own settings cannot
// override them. OTEL_LOG_USER_PROMPTS also turns on assistant response logging unless
// OTEL_LOG_ASSISTANT_RESPONSES is set, so that one is pinned off: the product records prompts only.
var claudeCodeKeys = []string{
	"CLAUDE_CODE_ENABLE_TELEMETRY",
	"OTEL_LOGS_EXPORTER",
	"OTEL_METRICS_EXPORTER",
	"OTEL_EXPORTER_OTLP_PROTOCOL",
	"OTEL_EXPORTER_OTLP_ENDPOINT",
	"OTEL_EXPORTER_OTLP_HEADERS",
	"OTEL_LOG_USER_PROMPTS",
	"OTEL_LOG_ASSISTANT_RESPONSES",
}

// claudeCodeEnv is the value of each of claudeCodeKeys for d, in that order.
func claudeCodeEnv(d Desired) []string {
	prompts := "0"
	if d.LogPrompts {
		prompts = "1"
	}
	return []string{
		"1",
		"otlp",
		"otlp",
		"http/protobuf",
		"http://" + d.HTTPListen,
		"Authorization=Bearer " + d.Token,
		prompts,
		"0",
	}
}

// The hook events the agent declares in the managed settings' hooks object, with the tools each
// runs for. UserPromptSubmit takes no matcher. PowerShell is matched beside Bash because on Windows
// it is the shell tool Claude Code runs commands through, and without Git Bash the only one. The
// matcher is a regular expression Claude Code tests unanchored, hence the anchors.
var claudeCodeHooks = []struct{ event, matcher string }{
	{"UserPromptSubmit", ""},
	{"PreToolUse", "^(Bash|PowerShell|WebFetch|mcp__.*)$"},
}

// claudeCodeHookTimeout is each hook's timeout in seconds, Claude Code's unit. The hook answers, or
// fails open, within 400 ms.
const claudeCodeHookTimeout = 1

// allowManagedHooksOnlyKey is the managed setting that lets only managed hooks run.
const allowManagedHooksOnlyKey = "allowManagedHooksOnly"

// hookHandler is a command hook. With args set Claude Code spawns command directly, without a
// shell, so the installed path needs no quoting for Git Bash or PowerShell.
type hookHandler struct {
	Type    string   `json:"type"`
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
	Timeout int      `json:"timeout,omitempty"`
}

type hookGroup struct {
	Matcher string        `json:"matcher,omitempty"`
	Hooks   []hookHandler `json:"hooks"`
}

// agentHookGroup is the matcher group that runs `<command> --hook claude_code <event>`.
func agentHookGroup(event, matcher, command string) (json.RawMessage, error) {
	return marshalCompact(hookGroup{
		Matcher: matcher,
		Hooks: []hookHandler{{
			Type:    "command",
			Command: command,
			Args:    []string{"--hook", ClaudeCodeTool, event},
			Timeout: claudeCodeHookTimeout,
		}},
	})
}

// isAgentGroup reports whether a matcher group holds only command hooks running the agent's
// `capture-core --hook claude_code`, from wherever the agent is or was installed.
func isAgentGroup(raw json.RawMessage) bool {
	var g struct {
		Hooks []json.RawMessage `json:"hooks"`
	}
	if json.Unmarshal(raw, &g) != nil || len(g.Hooks) == 0 {
		return false
	}
	for _, hr := range g.Hooks {
		var h hookHandler
		if json.Unmarshal(hr, &h) != nil || h.Type != "command" || len(h.Args) < 2 || h.Args[0] != "--hook" || h.Args[1] != ClaudeCodeTool {
			return false
		}
		base := strings.ToLower(h.Command[strings.LastIndexAny(h.Command, `/\`)+1:])
		if strings.TrimSuffix(base, ".exe") != "capture-core" {
			return false
		}
	}
	return true
}

// utf8BOM is kept when a managed file starts with it, as files saved by Windows tools often do.
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// ClaudeCode is Claude Code's machine-wide managed settings file. The agent owns claudeCodeKeys in
// its env object, its own entries in the hooks object, and allowManagedHooksOnly, and nothing else.
type ClaudeCode struct {
	path      string
	backup    backup
	files     managedFiles
	installed func() bool
}

// NewClaudeCodeWriter returns the writer for the managed settings file at path, or at the platform's
// managed location when path is empty. Its backup is kept in dir.
func NewClaudeCodeWriter(dir state.Dir, path string) *ClaudeCode {
	if path == "" {
		path = claudeCodeManagedPath()
	}
	return &ClaudeCode{
		path:      path,
		backup:    backupFor(dir, ClaudeCodeTool),
		files:     systemFiles{},
		installed: claudeCodeInstalled,
	}
}

// Path implements Writer.
func (c *ClaudeCode) Path() string { return c.path }

// Installed implements Writer: Claude Code is found at one of its documented install locations.
func (c *ClaudeCode) Installed() bool { return c.installed() }

// settings is a parsed managed settings file.
type settings struct {
	bom bool
	doc *object
}

func parseSettings(data []byte) (settings, error) {
	s := settings{bom: bytes.HasPrefix(data, utf8BOM)}
	doc, err := parseObject(bytes.TrimPrefix(data, utf8BOM))
	if err != nil {
		return settings{}, fmt.Errorf("toolconfig: the managed settings are not a JSON object: %w", err)
	}
	s.doc = doc
	return s, nil
}

// settings parses the original file; one that was absent is empty.
func (o original) settings() (settings, error) {
	if !o.Present {
		return settings{doc: &object{}}, nil
	}
	return parseSettings(o.Content)
}

// env is the settings' env object; an absent or null env is empty.
func (s settings) env() (*object, error) {
	env, err := s.objectAt("env")
	if err != nil {
		return nil, errors.New("toolconfig: the managed settings' env is not a JSON object")
	}
	return env, nil
}

// hooks is the settings' hooks object; an absent or null hooks is empty.
func (s settings) hooks() (*object, error) {
	h, err := s.objectAt("hooks")
	if err != nil {
		return nil, errors.New("toolconfig: the managed settings' hooks is not a JSON object")
	}
	return h, nil
}

// objectAt is the member key as an object; an absent or null member is empty.
func (s settings) objectAt(key string) (*object, error) {
	raw, ok := s.doc.get(key)
	if !ok || isNull(raw) {
		return &object{}, nil
	}
	return parseObject(raw)
}

func (s settings) bytes() ([]byte, error) {
	out, err := s.doc.indented()
	if err != nil {
		return nil, err
	}
	if s.bom {
		out = append(append([]byte(nil), utf8BOM...), out...)
	}
	return out, nil
}

func (c *ClaudeCode) read() (settings, []byte, bool, error) {
	if c.path == "" {
		return settings{}, nil, false, errors.New("toolconfig: Claude Code has no managed settings location on this platform")
	}
	raw, present, err := c.files.read(c.path)
	if err != nil {
		return settings{}, nil, false, fmt.Errorf("toolconfig: reading %s: %w", c.path, err)
	}
	s, err := parseSettings(raw)
	if err != nil {
		return settings{}, nil, false, err
	}
	return s, raw, present, nil
}

// merge sets the agent's keys in s to what d asks for. A key d does not ask for goes back to what
// orig, the file before the agent's first write, holds for it, or is taken out when orig has none.
// Every other key, the customer's env variables and hook entries included, keeps its value.
func merge(s, orig settings, d Desired) error {
	if err := mergeEnv(s, orig, d); err != nil {
		return err
	}
	if err := mergeHooks(s, orig, d); err != nil {
		return err
	}
	if d.Hooks && d.ManagedOnly {
		s.doc.set(allowManagedHooksOnlyKey, json.RawMessage("true"))
	} else {
		restore(s.doc, orig.doc, allowManagedHooksOnlyKey)
	}
	return nil
}

// restore sets key in doc to its value in orig, or deletes it when orig has none.
func restore(doc, orig *object, key string) {
	if v, ok := orig.get(key); ok {
		doc.set(key, v)
	} else {
		doc.delete(key)
	}
}

// mergeEnv sets or restores the telemetry variables. An env object whose variables do not change
// is left as written.
func mergeEnv(s, orig settings, d Desired) error {
	env, err := s.env()
	if err != nil {
		return err
	}
	origEnv, err := orig.env()
	if err != nil {
		return err
	}
	values := claudeCodeEnv(d)
	changed := false
	for i, k := range claudeCodeKeys {
		before, had := env.get(k)
		if d.OTel {
			q, err := marshalString(values[i])
			if err != nil {
				return err
			}
			env.set(k, q)
		} else {
			restore(env, origEnv, k)
		}
		after, has := env.get(k)
		changed = changed || had != has || !bytes.Equal(before, after)
	}
	if !changed {
		return nil
	}
	if _, origHasEnv := orig.doc.get("env"); env.empty() && !origHasEnv {
		s.doc.delete("env")
		return nil
	}
	e, err := env.compact()
	if err != nil {
		return err
	}
	s.doc.set("env", e)
	return nil
}

// mergeHooks puts the agent's matcher group in each of its events' lists, where an earlier one was
// or else at the end, or takes it out. The customer's groups keep their order and their values.
func mergeHooks(s, orig settings, d Desired) error {
	hooks, err := s.hooks()
	if err != nil {
		return err
	}
	origHooks, err := orig.hooks()
	if err != nil {
		origHooks = &object{}
	}
	changed := false
	for _, h := range claudeCodeHooks {
		raw, had := hooks.get(h.event)
		groups, err := parseArray(raw)
		if err != nil {
			return fmt.Errorf("toolconfig: the managed settings' %s hooks are not a JSON array", h.event)
		}
		var want json.RawMessage
		if d.Hooks {
			if want, err = agentHookGroup(h.event, h.matcher, d.HookCommand); err != nil {
				return err
			}
		}
		var out []json.RawMessage
		placed := false
		for _, g := range groups {
			if !isAgentGroup(g) {
				out = append(out, g)
				continue
			}
			if want != nil && !placed {
				out = append(out, want)
				placed = true
			}
		}
		if want != nil && !placed {
			out = append(out, want)
		}
		a, err := compactArray(out)
		if err != nil {
			return err
		}
		if (len(out) == 0 && (!had || isNull(raw))) || (had && equalJSON(raw, a)) {
			continue
		}
		changed = true
		if _, origHas := origHooks.get(h.event); len(out) == 0 && !origHas {
			hooks.delete(h.event)
		} else {
			hooks.set(h.event, a)
		}
	}
	if !changed {
		return nil
	}
	if _, origHasHooks := orig.doc.get("hooks"); hooks.empty() && !origHasHooks {
		s.doc.delete("hooks")
		return nil
	}
	c, err := hooks.compact()
	if err != nil {
		return err
	}
	s.doc.set("hooks", c)
	return nil
}

// holds reports whether s carries the agent's keys for what d asks for.
func holds(s settings, d Desired) bool {
	if d.OTel {
		env, err := s.env()
		if err != nil {
			return false
		}
		for i, v := range claudeCodeEnv(d) {
			raw, ok := env.get(claudeCodeKeys[i])
			if !ok {
				return false
			}
			var got string
			if json.Unmarshal(raw, &got) != nil || got != v {
				return false
			}
		}
	}
	if !d.Hooks {
		return true
	}
	hooks, err := s.hooks()
	if err != nil {
		return false
	}
	for _, h := range claudeCodeHooks {
		raw, _ := hooks.get(h.event)
		groups, err := parseArray(raw)
		if err != nil {
			return false
		}
		want, err := agentHookGroup(h.event, h.matcher, d.HookCommand)
		if err != nil {
			return false
		}
		found := false
		for _, g := range groups {
			found = found || equalJSON(g, want)
		}
		if !found {
			return false
		}
	}
	if d.ManagedOnly {
		v, ok := s.doc.get(allowManagedHooksOnlyKey)
		return ok && equalJSON(v, json.RawMessage("true"))
	}
	return true
}

// Apply implements Writer. A file that is not a JSON object, or whose env or hooks is not one, is
// left as it is: Claude Code refuses to start on it, and overwriting it would lose what it holds.
func (c *ClaudeCode) Apply(d Desired) error {
	if d.Hooks && d.HookCommand == "" {
		return errors.New("toolconfig: the agent's executable is unknown, so its hooks cannot be declared")
	}
	s, raw, present, err := c.read()
	if err != nil {
		return err
	}
	before, err := parseSettings(raw)
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
	origSettings, err := orig.settings()
	if err != nil {
		return err
	}
	if err := merge(s, origSettings, d); err != nil {
		return err
	}
	if err := c.backup.takeOnce(original{Present: present, Content: raw}); err != nil {
		return err
	}
	if present && sameJSON(s.doc, before.doc) {
		return nil
	}
	out, err := s.bytes()
	if err != nil {
		return err
	}
	if err := c.files.write(c.path, out); err != nil {
		return fmt.Errorf("toolconfig: writing %s: %w", c.path, err)
	}
	return nil
}

// Holds implements Writer.
func (c *ClaudeCode) Holds(d Desired) (bool, error) {
	s, _, present, err := c.read()
	if err != nil || !present {
		return false, err
	}
	return holds(s, d), nil
}

// Remove implements Writer. Each agent key goes back to the value the backup holds for it, or is
// deleted when the backup has none, and the agent's hook entries are taken out. When the result
// holds what the backup does, the backup's bytes are written back as they were; a file that did not
// exist before and holds nothing else is deleted. The backup is dropped once the file is restored.
func (c *ClaudeCode) Remove() error {
	orig, ok, err := c.backup.load()
	if err != nil || !ok {
		return err
	}
	s, raw, present, err := c.read()
	if err != nil {
		return err
	}
	if !present {
		return c.backup.drop()
	}
	origSettings, err := orig.settings()
	if err != nil {
		return err
	}
	if err := merge(s, origSettings, Desired{}); err != nil {
		return err
	}

	var out []byte
	switch {
	case orig.Present && sameJSON(s.doc, origSettings.doc):
		out = orig.Content
	case !orig.Present && s.doc.empty():
		if err := c.files.remove(c.path); err != nil {
			return fmt.Errorf("toolconfig: deleting %s: %w", c.path, err)
		}
		return c.backup.drop()
	default:
		if out, err = s.bytes(); err != nil {
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

// claudeCodeTool describes Claude Code to its provider.
var claudeCodeTool = tool{
	key:         ClaudeCodeTool,
	collector:   protocol.CollectorToolConfigClaudeCode,
	fingerprint: ClaudeCodeFingerprint,
	supported:   claudeCodeSupported,
}
