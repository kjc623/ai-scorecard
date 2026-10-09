package toolconfig

import (
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/shadow-ai-capture/device/capture-core/core"
)

// Drift watch timings: a change is checked once it has been quiet for driftDebounce, and every
// running provider is checked every driftBackstop in case a notification was missed.
const (
	driftDebounce = 250 * time.Millisecond
	driftBackstop = 60 * time.Second
)

// watchedFiles is a Writer that owns more than one file; Path names them all for the log.
type watchedFiles interface {
	watchedFiles() []string
}

// watchedRegistry is a Writer whose configuration is registry values.
type watchedRegistry interface {
	// watchRegistry calls changed whenever a value under one of the writer's keys changes, until
	// stop is closed.
	watchRegistry(stop <-chan struct{}, changed func())
}

// Watcher notices when a running provider's managed files or registry values change, and has the
// provider compare them with what it applied and apply its settings again when they differ. A file
// is watched through its folder, so a file deleted and created again is seen.
type Watcher struct {
	log      core.Logger
	debounce time.Duration
	backstop time.Duration
	fs       *fsnotify.Watcher // nil when the platform's file notifications could not start

	mu      sync.Mutex
	closed  bool
	targets map[*Provider]*watchTarget
	added   map[string]bool // folders fs watches
	regWG   sync.WaitGroup
}

// watchTarget is one registered provider.
type watchTarget struct {
	p     *Provider
	files []string
	timer *time.Timer
	stop  chan struct{} // ends the provider's registry watches
}

// NewWatcher returns a watcher. When file notifications cannot start, files are compared only by
// the backstop.
func NewWatcher(log core.Logger) *Watcher {
	if log == nil {
		log = nopLogger{}
	}
	w := &Watcher{
		log:      log,
		debounce: driftDebounce,
		backstop: driftBackstop,
		targets:  map[*Provider]*watchTarget{},
		added:    map[string]bool{},
	}
	fs, err := fsnotify.NewWatcher()
	if err != nil {
		log.Printf("toolconfig: file change notifications are unavailable; managed files are compared every %s: %v", driftBackstop, err)
	} else {
		w.fs = fs
	}
	return w
}

// Run handles change notifications and runs the backstop until stop is closed, then ends every
// watch.
func (w *Watcher) Run(stop <-chan struct{}) {
	tick := time.NewTicker(w.backstop)
	defer tick.Stop()
	var events <-chan fsnotify.Event
	var errs <-chan error
	if w.fs != nil {
		events, errs = w.fs.Events, w.fs.Errors
	}
	for {
		select {
		case <-stop:
			w.close()
			return
		case ev, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			w.handle(ev)
		case err, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			w.log.Printf("toolconfig: file change notifications: %v", err)
		case <-tick.C:
			w.mu.Lock()
			ps := make([]*Provider, 0, len(w.targets))
			for p := range w.targets {
				ps = append(ps, p)
			}
			w.mu.Unlock()
			for _, p := range ps {
				p.checkDrift()
			}
			w.watchFolders()
		}
	}
}

// close ends every watch. A provider that registers afterwards is not watched.
func (w *Watcher) close() {
	w.mu.Lock()
	w.closed = true
	for p, t := range w.targets {
		w.release(t)
		delete(w.targets, p)
	}
	w.mu.Unlock()
	w.regWG.Wait()
	if w.fs != nil {
		_ = w.fs.Close()
	}
}

// add watches a running provider's managed files or registry keys, and compares them once.
func (w *Watcher) add(p *Provider) {
	w.mu.Lock()
	if w.closed || w.targets[p] != nil {
		w.mu.Unlock()
		return
	}
	t := &watchTarget{p: p, stop: make(chan struct{})}
	if rw, ok := p.w.(watchedRegistry); ok {
		w.regWG.Add(1)
		go func() {
			defer w.regWG.Done()
			rw.watchRegistry(t.stop, func() { w.schedule(t) })
		}()
	} else {
		paths := []string{p.w.Path()}
		if fw, ok := p.w.(watchedFiles); ok {
			paths = fw.watchedFiles()
		}
		for _, f := range paths {
			if f != "" {
				t.files = append(t.files, filepath.Clean(f))
			}
		}
	}
	w.targets[p] = t
	w.scheduleLocked(t)
	w.mu.Unlock()
	w.watchFolders()
}

// remove stops watching a provider. It does not wait for a comparison already under way, which
// finds the provider stopped.
func (w *Watcher) remove(p *Provider) {
	w.mu.Lock()
	t := w.targets[p]
	if t == nil {
		w.mu.Unlock()
		return
	}
	w.release(t)
	delete(w.targets, p)
	unwanted := map[string]bool{}
	for _, f := range t.files {
		unwanted[filepath.Dir(f)] = true
	}
	for _, other := range w.targets {
		for _, f := range other.files {
			delete(unwanted, filepath.Dir(f))
		}
	}
	for dir := range unwanted {
		if w.added[dir] {
			delete(w.added, dir)
			if w.fs != nil {
				_ = w.fs.Remove(dir)
			}
		}
	}
	w.mu.Unlock()
}

// release ends a target's pending comparison and its registry watches. w.mu is held.
func (w *Watcher) release(t *watchTarget) {
	if t.timer != nil {
		t.timer.Stop()
	}
	close(t.stop)
}

// watchFolders starts watching the folder of every registered file that is not watched yet; a
// folder that does not exist yet is tried again after the next comparison.
func (w *Watcher) watchFolders() {
	if w.fs == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return
	}
	for _, t := range w.targets {
		for _, f := range t.files {
			dir := filepath.Dir(f)
			if w.added[dir] {
				continue
			}
			if err := w.fs.Add(dir); err == nil {
				w.added[dir] = true
			}
		}
	}
}

// handle schedules a comparison for the provider whose file an event names. An event for a
// watched folder itself (it was deleted or renamed) ends that folder's watch and schedules every
// provider with a file in it.
func (w *Watcher) handle(ev fsnotify.Event) {
	if !ev.Has(fsnotify.Write) && !ev.Has(fsnotify.Create) && !ev.Has(fsnotify.Rename) && !ev.Has(fsnotify.Remove) {
		return
	}
	name := filepath.Clean(ev.Name)
	w.mu.Lock()
	defer w.mu.Unlock()
	folder := false
	for dir := range w.added {
		if samePath(dir, name) && (ev.Has(fsnotify.Remove) || ev.Has(fsnotify.Rename)) {
			delete(w.added, dir)
			folder = true
		}
	}
	for _, t := range w.targets {
		for _, f := range t.files {
			if samePath(f, name) || (folder && samePath(filepath.Dir(f), name)) {
				w.scheduleLocked(t)
				break
			}
		}
	}
}

// schedule compares a target's configuration once its changes have been quiet for the debounce.
func (w *Watcher) schedule(t *watchTarget) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.targets[t.p] == t {
		w.scheduleLocked(t)
	}
}

func (w *Watcher) scheduleLocked(t *watchTarget) {
	if t.timer == nil {
		t.timer = time.AfterFunc(w.debounce, func() {
			t.p.checkDrift()
			w.watchFolders()
		})
		return
	}
	t.timer.Reset(w.debounce)
}

// samePath compares two cleaned paths as the platform's file system does.
func samePath(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}
