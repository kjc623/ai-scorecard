package toolconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/shadow-ai-capture/device/capture-core/hooks"
	"github.com/shadow-ai-capture/device/capture-core/state"
	"github.com/shadow-ai-capture/device/protocol"
)

// CursorTool is Cursor's endpoint.tools key, and its backup folder's name.
const CursorTool = "cursor"

// CursorFingerprint is Cursor's catalog fingerprint.
const CursorFingerprint = "app:cursor"

// cursorEvents are the hook events the agent declares, in the order it adds them.
var cursorEvents = []string{hooks.CursorBeforeSubmitPrompt, hooks.CursorBeforeMCPExecution}

// cursorVersion is the hooks file schema version the agent writes into a file that names none.
const cursorVersion = "1"

// Cursor is Cursor's enterprise hooks file, which Cursor runs beside the project's and the user's
// hooks. The agent owns one entry per event in cursorEvents, the one whose command runs its own hook,
// and adds the file's version when it has none; every other entry and key is the customer's. Cursor
// has no OTel export, so only the hook fields of a Desired apply.
type Cursor struct {
	path      string
	backup    backup
	files     managedFiles
	installed func() bool
}

// NewCursorWriter returns the writer for the hooks file at path, or at the platform's enterprise
// location when path is empty. installed reports whether Cursor is installed; nil reports that it is
// not. Its backup is kept in dir.
func NewCursorWriter(dir state.Dir, path string, installed func() bool) *Cursor {
	if path == "" {
		path = cursorHooksPath()
	}
	if installed == nil {
		installed = func() bool { return false }
	}
	return &Cursor{
		path:      path,
		backup:    backupFor(dir, CursorTool),
		files:     systemFiles{},
		installed: installed,
	}
}

// Path implements Writer.
func (c *Cursor) Path() string { return c.path }

// Installed implements Writer.
func (c *Cursor) Installed() bool { return c.installed() }

// cursorArgs is what follows the quoted executable in the agent's hook command for event.
func cursorArgs(event string) string { return " --hook " + hooks.CursorTool + " " + event }

// cursorEntry is the hook entry that runs `"<command>" --hook cursor <event>`.
func cursorEntry(event, command string) (json.RawMessage, error) {
	return marshalCompact(struct {
		Command string `json:"command"`
	}{`"` + command + `"` + cursorArgs(event)})
}

// isCursorEntry reports whether a hook entry runs the agent's `capture-core --hook cursor <event>`,
// from wherever the agent is or was installed.
func isCursorEntry(raw json.RawMessage, event string) bool {
	var e struct {
		Command string `json:"command"`
	}
	if json.Unmarshal(raw, &e) != nil {
		return false
	}
	quoted, ok := strings.CutPrefix(e.Command, `"`)
	if !ok {
		return false
	}
	exe, args, ok := strings.Cut(quoted, `"`)
	return ok && args == cursorArgs(event) && isCaptureCore(exe)
}

// cursorFile is a parsed hooks file and its hooks object.
type cursorFile struct {
	settings
	hooks *object
}

// cursorFile parses the original file; one that was absent is empty.
func (o original) cursorFile() (cursorFile, error) {
	if !o.Present {
		return cursorFile{settings: settings{doc: &object{}}, hooks: &object{}}, nil
	}
	return parseCursorFile(o.Content)
}

func (c *Cursor) read() (cursorFile, []byte, bool, error) {
	if c.path == "" {
		return cursorFile{}, nil, false, errors.New("toolconfig: Cursor has no enterprise hooks location on this platform")
	}
	raw, present, err := c.files.read(c.path)
	if err != nil {
		return cursorFile{}, nil, false, fmt.Errorf("toolconfig: reading %s: %w", c.path, err)
	}
	f, err := parseCursorFile(raw)
	if err != nil {
		return cursorFile{}, nil, false, err
	}
	return f, raw, present, nil
}

// parseCursorFile reads a hooks file whose hooks member, when present, is an object of arrays.
func parseCursorFile(data []byte) (cursorFile, error) {
	s, err := parseSettings(data)
	if err != nil {
		return cursorFile{}, errors.New("toolconfig: the hooks file is not a JSON object")
	}
	f := cursorFile{settings: s, hooks: &object{}}
	if raw, ok := s.doc.get("hooks"); ok && !isNull(raw) {
		if f.hooks, err = parseObject(raw); err != nil {
			return cursorFile{}, errors.New("toolconfig: the hooks file's hooks is not a JSON object")
		}
	}
	for _, m := range f.hooks.members {
		if _, err := parseArray(m.value); err != nil {
			return cursorFile{}, fmt.Errorf("toolconfig: the hooks file's %s is not a JSON array", m.key)
		}
	}
	return f, nil
}

// mergeCursor puts the agent's entry in each of cursorEvents' arrays, where an earlier one was or
// else after the customer's, or takes it out, as d asks. With the entries in, a file that names no
// schema version gets one; with them out, a version the original did not have goes when the rest of
// the file is as the original was.
func mergeCursor(f, orig cursorFile, d Desired) error {
	changed := false
	for _, ev := range cursorEvents {
		raw, had := f.hooks.get(ev)
		list, err := parseArray(raw)
		if err != nil {
			return fmt.Errorf("toolconfig: the hooks file's %s is not a JSON array", ev)
		}
		var want json.RawMessage
		if d.Hooks {
			if want, err = cursorEntry(ev, d.HookCommand); err != nil {
				return err
			}
		}
		var out []json.RawMessage
		placed := false
		for _, e := range list {
			if !isCursorEntry(e, ev) {
				out = append(out, e)
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
		if _, origHas := orig.hooks.get(ev); len(out) == 0 && !origHas {
			f.hooks.delete(ev)
		} else {
			f.hooks.set(ev, a)
		}
	}
	if changed {
		if _, origHasHooks := orig.doc.get("hooks"); f.hooks.empty() && !origHasHooks {
			f.doc.delete("hooks")
		} else {
			h, err := f.hooks.compact()
			if err != nil {
				return err
			}
			f.doc.set("hooks", h)
		}
	}
	_, hasVersion := f.doc.get("version")
	_, origHasVersion := orig.doc.get("version")
	switch {
	case d.Hooks && !hasVersion:
		f.doc.set("version", json.RawMessage(cursorVersion))
	case !d.Hooks && hasVersion && !origHasVersion:
		without := &object{members: append([]member(nil), f.doc.members...)}
		without.delete("version")
		if sameJSON(without, orig.doc) {
			f.doc.delete("version")
		}
	}
	return nil
}

// Apply implements Writer. A file that is not a JSON object, or whose hooks are not an object of
// arrays, is left as it is.
func (c *Cursor) Apply(d Desired) error {
	if d.Hooks && d.HookCommand == "" {
		return errors.New("toolconfig: the agent's executable is unknown, so its hooks cannot be declared")
	}
	f, raw, present, err := c.read()
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
	origFile, err := orig.cursorFile()
	if err != nil {
		return err
	}
	if err := mergeCursor(f, origFile, d); err != nil {
		return err
	}
	if err := c.backup.takeOnce(original{Present: present, Content: raw}); err != nil {
		return err
	}
	if (present && sameJSON(f.doc, before.doc)) || (!present && f.doc.empty()) {
		return nil
	}
	out, err := f.bytes()
	if err != nil {
		return err
	}
	if err := c.files.write(c.path, out); err != nil {
		return fmt.Errorf("toolconfig: writing %s: %w", c.path, err)
	}
	return nil
}

// Holds implements Writer.
func (c *Cursor) Holds(d Desired) (bool, error) {
	f, _, present, err := c.read()
	if err != nil || !present {
		return false, err
	}
	if !d.Hooks {
		return true, nil
	}
	for _, ev := range cursorEvents {
		want, err := cursorEntry(ev, d.HookCommand)
		if err != nil {
			return false, err
		}
		raw, _ := f.hooks.get(ev)
		list, _ := parseArray(raw)
		found := false
		for _, e := range list {
			found = found || equalJSON(e, want)
		}
		if !found {
			return false, nil
		}
	}
	return true, nil
}

// Remove implements Writer. The agent's entries are taken out; an event array, the hooks object and
// the version the agent added go too when they are left as the backup had them (absent). When the
// result holds what the backup does, the backup's bytes are written back as they were; a file that
// did not exist before and holds nothing else is deleted. The backup is dropped once the file is
// restored.
func (c *Cursor) Remove() error {
	orig, ok, err := c.backup.load()
	if err != nil || !ok {
		return err
	}
	f, raw, present, err := c.read()
	if err != nil {
		return err
	}
	if !present {
		return c.backup.drop()
	}
	origFile, err := orig.cursorFile()
	if err != nil {
		return err
	}
	if err := mergeCursor(f, origFile, Desired{}); err != nil {
		return err
	}

	var out []byte
	switch {
	case orig.Present && sameJSON(f.doc, origFile.doc):
		out = orig.Content
	case !orig.Present && f.doc.empty():
		if err := c.files.remove(c.path); err != nil {
			return fmt.Errorf("toolconfig: deleting %s: %w", c.path, err)
		}
		return c.backup.drop()
	default:
		if out, err = f.bytes(); err != nil {
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

// cursorTool describes Cursor to its provider: hooks only, with no managed-only setting.
var cursorTool = tool{
	key:         CursorTool,
	collector:   protocol.CollectorToolConfigCursor,
	fingerprint: CursorFingerprint,
	supported:   cursorSupported,
}

// NewCursor returns the tool_config_cursor collector over w.
func NewCursor(w Writer, cfg Config) *Provider { return newProvider(cursorTool, w, cfg) }
