package toolconfig

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"reflect"

	"github.com/BurntSushi/toml"

	"github.com/shadow-ai-capture/device/capture-core/state"
)

// The [otel] keys the agent owns in Codex's config: the log exporter and prompt logging. Codex's
// trace and metrics exporters, and every other key, stay as the customer set them.
const (
	codexOTelTable    = "otel"
	codexExporterKey  = "exporter"
	codexLogPromptKey = "log_user_prompt"
)

// codexOTelKeys are the agent's keys in the [otel] table.
var codexOTelKeys = []string{codexExporterKey, codexLogPromptKey}

// codexExporter is the log exporter that sends Codex's events to the receiver: OTLP/HTTP with
// protobuf bodies and the bearer token. Codex posts to the endpoint as given, so it names the logs
// path.
func codexExporter(d Desired) map[string]any {
	return map[string]any{
		"otlp-http": map[string]any{
			"endpoint": "http://" + d.HTTPListen + "/v1/logs",
			"protocol": "binary",
			"headers":  map[string]any{"Authorization": "Bearer " + d.Token},
		},
	}
}

// CodexConfig is Codex's system config.toml, where its OTel export is configured: requirements.toml
// has no otel key. It is Codex's lowest config layer, so a user's own config.toml can override it.
// The agent owns the exporter and log_user_prompt keys of its [otel] table and nothing else; only
// the OTel fields of a Desired apply.
type CodexConfig struct {
	path   string
	backup backup
	files  managedFiles
	// userConfigs lists the users' own config.toml files, which Codex lays over this one.
	userConfigs func() []string
}

// NewCodexConfigWriter returns the writer for the config file at path, or at the platform's system
// location when path is empty. userConfigs lists the users' config files that override it; nil is
// the platform's. Its backup is kept in dir, beside the requirements file's.
func NewCodexConfigWriter(dir state.Dir, path string, userConfigs func() []string) *CodexConfig {
	if path == "" {
		path = codexConfigPath()
	}
	if userConfigs == nil {
		userConfigs = codexUserConfigs
	}
	return &CodexConfig{
		path:        path,
		backup:      backup{path: dir.Path(backupDir, CodexTool, "config", "original")},
		files:       systemFiles{},
		userConfigs: userConfigs,
	}
}

// Path is the config file.
func (c *CodexConfig) Path() string { return c.path }

// codexConfigFile is a parsed config.toml.
type codexConfigFile struct {
	bom bool
	doc map[string]any
}

func parseCodexConfig(data []byte) (codexConfigFile, error) {
	f := codexConfigFile{bom: bytes.HasPrefix(data, utf8BOM), doc: map[string]any{}}
	if _, err := toml.NewDecoder(bytes.NewReader(bytes.TrimPrefix(data, utf8BOM))).Decode(&f.doc); err != nil {
		return codexConfigFile{}, errors.New("toolconfig: the Codex config is not TOML")
	}
	f.doc = normalizeTOML(f.doc).(map[string]any)
	return f, nil
}

// codexConfigFile parses the original file; one that was absent is empty.
func (o original) codexConfigFile() (codexConfigFile, error) {
	if !o.Present {
		return codexConfigFile{doc: map[string]any{}}, nil
	}
	return parseCodexConfig(o.Content)
}

// otel is the file's [otel] table, and false when it has none. A value that is not a table is an
// error: Codex refuses to start on it.
func (f codexConfigFile) otel() (map[string]any, bool, error) {
	v, ok := f.doc[codexOTelTable]
	if !ok {
		return nil, false, nil
	}
	t, isTable := v.(map[string]any)
	if !isTable {
		return nil, false, errors.New("toolconfig: the Codex config's otel is not a table")
	}
	return t, true, nil
}

func (f codexConfigFile) bytes() ([]byte, error) {
	out, err := encodeCodex(f.doc)
	if err != nil {
		return nil, err
	}
	if f.bom {
		out = append(append([]byte(nil), utf8BOM...), out...)
	}
	return out, nil
}

func (c *CodexConfig) read() (codexConfigFile, []byte, bool, error) {
	if c.path == "" {
		return codexConfigFile{}, nil, false, errors.New("toolconfig: Codex has no system config location on this platform")
	}
	raw, present, err := c.files.read(c.path)
	if err != nil {
		return codexConfigFile{}, nil, false, fmt.Errorf("toolconfig: reading %s: %w", c.path, err)
	}
	f, err := parseCodexConfig(raw)
	if err != nil {
		return codexConfigFile{}, nil, false, err
	}
	return f, raw, present, nil
}

// mergeCodexOTel sets the agent's [otel] keys in f to what d asks for, or puts back what orig, the
// file before the agent's first write, holds for them. An [otel] table the agent's keys leave empty
// goes when orig had none.
func mergeCodexOTel(f, orig codexConfigFile, d Desired) error {
	otel, had, err := f.otel()
	if err != nil {
		return err
	}
	origOTel, origHad, err := orig.otel()
	if err != nil {
		origOTel, origHad = nil, false
	}
	if d.OTel {
		if !had {
			otel = map[string]any{}
			f.doc[codexOTelTable] = otel
		}
		otel[codexExporterKey] = codexExporter(d)
		otel[codexLogPromptKey] = d.LogPrompts
		return nil
	}
	if !had {
		otel = map[string]any{}
	}
	for _, k := range codexOTelKeys {
		restoreKey(otel, origOTel, k)
	}
	switch {
	case len(otel) == 0 && (!had || !origHad):
		delete(f.doc, codexOTelTable)
	case !had:
		f.doc[codexOTelTable] = otel
	}
	return nil
}

// Apply merges the agent's [otel] keys for d. A file that is not TOML, or whose otel is not a
// table, is left as it is: Codex refuses to start on it, and overwriting it would lose what it
// holds. A rewrite drops the file's comments, which Remove puts back with the original bytes.
func (c *CodexConfig) Apply(d Desired) error {
	f, raw, present, err := c.read()
	if err != nil {
		return err
	}
	before, err := parseCodexConfig(raw)
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
	origFile, err := orig.codexConfigFile()
	if err != nil {
		return err
	}
	if err := mergeCodexOTel(f, origFile, d); err != nil {
		return err
	}
	if err := c.backup.takeOnce(original{Present: present, Content: raw}); err != nil {
		return err
	}
	if (present && reflect.DeepEqual(f.doc, before.doc)) || (!present && len(f.doc) == 0) {
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

// Holds reports whether the file carries the agent's [otel] keys for d.
func (c *CodexConfig) Holds(d Desired) (bool, error) {
	f, _, present, err := c.read()
	if err != nil || !present {
		return false, err
	}
	if !d.OTel {
		return true, nil
	}
	otel, _, err := f.otel()
	if err != nil {
		return false, nil
	}
	return reflect.DeepEqual(otel[codexExporterKey], codexExporter(d)) && otel[codexLogPromptKey] == d.LogPrompts, nil
}

// Remove gives each agent key the backup's value, or takes it out when the backup has none. When
// the result holds what the backup does, the backup's bytes are written back as they were, comments
// included; a file that did not exist before and holds nothing else is deleted. The backup is
// dropped once the file is restored.
func (c *CodexConfig) Remove() error {
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
	origFile, err := orig.codexConfigFile()
	if err != nil {
		return err
	}
	if err := mergeCodexOTel(f, origFile, Desired{}); err != nil {
		return err
	}

	var out []byte
	switch {
	case orig.Present && reflect.DeepEqual(f.doc, origFile.doc):
		out = orig.Content
	case !orig.Present && len(f.doc) == 0:
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

// Overridden reports whether a user's config.toml, its [otel] table laid over this file the way
// Codex merges its layers, leaves a log exporter other than the agent's, so that user's sessions
// export nowhere or somewhere else. The users' files are only read. One that is missing, or that
// Codex could not parse (it would not start), overrides nothing.
func (c *CodexConfig) Overridden(d Desired) bool {
	if !d.OTel || c.userConfigs == nil {
		return false
	}
	want := codexExporter(d)
	for _, path := range c.userConfigs() {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		f, err := parseCodexConfig(raw)
		if err != nil {
			continue
		}
		user, ok := f.doc[codexOTelTable]
		if !ok {
			continue
		}
		effective, isTable := mergeTOML(map[string]any{codexExporterKey: want}, user).(map[string]any)
		if !isTable || !reflect.DeepEqual(effective[codexExporterKey], want) {
			return true
		}
	}
	return false
}

// mergeTOML lays overlay over base as Codex merges its config layers: tables merge key by key, and
// any other value replaces what is under it. base is not changed.
func mergeTOML(base, overlay any) any {
	bt, ok := base.(map[string]any)
	ot, ok2 := overlay.(map[string]any)
	if !ok || !ok2 {
		return overlay
	}
	out := make(map[string]any, len(bt)+len(ot))
	for k, v := range bt {
		out[k] = v
	}
	for k, v := range ot {
		if cur, ok := out[k]; ok {
			out[k] = mergeTOML(cur, v)
		} else {
			out[k] = v
		}
	}
	return out
}

// CodexFiles is the Codex CLI's two system files as one Writer: the requirements file declares the
// agent's prompt hook (the hook fields of a Desired) and the config file points the OTel export at
// the receiver (the OTel fields). Each file is written, checked and restored on its own, with its
// own backup.
type CodexFiles struct {
	requirements *Codex
	config       *CodexConfig
}

// NewCodexFiles returns the Writer over Codex's requirements and config files.
func NewCodexFiles(requirements *Codex, config *CodexConfig) *CodexFiles {
	return &CodexFiles{requirements: requirements, config: config}
}

// Path implements Writer.
func (c *CodexFiles) Path() string { return c.requirements.Path() + " and " + c.config.Path() }

// watchedFiles implements watchedFiles.
func (c *CodexFiles) watchedFiles() []string {
	return []string{c.requirements.Path(), c.config.Path()}
}

// Installed implements Writer.
func (c *CodexFiles) Installed() bool { return c.requirements.Installed() }

// Apply implements Writer: each file takes its part of d, and the file whose part d does not ask
// for is restored as Remove restores it.
func (c *CodexFiles) Apply(d Desired) error {
	var reqErr, cfgErr error
	if d.Hooks {
		reqErr = c.requirements.Apply(d)
	} else {
		reqErr = c.requirements.Remove()
	}
	if d.OTel {
		cfgErr = c.config.Apply(d)
	} else {
		cfgErr = c.config.Remove()
	}
	return errors.Join(reqErr, cfgErr)
}

// Holds implements Writer: each file d asks something of holds it.
func (c *CodexFiles) Holds(d Desired) (bool, error) {
	if d.Hooks {
		if ok, err := c.requirements.Holds(d); err != nil || !ok {
			return false, err
		}
	}
	if d.OTel {
		return c.config.Holds(d)
	}
	return true, nil
}

// Remove implements Writer.
func (c *CodexFiles) Remove() error {
	return errors.Join(c.requirements.Remove(), c.config.Remove())
}

// Overridden implements overridable: only the config file can be overridden by a user.
func (c *CodexFiles) Overridden(d Desired) bool { return c.config.Overridden(d) }
