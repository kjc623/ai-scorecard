package toolconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

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

// utf8BOM is kept when a managed file starts with it, as files saved by Windows tools often do.
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// ClaudeCode is Claude Code's machine-wide managed settings file. The agent owns claudeCodeKeys in
// its env object and nothing else.
type ClaudeCode struct {
	path      string
	backup    backup
	files     managedFiles
	installed func() bool
}

// NewClaudeCodeWriter returns the writer for the managed settings file at path, or at the platform's
// managed location when path is empty. Its backup is kept in dir. installed reports whether Claude
// Code is installed on the device; nil reports that it is not.
func NewClaudeCodeWriter(dir state.Dir, path string, installed func() bool) *ClaudeCode {
	if path == "" {
		path = claudeCodeManagedPath()
	}
	if installed == nil {
		installed = func() bool { return false }
	}
	return &ClaudeCode{
		path:      path,
		backup:    backupFor(dir, ClaudeCodeTool),
		files:     systemFiles{},
		installed: installed,
	}
}

// Path implements Writer.
func (c *ClaudeCode) Path() string { return c.path }

// Installed implements Writer, through the installed seam.
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

// env is the settings' env object; an absent or null env is empty.
func (s settings) env() (*object, error) {
	raw, ok := s.doc.get("env")
	if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return &object{}, nil
	}
	env, err := parseObject(raw)
	if err != nil {
		return nil, errors.New("toolconfig: the managed settings' env is not a JSON object")
	}
	return env, nil
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

// holds reports whether env carries every agent key with its value for d.
func holds(env *object, d Desired) bool {
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
	return true
}

// Apply implements Writer. A file that is not a JSON object, or whose env is not one, is left as it
// is: Claude Code refuses to start on it, and overwriting it would lose what it holds.
func (c *ClaudeCode) Apply(d Desired) error {
	s, raw, present, err := c.read()
	if err != nil {
		return err
	}
	env, err := s.env()
	if err != nil {
		return err
	}
	if err := c.backup.takeOnce(original{Present: present, Content: raw}); err != nil {
		return err
	}
	if present && holds(env, d) {
		return nil
	}
	for i, v := range claudeCodeEnv(d) {
		q, err := marshalString(v)
		if err != nil {
			return err
		}
		env.set(claudeCodeKeys[i], q)
	}
	e, err := env.compact()
	if err != nil {
		return err
	}
	s.doc.set("env", e)
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
	env, err := s.env()
	if err != nil {
		return false, err
	}
	return holds(env, d), nil
}

// Remove implements Writer. Each agent key goes back to the value the backup holds for it, or is
// deleted when the backup has none. When the result holds what the backup does, the backup's bytes
// are written back as they were; a file that did not exist before and holds nothing else is
// deleted. The backup is dropped once the file is restored.
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
	env, err := s.env()
	if err != nil {
		return err
	}
	origEnv, origHasEnv := &object{}, false
	var origSettings settings
	if orig.Present {
		if origSettings, err = parseSettings(orig.Content); err != nil {
			return err
		}
		if origEnv, err = origSettings.env(); err != nil {
			return err
		}
		_, origHasEnv = origSettings.doc.get("env")
	}
	for _, k := range claudeCodeKeys {
		if v, ok := origEnv.get(k); ok {
			env.set(k, v)
		} else {
			env.delete(k)
		}
	}
	if env.empty() && !origHasEnv {
		s.doc.delete("env")
	} else {
		e, err := env.compact()
		if err != nil {
			return err
		}
		s.doc.set("env", e)
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
