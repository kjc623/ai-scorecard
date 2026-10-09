package inventory

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/shadow-ai-capture/device/capture-core/core"
	"github.com/shadow-ai-capture/device/capture-core/discovery"
	"github.com/shadow-ai-capture/device/capture-core/hostinfo"
	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/protocol"
)

// categoryLocalRuntime is the catalog category of the apps the model scanner looks for.
const categoryLocalRuntime = "local_runtime"

// The runtimes whose model store layout the scanner reads, by catalog app key. Another
// local_runtime app is still found by its process, listener or store, with no model names.
const (
	appOllama   = "ollama"
	appLMStudio = "lm_studio"
)

// The envelope's model_names limits.
const (
	maxModelNames     = 64
	maxModelNameChars = 200
)

// ollamaModelsVar is the environment variable that moves Ollama's model store.
const ollamaModelsVar = "OLLAMA_MODELS"

// RunningProcess is one entry of the machine's process list.
type RunningProcess struct {
	PID uint32
	// Name is the image's base name, such as ollama.exe.
	Name string
}

// ModelHost is the read-only view of the machine the model scanner needs besides the model stores,
// which it reads directly.
type ModelHost interface {
	// Profiles lists the user profiles, as for the CLI scanner.
	Profiles() ([]Profile, error)
	// UserEnv is a variable of the user's own environment (the Environment key of their hive),
	// unexpanded; "" when it is unset or the hive is not loaded.
	UserEnv(sid, name string) (string, error)
	// Getenv returns a machine environment variable, for expanding a user value that names one.
	Getenv(name string) string
	// Processes lists the running processes.
	Processes() ([]RunningProcess, error)
	// ProcessInfo describes a running process (hostinfo.ProcessInfo).
	ProcessInfo(pid uint32) (hostinfo.Process, error)
	// ListenersOn lists the PIDs listening for TCP on a local port (hostinfo.ListenersOn).
	ListenersOn(port int) ([]uint32, error)
	// FileVersion is an executable's PE file version, read as data; "" when it has none.
	FileVersion(path string) (string, error)
}

// ModelScanner finds the catalog's local model runtimes per user and lists the models in their
// stores. A runtime is present for a user when one of its executables runs as that user, or its
// model store exists in the user's profile. The scan reads files and process metadata only; it
// never asks a runtime anything.
type ModelScanner struct {
	host   ModelHost
	person func(hostinfo.User) core.Person
}

// NewModelScanner returns the local model scanner over host; person names a profile owner's person.
func NewModelScanner(host ModelHost, person func(hostinfo.User) core.Person) *ModelScanner {
	return &ModelScanner{host: host, person: person}
}

// Scan implements Scanner.
func (s *ModelScanner) Scan(ctx context.Context, b *policy.Bundle) ([]discovery.Record, []error) {
	return s.ScanCounted(ctx, b, nil)
}

// ScanCounted implements CountedScanner: a record whose model list had to be cut to the envelope's
// limits counts one dropped on counters.
func (s *ModelScanner) ScanCounted(ctx context.Context, b *policy.Bundle, counters *core.CounterSet) ([]discovery.Record, []error) {
	runtimes := b.AppsInCategory(categoryLocalRuntime)
	if len(runtimes) == 0 {
		return nil, nil
	}
	ms := &modelScan{
		s: s, b: b, counters: counters, runtimes: runtimes,
		running:   map[string]map[string][]runningImage{},
		listening: map[string]map[uint32]bool{},
		versions:  map[string]string{},
	}
	ms.processes()
	ms.listeners()
	profiles, err := s.host.Profiles()
	if err != nil {
		ms.fail("listing the user profiles", err)
	}
	for _, p := range profiles {
		if ctx.Err() != nil {
			ms.errs = append(ms.errs, ctx.Err())
			break
		}
		for _, app := range runtimes {
			ms.profile(p, app)
		}
	}
	return ms.recs, ms.errs
}

// runningImage is a runtime's process and its executable.
type runningImage struct {
	pid   uint32
	image string
}

// modelScan is one pass of the model scanner.
type modelScan struct {
	s        *ModelScanner
	b        *policy.Bundle
	counters *core.CounterSet
	runtimes []string
	// running is each runtime's processes by the SID they run as, upper case.
	running map[string]map[string][]runningImage
	// listening is each runtime's processes that listen on one of its ports.
	listening map[string]map[uint32]bool
	versions  map[string]string // FileVersion per path, read once per pass
	recs      []discovery.Record
	errs      []error
}

func (ms *modelScan) fail(what string, err error) {
	ms.errs = append(ms.errs, fmt.Errorf("%s: %w", what, err))
}

// processes finds the runtimes' processes and the person each runs as. A process that cannot be
// described has usually ended since the listing, and one that runs as no person (a service
// account) belongs to no profile; neither is counted.
func (ms *modelScan) processes() {
	procs, err := ms.s.host.Processes()
	if err != nil {
		ms.fail("listing the running processes", err)
	}
	for _, p := range procs {
		var apps []string
		for _, app := range ms.b.AppByExe(platformWindows, p.Name) {
			if slices.Contains(ms.runtimes, app) {
				apps = append(apps, app)
			}
		}
		if len(apps) == 0 {
			continue
		}
		info, err := ms.s.host.ProcessInfo(p.PID)
		if err != nil || info.User == nil || !userSID(info.User.SID) {
			continue
		}
		sid := strings.ToUpper(info.User.SID)
		for _, app := range apps {
			if ms.running[app] == nil {
				ms.running[app] = map[string][]runningImage{}
			}
			ms.running[app][sid] = append(ms.running[app][sid], runningImage{pid: p.PID, image: info.Image})
		}
	}
}

// listeners finds which of the runtimes' processes listen on one of the runtime's ports. Only a
// runtime with a running process is looked up: a listener counts only when such a process owns it.
func (ms *modelScan) listeners() {
	for _, app := range ms.runtimes {
		if len(ms.running[app]) == 0 {
			continue
		}
		for _, v := range ms.b.SignalValues(app, platformWindows, policy.SignalListenPort) {
			port, err := strconv.Atoi(v)
			if err != nil || port < 1 || port > 65535 {
				continue
			}
			pids, err := ms.s.host.ListenersOn(port)
			if err != nil {
				ms.fail(fmt.Sprintf("listing the listeners on port %d", port), err)
				continue
			}
			for _, pid := range pids {
				if ms.listening[app] == nil {
					ms.listening[app] = map[uint32]bool{}
				}
				ms.listening[app][pid] = true
			}
		}
	}
}

// profile emits the runtime's record for the profile's owner when the runtime is present for them.
func (ms *modelScan) profile(p Profile, app string) {
	procs := ms.running[app][strings.ToUpper(p.User.SID)]
	var names []string
	stored := false
	for _, dir := range ms.stores(p, app) {
		fi, err := os.Stat(dir)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			ms.fail("checking the model store "+dir, err)
			continue
		}
		if !fi.IsDir() {
			continue
		}
		stored = true
		found, err := storedModels(app, dir)
		if err != nil {
			ms.fail("reading the model store "+dir, err)
		}
		names = append(names, found...)
	}
	if len(procs) == 0 && !stored {
		return
	}

	// The listening process's executable names the version first, then the others' in path order.
	basis := protocol.DetectionBasisModelStore
	var listeners, others []string
	for _, pr := range procs {
		if ms.listening[app][pr.pid] {
			basis = protocol.DetectionBasisPortListen
			listeners = append(listeners, pr.image)
		} else {
			others = append(others, pr.image)
		}
	}
	slices.Sort(listeners)
	slices.Sort(others)
	ver := ""
	for _, image := range slices.Concat(listeners, others) {
		if ver = ms.fileVersion(image); ver != "" {
			break
		}
	}

	names, cut := capModelNames(names)
	if cut && ms.counters != nil {
		ms.counters.Add(protocol.CounterDropped)
	}
	person := ms.s.person(p.User)
	ms.recs = append(ms.recs, discovery.Record{
		Type:       protocol.DiscoveryTypeLocalModel,
		Basis:      basis,
		AppKey:     app,
		Version:    version(ver),
		ModelNames: names,
		Person:     &person,
	})
}

// stores is the runtime's model store folders in the profile: the catalog's model_store values,
// with a leading ~ and the profile's %VAR%s expanded. Ollama's store is the user's OLLAMA_MODELS
// instead when it is set, read as Ollama reads it (trimmed of spaces and quotes).
func (ms *modelScan) stores(p Profile, app string) []string {
	env := ms.profileEnv(p.Dir)
	if app == appOllama && p.Loaded {
		v, err := ms.s.host.UserEnv(p.User.SID, ollamaModelsVar)
		if err != nil {
			ms.fail("reading "+ollamaModelsVar+" of user "+p.User.SID, err)
		}
		if v = strings.Trim(strings.TrimSpace(v), `"'`); v != "" {
			if dir := storePath(expandPercent(v, env)); dir != "" {
				return []string{dir}
			}
			return nil
		}
	}
	var out []string
	for _, v := range ms.b.SignalValues(app, platformWindows, policy.SignalModelStore) {
		if rest, ok := strings.CutPrefix(v, "~"); ok && (rest == "" || rest[0] == '/' || rest[0] == '\\') {
			v = p.Dir + rest
		}
		if dir := storePath(expandPercent(v, env)); dir != "" && !slices.Contains(out, dir) {
			out = append(out, dir)
		}
	}
	return out
}

// storePath is an expanded store value as a clean absolute path, either slash a separator, or ""
// when it is relative or still names an unset variable.
func storePath(v string) string {
	sep := string(filepath.Separator)
	v = strings.NewReplacer(`\`, sep, "/", sep).Replace(strings.TrimSpace(v))
	if v == "" || strings.Contains(v, "%") || !filepath.IsAbs(v) {
		return ""
	}
	return filepath.Clean(v)
}

// profileEnv resolves the variables a user's own values name: the profile's folders first, then the
// machine's.
func (ms *modelScan) profileEnv(home string) func(string) (string, bool) {
	own := map[string]string{
		"USERPROFILE":  home,
		"APPDATA":      filepath.Join(home, "AppData", "Roaming"),
		"LOCALAPPDATA": filepath.Join(home, "AppData", "Local"),
	}
	return func(name string) (string, bool) {
		if v, ok := own[strings.ToUpper(name)]; ok {
			return v, true
		}
		v := ms.s.host.Getenv(name)
		return v, v != ""
	}
}

func (ms *modelScan) fileVersion(file string) string {
	if v, ok := ms.versions[file]; ok {
		return v
	}
	v, err := ms.s.host.FileVersion(file)
	if err != nil {
		ms.fail("reading the file version of "+file, err)
	}
	ms.versions[file] = v
	return v
}

// storedModels lists the models in a runtime's store, for the runtimes whose layout is known.
func storedModels(app, dir string) ([]string, error) {
	switch app {
	case appOllama:
		return ollamaModels(dir)
	case appLMStudio:
		return lmStudioModels(dir)
	}
	return nil, nil
}

// ollamaModels lists the models an Ollama store holds, named as `ollama list` names them. Each
// model is a manifest file <root>\<host>\<namespace>\<model>\<tag>, under manifests-v2 (written by
// current releases) or manifests (older ones). Only the public registry's models are read, whose
// host is registry.ollama.ai or ollama.com; the library namespace is shown without its prefix.
func ollamaModels(dir string) ([]string, error) {
	var names []string
	var errs []error
	for _, root := range []string{filepath.Join(dir, "manifests-v2"), filepath.Join(dir, "manifests")} {
		hosts, err := readDir(root)
		if err != nil {
			errs = append(errs, err)
		}
		for _, host := range hosts {
			if !host.IsDir() || !strings.EqualFold(host.Name(), "registry.ollama.ai") && !strings.EqualFold(host.Name(), "ollama.com") {
				continue
			}
			found, err := ollamaManifests(filepath.Join(root, host.Name()))
			if err != nil {
				errs = append(errs, err)
			}
			names = append(names, found...)
		}
	}
	return names, errors.Join(errs...)
}

// ollamaManifests walks one registry host's <namespace>\<model>\<tag> manifests. A folder with no
// tag file in it, and a name part Ollama would refuse, name no model.
func ollamaManifests(hostDir string) ([]string, error) {
	var names []string
	var errs []error
	namespaces, err := readDir(hostDir)
	if err != nil {
		errs = append(errs, err)
	}
	for _, ns := range namespaces {
		if !ns.IsDir() || !ollamaPart(ns.Name(), false) {
			continue
		}
		models, err := readDir(filepath.Join(hostDir, ns.Name()))
		if err != nil {
			errs = append(errs, err)
		}
		for _, m := range models {
			if !m.IsDir() || !ollamaPart(m.Name(), true) {
				continue
			}
			tags, err := readDir(filepath.Join(hostDir, ns.Name(), m.Name()))
			if err != nil {
				errs = append(errs, err)
			}
			for _, tag := range tags {
				if tag.IsDir() || !ollamaPart(tag.Name(), true) {
					continue
				}
				name := m.Name() + ":" + tag.Name()
				if !strings.EqualFold(ns.Name(), "library") {
					name = ns.Name() + "/" + name
				}
				names = append(names, name)
			}
		}
	}
	return names, errors.Join(errs...)
}

// ollamaPart reports whether s is a valid namespace, model or tag part of an Ollama model name: 1
// to 80 characters, a letter, digit or underscore first, then those and '-' and '_', and '.' except
// in a namespace. It also leaves out Ollama's temporary .manifest-* files.
func ollamaPart(s string, dotted bool) bool {
	if len(s) < 1 || len(s) > 80 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_':
		case i > 0 && c == '-':
		case i > 0 && c == '.' && dotted:
		default:
			return false
		}
	}
	return true
}

// lmStudioModels lists the models an LM Studio store holds: each <publisher>\<model> folder with a
// GGUF file (*.gguf) or MLX weights (model*.safetensors, as mlx-lm saves them) in it, named
// <publisher>/<model>.
func lmStudioModels(dir string) ([]string, error) {
	var names []string
	var errs []error
	publishers, err := readDir(dir)
	if err != nil {
		errs = append(errs, err)
	}
	for _, pub := range publishers {
		if !pub.IsDir() || strings.HasPrefix(pub.Name(), ".") {
			continue
		}
		models, err := readDir(filepath.Join(dir, pub.Name()))
		if err != nil {
			errs = append(errs, err)
		}
		for _, m := range models {
			if !m.IsDir() || strings.HasPrefix(m.Name(), ".") {
				continue
			}
			files, err := readDir(filepath.Join(dir, pub.Name(), m.Name()))
			if err != nil {
				errs = append(errs, err)
			}
			if slices.ContainsFunc(files, weightFile) {
				names = append(names, pub.Name()+"/"+m.Name())
			}
		}
	}
	return names, errors.Join(errs...)
}

func weightFile(e os.DirEntry) bool {
	if e.IsDir() {
		return false
	}
	n := strings.ToLower(e.Name())
	return strings.HasSuffix(n, ".gguf") || strings.HasPrefix(n, "model") && strings.HasSuffix(n, ".safetensors")
}

// capModelNames sorts the names and keeps those the envelope can carry: at most maxModelNames,
// each of 1 to maxModelNameChars characters. cut reports that one was left out.
func capModelNames(names []string) (kept []string, cut bool) {
	slices.Sort(names)
	names = slices.Compact(names)
	for _, n := range names {
		if n == "" || utf8.RuneCountInString(n) > maxModelNameChars {
			cut = true
			continue
		}
		if len(kept) == maxModelNames {
			return kept, true
		}
		kept = append(kept, n)
	}
	return kept, cut
}
