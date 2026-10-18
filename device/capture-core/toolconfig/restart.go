package toolconfig

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/state"
	"github.com/shadow-ai-capture/device/protocol"
)

// AppProcess is a running process of a catalog app.
type AppProcess struct {
	PID uint32
	// Started is the process's creation time.
	Started time.Time
	// Session is the desktop session the process runs in; 0 is the services' session, where no
	// one is signed in.
	Session uint32
	// App is the catalog app the process runs. Parent is the catalog app of the process that
	// started it, empty when that process is no app's or has exited.
	App, Parent string
}

// restartKey is one app a person signed in to a session is told to restart, by the name they know
// it by.
type restartKey struct {
	session uint32
	app     string
}

// restartNotified is what was done about a restartKey for the configuration that changed at
// changed: shown, or tried and not shown.
type restartNotified struct {
	changed time.Time
	shown   bool
}

// restartName is the name a person restarts p by: the app that started it when that is one of the
// tool's restart apps too, as the Claude desktop app is for the Claude Code sessions it runs, else
// its own.
func (t tool) restartName(p AppProcess) string {
	if name, ok := t.restartApps[p.Parent]; ok {
		return name
	}
	return t.restartApps[p.App]
}

// restartNotification tells a person to restart app.
func restartNotification(app string) protocol.Notify {
	return protocol.Notify{
		Title: "Restart " + app,
		Body:  "Your organization has updated settings for " + app + ". Restart " + app + " to apply them.",
	}
}

// configState is when the tool's managed files last changed, zero when none can be read, and a
// digest of what they hold.
func (p *Provider) configState() (time.Time, string) {
	files := []string{p.w.Path()}
	if fw, ok := p.w.(watchedFiles); ok {
		files = fw.watchedFiles()
	}
	var last time.Time
	h := sha256.New()
	for _, f := range files {
		if fi, err := os.Stat(f); err == nil && fi.ModTime().After(last) {
			last = fi.ModTime()
		}
		b, err := os.ReadFile(f)
		fmt.Fprintf(h, "%d:%s:%t:", len(f), f, err == nil)
		if err == nil {
			fmt.Fprintf(h, "%d:", len(b))
			h.Write(b)
		}
	}
	return last, hex.EncodeToString(h.Sum(nil))
}

// runningWith is the processes of the tool's restart apps known to run with the configuration whose
// digest is Digest: each was seen running after the configuration was last written with it. The
// agent removes the configuration when it stops and writes it again when it starts, so a process
// that ran with it before an update of the agent still runs with it when the update writes the
// same configuration again.
type runningWith struct {
	Digest    string      `json:"digest"`
	Processes []processID `json:"processes"`
}

// processID names a process across reuses of its PID.
type processID struct {
	PID     uint32    `json:"pid"`
	Started time.Time `json:"started"`
}

func (r runningWith) has(pr AppProcess) bool {
	return slices.ContainsFunc(r.Processes, func(id processID) bool { return id.PID == pr.PID && id.Started.Equal(pr.Started) })
}

func (r runningWith) equal(o runningWith) bool {
	return r.Digest == o.Digest && slices.EqualFunc(r.Processes, o.Processes, func(a, b processID) bool { return a.PID == b.PID && a.Started.Equal(b.Started) })
}

// loadRunning reads the processes kept in path; none when it is empty, missing or unreadable.
func loadRunning(path string) runningWith {
	var r runningWith
	b, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(b, &r) != nil {
		return runningWith{}
	}
	return r
}

// keepRunning records r as the processes running with the configuration, in memory and, when it
// changed, in the provider's running file. A failed write is logged once until one succeeds.
func (p *Provider) keepRunning(r runningWith) {
	p.mu.Lock()
	same := p.current.equal(r)
	p.current = r
	p.mu.Unlock()
	if same || p.cfg.RunningFile == "" {
		return
	}
	err := writeRunning(p.cfg.RunningFile, r)
	p.mu.Lock()
	failing := p.runningFailing
	p.runningFailing = err != nil
	p.mu.Unlock()
	if err != nil && !failing {
		p.cfg.Log.Printf("toolconfig: %s: recording the processes running with its configuration: %v", p.tool.key, err)
	}
}

func writeRunning(path string, r runningWith) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if err := state.MkdirProtected(filepath.Dir(path)); err != nil {
		return err
	}
	return state.WriteFile(path, b)
}

// checkRestart looks, while the tool's configuration is in place and the bundle's restart notice is
// on, for processes of the tool's restart apps that started before the configuration was last
// written and are not known to run with what it holds now: each runs without it until it restarts.
// While the notice includes the dashboard the health report carries them; while it includes notify,
// the person in each session running one is told once per change which app to restart.
func (p *Provider) checkRestart() {
	if len(p.tool.restartApps) == 0 || p.cfg.Processes == nil {
		return
	}
	p.mu.Lock()
	notice, inPlace := p.notice, p.running && !p.inPlaceSince.IsZero()
	p.mu.Unlock()
	var stale []AppProcess
	var changed time.Time
	var digest string
	if inPlace && (notice.Dashboard() || notice.Notify()) {
		changed, digest = p.configState()
	}
	if !changed.IsZero() {
		procs, err := p.cfg.Processes(slices.Sorted(maps.Keys(p.tool.restartApps)))
		p.mu.Lock()
		failing := p.processesFailing
		p.processesFailing = err != nil
		if err != nil {
			p.restartDue = false
		}
		known := p.current
		p.mu.Unlock()
		if err != nil {
			if !failing {
				p.counters.Add(protocol.CounterErrors)
				p.cfg.Log.Printf("toolconfig: %s: listing the running processes: %v", p.tool.key, err)
			}
			return
		}
		if known.Digest != digest {
			known = runningWith{}
		}
		current := runningWith{Digest: digest}
		for _, pr := range procs {
			if p.tool.restartApps[pr.App] == "" {
				continue
			}
			if !pr.Started.Before(changed) || known.has(pr) {
				current.Processes = append(current.Processes, processID{PID: pr.PID, Started: pr.Started})
				continue
			}
			stale = append(stale, pr)
		}
		slices.SortFunc(current.Processes, func(a, b processID) int { return cmp.Or(cmp.Compare(a.PID, b.PID), a.Started.Compare(b.Started)) })
		p.keepRunning(current)
	}
	p.mu.Lock()
	p.restartDue = len(stale) > 0
	p.mu.Unlock()
	if notice.Notify() && p.cfg.Notify != nil {
		p.notifyRestart(stale, changed)
	}
}

// notifyRestart tells the person in each session running a stale process to restart its app, once
// per configuration change. A notification that is not shown is tried again at the next check, and
// only its first failure is logged.
func (p *Provider) notifyRestart(stale []AppProcess, changed time.Time) {
	due := map[restartKey]bool{}
	for _, pr := range stale {
		if pr.Session != 0 {
			due[restartKey{pr.Session, p.tool.restartName(pr)}] = true
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for k := range p.notified {
		if !due[k] {
			delete(p.notified, k)
		}
	}
	for k := range due {
		if p.notifying[k] {
			continue
		}
		prev, tried := p.notified[k]
		if tried && prev.shown && !prev.changed.Before(changed) {
			continue
		}
		logFailure := !tried || prev.changed.Before(changed)
		p.notifying[k] = true
		go func() {
			err := p.cfg.Notify(k.session, restartNotification(k.app))
			p.mu.Lock()
			delete(p.notifying, k)
			if p.running {
				p.notified[k] = restartNotified{changed: changed, shown: err == nil}
			}
			p.mu.Unlock()
			if err != nil && logFailure {
				p.cfg.Log.Printf("toolconfig: %s: could not tell session %d to restart %s: %v", p.tool.key, k.session, k.app, err)
			}
		}()
	}
}
