package winproxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/shadow-ai-capture/device/capture-core/state"
)

// StateFile is the record file's name in the agent's state directory.
const StateFile = "pac-originals.json"

// record is one user's AutoConfigURL as it was before the PAC was applied, and the URL applied.
type record struct {
	HadOriginal bool   `json:"had_original"`
	Original    string `json:"original,omitempty"`
	Applied     string `json:"applied"`
}

func (r record) restore() Restore { return Restore{hadOriginal: r.HadOriginal, original: r.Original} }

// records keeps each applied user's record in a file in the state directory until the user's
// AutoConfigURL is restored, so an original survives a crash, a user whose hive was unloaded before
// the restore, and the end of the process. A records with no path keeps nothing.
type records struct {
	path string
	mu   sync.Mutex
}

func (r *records) read() (map[string]record, error) {
	out := map[string]record{}
	raw, err := os.ReadFile(r.path)
	if errors.Is(err, fs.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, fmt.Errorf("winproxy: reading %s: %w", r.path, err)
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("winproxy: %s is unreadable: %w", r.path, err)
	}
	return out, nil
}

func (r *records) write(all map[string]record) error {
	if len(all) == 0 {
		if err := os.Remove(r.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("winproxy: deleting %s: %w", r.path, err)
		}
		return nil
	}
	raw, err := json.Marshal(all)
	if err != nil {
		return err
	}
	if err := state.MkdirProtected(filepath.Dir(r.path)); err != nil {
		return fmt.Errorf("winproxy: creating %s: %w", filepath.Dir(r.path), err)
	}
	if err := state.WriteFile(r.path, raw); err != nil {
		return fmt.Errorf("winproxy: writing %s: %w", r.path, err)
	}
	return nil
}

// get returns the user's record, and false when none is kept.
func (r *records) get(sid string) (record, bool, error) {
	if r == nil || r.path == "" {
		return record{}, false, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	all, err := r.read()
	if err != nil {
		return record{}, false, err
	}
	rec, ok := all[sid]
	return rec, ok, nil
}

// put keeps rec for the user.
func (r *records) put(sid string, rec record) error {
	if r == nil || r.path == "" {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	all, err := r.read()
	if err != nil {
		return err
	}
	if cur, ok := all[sid]; ok && cur == rec {
		return nil
	}
	all[sid] = rec
	return r.write(all)
}

// drop forgets the user's record once their AutoConfigURL is restored.
func (r *records) drop(sid string) error {
	if r == nil || r.path == "" {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	all, err := r.read()
	if err != nil {
		return err
	}
	if _, ok := all[sid]; !ok {
		return nil
	}
	delete(all, sid)
	return r.write(all)
}

// RestoreRecorded puts back the AutoConfigURL of every user the record file at path holds, when it
// still names the PAC the agent applied, and forgets each user it is done with: restored, or found
// holding another value, which is someone else's change. A user whose settings cannot be opened
// (their hive is not loaded) is kept and reported. open is the user settings seam; nil opens the
// platform's. It is the uninstall path for a PAC the running service did not restore.
func RestoreRecorded(path string, open func(sid string) (Registry, error)) (restored int, err error) {
	if open == nil {
		open = openInternetSettings
	}
	r := &records{path: path}
	all, err := r.read()
	if err != nil {
		return 0, err
	}
	sids := make([]string, 0, len(all))
	for sid := range all {
		sids = append(sids, sid)
	}
	sort.Strings(sids)
	var errs []error
	for _, sid := range sids {
		ok, err := restoreRecord(sid, all[sid], open)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if ok {
			restored++
		}
		delete(all, sid)
	}
	if err := r.write(all); err != nil {
		errs = append(errs, err)
	}
	return restored, errors.Join(errs...)
}

// restoreRecord restores one user when their AutoConfigURL is still the applied one, and reports
// whether it did.
func restoreRecord(sid string, rec record, open func(sid string) (Registry, error)) (bool, error) {
	reg, err := open(sid)
	if err != nil {
		return false, fmt.Errorf("winproxy: %s: %w", sid, err)
	}
	defer reg.Close()
	cur, ok, err := reg.GetString(autoConfigURL)
	if err != nil {
		return false, fmt.Errorf("winproxy: %s: reading AutoConfigURL: %w", sid, err)
	}
	if !ok || cur != rec.Applied {
		return false, nil
	}
	if err := NewSettings(reg).Restore(rec.restore()); err != nil {
		return false, fmt.Errorf("winproxy: %s: restoring AutoConfigURL: %w", sid, err)
	}
	return true, nil
}
