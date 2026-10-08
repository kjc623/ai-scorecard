package toolconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

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
// and adds the file's version when it has none; every other entry and key is the customer's.
type Cursor struct {
	path      string
	exe       string
	backup    backup
	files     managedFiles
	installed func() bool
}

// NewCursorWriter returns the writer for the hooks file at path, or at the platform's enterprise
// location when path is empty. exe is the installed capture-core executable the hook command runs,
// and installed reports whether Cursor is installed. Its backup is kept in dir.
func NewCursorWriter(dir state.Dir, path, exe string, installed func() bool) *Cursor {
	if path == "" {
		path = cursorHooksPath()
	}
	if installed == nil {
		installed = func() bool { return false }
	}
	return &Cursor{
		path:      path,
		exe:       exe,
		backup:    backupFor(dir, CursorTool),
		files:     systemFiles{},
		installed: installed,
	}
}

// Path implements Writer.
func (c *Cursor) Path() string { return c.path }

// Installed implements Writer.
func (c *Cursor) Installed() bool { return c.installed() }

// command is the hook command for event: the quoted executable, then the hook arguments.
func (c *Cursor) command(event string) string {
	return `"` + c.exe + `" --hook ` + hooks.CursorTool + " " + event
}

// cursorFile is a parsed hooks file and its hooks object.
type cursorFile struct {
	settings
	hooks *object
}

func (c *Cursor) read() (cursorFile, []byte, bool, error) {
	if c.path == "" {
		return cursorFile{}, nil, false, errors.New("toolconfig: Cursor has no enterprise hooks location on this platform")
	}
	if c.exe == "" {
		return cursorFile{}, nil, false, errors.New("toolconfig: the agent's executable is not known")
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
		if _, err := entries(m.value); err != nil {
			return cursorFile{}, fmt.Errorf("toolconfig: the hooks file's %s is not a JSON array", m.key)
		}
	}
	return f, nil
}

func isNull(raw json.RawMessage) bool { return bytes.Equal(bytes.TrimSpace(raw), []byte("null")) }

// entries reads an event's array of hook entries; an absent or null array is empty.
func entries(raw json.RawMessage) ([]json.RawMessage, error) {
	if len(raw) == 0 || isNull(raw) {
		return nil, nil
	}
	var list []json.RawMessage
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, err
	}
	return list, nil
}

// eventEntries is event's entries in f.
func (f cursorFile) eventEntries(event string) []json.RawMessage {
	raw, _ := f.hooks.get(event)
	list, _ := entries(raw)
	return list
}

// setEntries replaces event's array in f.
func (f cursorFile) setEntries(event string, list []json.RawMessage) error {
	var b bytes.Buffer
	b.WriteByte('[')
	for i, e := range list {
		if i > 0 {
			b.WriteByte(',')
		}
		if err := json.Compact(&b, e); err != nil {
			return err
		}
	}
	b.WriteByte(']')
	f.hooks.set(event, b.Bytes())
	return nil
}

// isEntry reports whether a hook entry runs command.
func isEntry(raw json.RawMessage, command string) bool {
	var e struct {
		Command string `json:"command"`
	}
	return json.Unmarshal(raw, &e) == nil && e.Command == command
}

// hasEntry reports whether one of list runs command.
func hasEntry(list []json.RawMessage, command string) bool {
	for _, e := range list {
		if isEntry(e, command) {
			return true
		}
	}
	return false
}

// holds reports whether every event in cursorEvents has the agent's entry.
func (c *Cursor) holds(f cursorFile) bool {
	for _, ev := range cursorEvents {
		if !hasEntry(f.eventEntries(ev), c.command(ev)) {
			return false
		}
	}
	return true
}

// Apply implements Writer; the hook command depends on nothing in d. The agent's entry is added
// after the customer's for each event that lacks it. A file that is not a JSON object, or whose
// hooks are not an object of arrays, is left as it is.
func (c *Cursor) Apply(Desired) error {
	f, raw, present, err := c.read()
	if err != nil {
		return err
	}
	if err := c.backup.takeOnce(original{Present: present, Content: raw}); err != nil {
		return err
	}
	if present && c.holds(f) {
		return nil
	}
	if _, ok := f.doc.get("version"); !ok {
		f.doc.set("version", json.RawMessage(cursorVersion))
	}
	for _, ev := range cursorEvents {
		list := f.eventEntries(ev)
		if hasEntry(list, c.command(ev)) {
			continue
		}
		cmd, err := marshalString(c.command(ev))
		if err != nil {
			return err
		}
		entry := append(append([]byte(`{"command":`), cmd...), '}')
		if err := f.setEntries(ev, append(list, entry)); err != nil {
			return err
		}
	}
	h, err := f.hooks.compact()
	if err != nil {
		return err
	}
	f.doc.set("hooks", h)
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
func (c *Cursor) Holds(Desired) (bool, error) {
	f, _, present, err := c.read()
	if err != nil || !present {
		return false, err
	}
	return c.holds(f), nil
}

// Remove implements Writer. The agent's entries are taken out; an event array, the hooks object
// and the version the agent added go too when they are left as the backup had them (absent). When
// the result holds what the backup does, the backup's bytes are written back as they were; a file
// that did not exist before and holds nothing else is deleted. The backup is dropped once the file
// is restored.
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
	origFile := cursorFile{settings: settings{doc: &object{}}, hooks: &object{}}
	if orig.Present {
		if origFile, err = parseCursorFile(orig.Content); err != nil {
			return err
		}
	}
	_, origHasHooks := origFile.doc.get("hooks")
	_, origHasVersion := origFile.doc.get("version")

	for _, ev := range cursorEvents {
		list := f.eventEntries(ev)
		if !hasEntry(list, c.command(ev)) {
			continue
		}
		var kept []json.RawMessage
		for _, e := range list {
			if !isEntry(e, c.command(ev)) {
				kept = append(kept, e)
			}
		}
		if _, origHasEvent := origFile.hooks.get(ev); len(kept) == 0 && !origHasEvent {
			f.hooks.delete(ev)
		} else if err := f.setEntries(ev, kept); err != nil {
			return err
		}
	}
	if f.hooks.empty() && !origHasHooks {
		f.doc.delete("hooks")
	} else {
		h, err := f.hooks.compact()
		if err != nil {
			return err
		}
		f.doc.set("hooks", h)
	}
	if !origHasVersion {
		without := &object{members: append([]member(nil), f.doc.members...)}
		without.delete("version")
		if sameJSON(without, origFile.doc) {
			f.doc = without
		}
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

// cursorTool describes Cursor to its provider.
var cursorTool = tool{
	key:         CursorTool,
	collector:   protocol.CollectorToolConfigCursor,
	fingerprint: CursorFingerprint,
	supported:   cursorSupported,
	hooks:       true,
}

// NewCursor returns the tool_config_cursor collector over w.
func NewCursor(w Writer, cfg Config) *Provider { return newProvider(cursorTool, w, cfg) }
