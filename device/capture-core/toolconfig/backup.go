package toolconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/shadow-ai-capture/device/capture-core/state"
)

// backupDir is the state directory's folder for the backups, one subfolder per tool.
const backupDir = "toolconfig"

// original is a managed file as it was before the agent first wrote it.
type original struct {
	Present bool   `json:"present"`
	Content []byte `json:"content,omitempty"`
}

// backup is a tool's toolconfig/<tool>/original file in the state directory.
type backup struct{ path string }

func backupFor(dir state.Dir, tool string) backup {
	return backup{path: dir.Path(backupDir, tool, "original")}
}

// load returns the backup, and false when none is kept.
func (b backup) load() (original, bool, error) {
	raw, err := os.ReadFile(b.path)
	if errors.Is(err, fs.ErrNotExist) {
		return original{}, false, nil
	}
	if err != nil {
		return original{}, false, fmt.Errorf("toolconfig: reading the backup %s: %w", b.path, err)
	}
	var o original
	if err := json.Unmarshal(raw, &o); err != nil {
		return original{}, false, fmt.Errorf("toolconfig: the backup %s is unreadable: %w", b.path, err)
	}
	return o, true, nil
}

// takeOnce records o unless a backup is already kept: the first one is the file as it was before
// the agent touched it, and every later write would only record the agent's own keys.
func (b backup) takeOnce(o original) error {
	if _, ok, err := b.load(); err != nil || ok {
		return err
	}
	raw, err := json.Marshal(o)
	if err != nil {
		return err
	}
	if err := state.MkdirProtected(filepath.Dir(b.path)); err != nil {
		return fmt.Errorf("toolconfig: creating %s: %w", filepath.Dir(b.path), err)
	}
	if err := state.WriteFile(b.path, raw); err != nil {
		return fmt.Errorf("toolconfig: writing the backup %s: %w", b.path, err)
	}
	return nil
}

// drop deletes the backup once the file holds none of the agent's keys, so the next write backs up
// the file as it is then.
func (b backup) drop() error {
	if err := os.Remove(b.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("toolconfig: deleting the backup %s: %w", b.path, err)
	}
	return nil
}
