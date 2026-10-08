package inventory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/discovery"
	"github.com/shadow-ai-capture/device/capture-core/hostinfo"
	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/capture-core/state"
	"github.com/shadow-ai-capture/device/protocol"
)

const (
	linusSID  = "S-1-5-21-1004336348-1177238915-682003330-1002"
	systemSID = "S-1-5-18"
)

// modelCatalog holds the catalog's signals for the two local runtimes, and an IDE that is not one.
func modelCatalog() *policy.Bundle {
	sig := func(platform, kind, value string) policy.CatalogSignal {
		return policy.CatalogSignal{Platform: platform, Kind: kind, Value: value}
	}
	return &policy.Bundle{
		Endpoint: policy.EndpointPolicy{
			Inventory:            policy.EndpointInventory{Enabled: true, IntervalMinutes: 360},
			DiscoveryDailyBudget: 200,
		},
		Catalog: []policy.CatalogApp{
			{AppKey: "lm_studio", Category: "local_runtime", Signals: []policy.CatalogSignal{
				sig("any", policy.SignalListenPort, "1234"), sig("any", policy.SignalModelStore, "~/.lmstudio/models"),
			}},
			{AppKey: "ollama", Category: "local_runtime", Signals: []policy.CatalogSignal{
				sig("any", policy.SignalListenPort, "11434"),
				sig("linux", policy.SignalModelStore, "/usr/share/ollama/.ollama/models"),
				sig("macos", policy.SignalModelStore, "~/.ollama/models"),
				sig("windows", policy.SignalModelStore, `%USERPROFILE%\.ollama\models`),
				sig("windows", policy.SignalWindowsExe, "ollama app.exe"),
				sig("windows", policy.SignalWindowsExe, "ollama.exe"),
			}},
			{AppKey: "vscode", Category: "ide", Signals: []policy.CatalogSignal{
				sig("windows", policy.SignalWindowsExe, "Code.exe"),
			}},
		},
	}
}

// fakeModelHost is the process list, process owners, listeners and registry values of a fixture
// device whose profiles are folders on disk.
type fakeModelHost struct {
	profiles    []Profile
	profilesErr error
	userEnv     map[string]map[string]string // SID -> name -> value
	env         map[string]string
	procs       []RunningProcess
	procsErr    error
	info        map[uint32]hostinfo.Process
	listeners   map[int][]uint32
	listenErr   map[int]error
	versions    map[string]string

	infoCalls   []uint32
	listenCalls []int
}

func (h *fakeModelHost) Profiles() ([]Profile, error) { return h.profiles, h.profilesErr }

func (h *fakeModelHost) UserEnv(sid, name string) (string, error) { return h.userEnv[sid][name], nil }

func (h *fakeModelHost) Getenv(name string) string { return h.env[strings.ToUpper(name)] }

func (h *fakeModelHost) Processes() ([]RunningProcess, error) { return h.procs, h.procsErr }

func (h *fakeModelHost) ProcessInfo(pid uint32) (hostinfo.Process, error) {
	h.infoCalls = append(h.infoCalls, pid)
	p, ok := h.info[pid]
	if !ok {
		return hostinfo.Process{}, fmt.Errorf("opening process %d: gone", pid)
	}
	return p, nil
}

func (h *fakeModelHost) ListenersOn(port int) ([]uint32, error) {
	h.listenCalls = append(h.listenCalls, port)
	return h.listeners[port], h.listenErr[port]
}

func (h *fakeModelHost) FileVersion(path string) (string, error) { return h.versions[path], nil }

func runningAs(pid uint32, image, sid string) hostinfo.Process {
	return hostinfo.Process{PID: pid, Image: image, User: &hostinfo.User{SID: sid}}
}

// modelTree is the fixture device under root:
//   - Ada (hive loaded) runs Ollama: the tray app and the server, which listens on 11434. Her store
//     holds models in both manifest trees (one in each, one in both), a namespaced model, a
//     tag-less model folder, a model from another registry and a temporary manifest. Her LM Studio
//     store holds a GGUF model, an MLX model and a folder without weights; LM Studio is not running.
//   - Grace (hive loaded) moved Ollama's store with OLLAMA_MODELS to an empty folder; her default
//     store, which Ollama no longer reads, still holds a model.
//   - Linus (hive not loaded) has no store but runs ollama.exe, which is not listening.
//   - The system account runs another ollama.exe that listens; VS Code runs; one more ollama.exe
//     ended before it could be described.
func modelTree(t *testing.T) *fakeModelHost {
	t.Helper()
	root := t.TempDir()
	users := filepath.Join(root, "Users")
	ada, grace, linus := filepath.Join(users, "ada"), filepath.Join(users, "grace"), filepath.Join(users, "linus")
	mkdir(t, linus)

	store := filepath.Join(ada, ".ollama", "models")
	put(t, filepath.Join(store, "manifests", "registry.ollama.ai", "library", "llama3.2", "1b"), "{}")
	put(t, filepath.Join(store, "manifests-v2", "ollama.com", "library", "llama3.2", "1b"), "{}")
	put(t, filepath.Join(store, "manifests-v2", "ollama.com", "library", "qwen2.5", "0.5b"), "{}")
	put(t, filepath.Join(store, "manifests-v2", "ollama.com", "library", "qwen2.5", ".manifest-123456"), "")
	put(t, filepath.Join(store, "manifests", "registry.ollama.ai", "jmorganca", "custom", "latest"), "{}")
	mkdir(t, filepath.Join(store, "manifests", "registry.ollama.ai", "library", "untagged"))
	put(t, filepath.Join(store, "manifests-v2", "hf.co", "bartowski", "some-gguf", "Q4_K_M"), "{}")
	put(t, filepath.Join(store, "blobs", "sha256-0123"), "weights")

	lms := filepath.Join(ada, ".lmstudio", "models")
	put(t, filepath.Join(lms, "lmstudio-community", "Qwen2.5-7B-Instruct-GGUF", "Qwen2.5-7B-Instruct-Q4_K_M.gguf"), "gguf")
	put(t, filepath.Join(lms, "mlx-community", "Llama-3.2-1B-Instruct-4bit", "model.safetensors"), "mlx")
	put(t, filepath.Join(lms, "mlx-community", "Llama-3.2-1B-Instruct-4bit", "config.json"), "{}")
	put(t, filepath.Join(lms, "someone", "notes-only", "README.md"), "no weights")
	put(t, filepath.Join(lms, "someone", "stray.gguf"), "not in a model folder")

	mkdir(t, filepath.Join(grace, "models", "ollama"))
	put(t, filepath.Join(grace, ".ollama", "models", "manifests", "registry.ollama.ai", "library", "phi3", "latest"), "{}")

	programs := filepath.Join(ada, "AppData", "Local", "Programs", "Ollama")
	trayExe, serverExe := filepath.Join(programs, "ollama app.exe"), filepath.Join(programs, "ollama.exe")
	linusExe := filepath.Join(linus, "AppData", "Local", "Programs", "Ollama", "ollama.exe")
	return &fakeModelHost{
		profiles: []Profile{
			{User: hostinfo.User{SID: adaSID, Account: `AzureAD\ada`}, Dir: ada, Loaded: true},
			{User: hostinfo.User{SID: graceSID, Account: `CONTOSO\grace`}, Dir: grace, Loaded: true},
			{User: hostinfo.User{SID: linusSID, Account: `CONTOSO\linus`}, Dir: linus},
		},
		userEnv: map[string]map[string]string{graceSID: {"OLLAMA_MODELS": `"%USERPROFILE%\models\ollama"`}},
		procs: []RunningProcess{
			{PID: 4, Name: "System"}, {PID: 10, Name: "ollama app.exe"}, {PID: 11, Name: "ollama.exe"},
			{PID: 30, Name: "ollama.exe"}, {PID: 40, Name: "ollama.exe"}, {PID: 50, Name: "Code.exe"},
			{PID: 60, Name: "ollama.exe"},
		},
		info: map[uint32]hostinfo.Process{
			10: runningAs(10, trayExe, adaSID),
			11: runningAs(11, serverExe, strings.ToLower(adaSID)),
			30: runningAs(30, linusExe, linusSID),
			40: runningAs(40, `C:\Program Files\Ollama\ollama.exe`, systemSID),
			50: runningAs(50, `C:\Program Files\Microsoft VS Code\Code.exe`, adaSID),
		},
		listeners: map[int][]uint32{11434: {11, 40}},
		versions:  map[string]string{serverExe: "0.40.2.0", trayExe: "0.40.1.0", linusExe: "0.39.0.0"},
	}
}

type modelFound struct {
	app, basis, version, userRef string
	names                        []string
}

func summariseModels(t *testing.T, recs []discovery.Record) []modelFound {
	t.Helper()
	var out []modelFound
	for _, r := range recs {
		if r.Type != protocol.DiscoveryTypeLocalModel || r.Person == nil || r.UserRef != "" {
			t.Fatalf("record %+v is not a local_model of a person", r)
		}
		out = append(out, modelFound{r.AppKey, string(r.Basis), r.Version, r.Person.UserRef, r.ModelNames})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].userRef != out[j].userRef {
			return out[i].userRef < out[j].userRef
		}
		return out[i].app < out[j].app
	})
	return out
}

func TestModelScannerFindsRuntimesAndTheirModels(t *testing.T) {
	host := modelTree(t)
	recs, errs := NewModelScanner(host, person).Scan(context.Background(), modelCatalog())
	if len(errs) != 0 {
		t.Fatalf("errors: %v", errs)
	}
	got := summariseModels(t, recs)
	want := []modelFound{
		{"ollama", "model_store", "", "u_1001", nil},
		{"ollama", "model_store", "0.39.0.0", "u_1002", nil},
		{"lm_studio", "model_store", "", "u_4444", []string{"lmstudio-community/Qwen2.5-7B-Instruct-GGUF", "mlx-community/Llama-3.2-1B-Instruct-4bit"}},
		{"ollama", "port_listen", "0.40.2.0", "u_4444", []string{"jmorganca/custom:latest", "llama3.2:1b", "qwen2.5:0.5b"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("found\n%+v\nwant\n%+v", got, want)
	}
	slices.Sort(host.infoCalls)
	if !reflect.DeepEqual(host.infoCalls, []uint32{10, 11, 30, 40, 60}) {
		t.Errorf("described processes %v, want only the runtimes' executables", host.infoCalls)
	}
	if !reflect.DeepEqual(host.listenCalls, []int{11434}) {
		t.Errorf("listener lookups %v, want only Ollama's port: LM Studio has no running process", host.listenCalls)
	}
}

// The presence rules, one runtime and one user at a time: a process of the runtime's executable
// running as the user, or the store in their profile, makes it present; the basis is port_listen
// only when such a process owns a listener on the runtime's port.
func TestModelScannerPresenceRules(t *testing.T) {
	const exe = `C:\Users\ada\AppData\Local\Programs\Ollama\ollama.exe`
	for _, tc := range []struct {
		name      string
		store     bool
		procs     []RunningProcess
		info      map[uint32]hostinfo.Process
		listeners map[int][]uint32
		want      []modelFound
	}{
		{name: "nothing", want: nil},
		{name: "store only", store: true, want: []modelFound{{"ollama", "model_store", "", "u_4444", []string{"llama3.2:1b"}}}},
		{
			name:  "process, not listening",
			procs: []RunningProcess{{PID: 7, Name: "ollama.exe"}},
			info:  map[uint32]hostinfo.Process{7: runningAs(7, exe, adaSID)},
			want:  []modelFound{{"ollama", "model_store", "0.40.2.0", "u_4444", nil}},
		},
		{
			name:      "process listening",
			procs:     []RunningProcess{{PID: 7, Name: "OLLAMA.EXE"}},
			info:      map[uint32]hostinfo.Process{7: runningAs(7, exe, adaSID)},
			listeners: map[int][]uint32{11434: {7}},
			want:      []modelFound{{"ollama", "port_listen", "0.40.2.0", "u_4444", nil}},
		},
		{
			name:      "process listening, with a store",
			store:     true,
			procs:     []RunningProcess{{PID: 7, Name: "ollama.exe"}},
			info:      map[uint32]hostinfo.Process{7: runningAs(7, exe, adaSID)},
			listeners: map[int][]uint32{11434: {7}},
			want:      []modelFound{{"ollama", "port_listen", "0.40.2.0", "u_4444", []string{"llama3.2:1b"}}},
		},
		{
			name:      "the port's listener is another program",
			store:     true,
			procs:     []RunningProcess{{PID: 7, Name: "ollama.exe"}, {PID: 8, Name: "docker.exe"}},
			info:      map[uint32]hostinfo.Process{7: runningAs(7, exe, adaSID), 8: runningAs(8, `C:\docker.exe`, adaSID)},
			listeners: map[int][]uint32{11434: {8}},
			want:      []modelFound{{"ollama", "model_store", "0.40.2.0", "u_4444", []string{"llama3.2:1b"}}},
		},
		{
			name:      "a listener with no runtime process",
			listeners: map[int][]uint32{11434: {8}},
			want:      nil,
		},
		{
			name:      "the process runs as someone else",
			procs:     []RunningProcess{{PID: 7, Name: "ollama.exe"}},
			info:      map[uint32]hostinfo.Process{7: runningAs(7, exe, graceSID)},
			listeners: map[int][]uint32{11434: {7}},
			want:      nil,
		},
		{
			name:      "the process runs as no one readable",
			procs:     []RunningProcess{{PID: 7, Name: "ollama.exe"}},
			info:      map[uint32]hostinfo.Process{7: {PID: 7, Image: exe}},
			listeners: map[int][]uint32{11434: {7}},
			want:      nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			if tc.store {
				put(t, filepath.Join(home, ".ollama", "models", "manifests-v2", "ollama.com", "library", "llama3.2", "1b"), "{}")
			}
			host := &fakeModelHost{
				profiles:  []Profile{{User: hostinfo.User{SID: adaSID}, Dir: home, Loaded: true}},
				procs:     tc.procs,
				info:      tc.info,
				listeners: tc.listeners,
				versions:  map[string]string{exe: "0.40.2.0"},
			}
			b := modelCatalog()
			b.Catalog = b.Catalog[1:] // Ollama alone
			recs, errs := NewModelScanner(host, person).Scan(context.Background(), b)
			if len(errs) != 0 {
				t.Fatalf("errors: %v", errs)
			}
			if got := summariseModels(t, recs); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("found %+v, want %+v", got, tc.want)
			}
			if len(tc.procs) == 0 && len(host.listenCalls) != 0 {
				t.Errorf("looked up listeners %v with no runtime process", host.listenCalls)
			}
		})
	}
}

func TestOllamaModels(t *testing.T) {
	store := t.TempDir()
	v2, legacy := filepath.Join(store, "manifests-v2"), filepath.Join(store, "manifests")
	put(t, filepath.Join(v2, "ollama.com", "library", "llama3.2", "1b"), "{}")
	put(t, filepath.Join(v2, "ollama.com", "library", "llama3.2", "latest"), "{}")
	put(t, filepath.Join(legacy, "registry.ollama.ai", "library", "qwen2.5", "0.5b"), "{}")
	put(t, filepath.Join(legacy, "registry.ollama.ai", "huihui_ai", "deepseek-r1-abliterated", "8b"), "{}")
	mkdir(t, filepath.Join(legacy, "registry.ollama.ai", "library", "tagless"))
	mkdir(t, filepath.Join(legacy, "registry.ollama.ai", "library", "nested", "notatag"))
	put(t, filepath.Join(legacy, "registry.ollama.ai", "library", "loose-file"), "{}")
	put(t, filepath.Join(legacy, "registry.ollama.ai", "bad.namespace", "model", "latest"), "{}")
	put(t, filepath.Join(legacy, "registry.ollama.ai", "library", "-model", "latest"), "{}")
	put(t, filepath.Join(legacy, "localhost%3A5000", "library", "private", "latest"), "{}")
	got, err := ollamaModels(store)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	want := []string{"huihui_ai/deepseek-r1-abliterated:8b", "llama3.2:1b", "llama3.2:latest", "qwen2.5:0.5b"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("models %v, want %v", got, want)
	}

	for name, dir := range map[string]string{"empty store": t.TempDir(), "missing store": filepath.Join(store, "absent")} {
		if got, err := ollamaModels(dir); len(got) != 0 || err != nil {
			t.Errorf("%s: %v, %v; want none", name, got, err)
		}
	}
}

func TestLMStudioModels(t *testing.T) {
	store := t.TempDir()
	put(t, filepath.Join(store, "lmstudio-community", "gemma-2-2b-it-GGUF", "gemma-2-2b-it-Q4_K_M.GGUF"), "gguf")
	put(t, filepath.Join(store, "mlx-community", "Qwen3-8B-MLX-4bit", "model-00001-of-00002.safetensors"), "mlx")
	put(t, filepath.Join(store, "someone", "only-adapters", "adapters.safetensors"), "not a model's weights")
	put(t, filepath.Join(store, "someone", "deeper", "sub", "model.gguf"), "too deep")
	mkdir(t, filepath.Join(store, "someone", "empty"))
	put(t, filepath.Join(store, ".internal", "x", "model.gguf"), "hidden")
	got, err := lmStudioModels(store)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	want := []string{"lmstudio-community/gemma-2-2b-it-GGUF", "mlx-community/Qwen3-8B-MLX-4bit"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("models %v, want %v", got, want)
	}
	if got, err := lmStudioModels(t.TempDir()); len(got) != 0 || err != nil {
		t.Errorf("empty store: %v, %v; want none", got, err)
	}
}

func TestStorePath(t *testing.T) {
	home := t.TempDir()
	sep := string(filepath.Separator)
	host := &fakeModelHost{env: map[string]string{"SYSTEMDRIVE": home}}
	ms := &modelScan{s: NewModelScanner(host, person)}
	env := ms.profileEnv(home)
	for in, want := range map[string]string{
		`%USERPROFILE%\.ollama\models`: home + sep + ".ollama" + sep + "models",
		`%SystemDrive%\models\`:        home + sep + "models",
		`%LOCALAPPDATA%/models`:        filepath.Join(home, "AppData", "Local", "models"),
		`%UNSET%\models`:               "",
		`models`:                       "",
		``:                             "",
	} {
		if got := storePath(expandPercent(in, env)); got != want {
			t.Errorf("storePath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCapModelNames(t *testing.T) {
	var many []string
	for i := 70; i > 0; i-- {
		many = append(many, fmt.Sprintf("model%02d:latest", i))
	}
	kept, cut := capModelNames(append(many, "model01:latest"))
	if !cut || len(kept) != maxModelNames || kept[0] != "model01:latest" || kept[63] != "model64:latest" {
		t.Errorf("70 names: kept %d (%v ... %v), cut %v; want the first 64 sorted, cut", len(kept), kept[0], kept[len(kept)-1], cut)
	}
	long := strings.Repeat("x", maxModelNameChars+1)
	kept, cut = capModelNames([]string{"b:1", long, "a:1", "a:1"})
	if !cut || !reflect.DeepEqual(kept, []string{"a:1", "b:1"}) {
		t.Errorf("an over-long name: kept %v, cut %v", kept, cut)
	}
	kept, cut = capModelNames([]string{strings.Repeat("é", maxModelNameChars)})
	if cut || len(kept) != 1 {
		t.Errorf("a name of exactly %d characters: kept %v, cut %v", maxModelNameChars, kept, cut)
	}
	if kept, cut = capModelNames(nil); kept != nil || cut {
		t.Errorf("no names: %v, %v", kept, cut)
	}
}

// What the scan cannot read is reported; what it can still produces records.
func TestModelScannerReportsWhatItCouldNotRead(t *testing.T) {
	host := modelTree(t)
	host.procsErr = errors.New("access denied")
	host.listenErr = map[int]error{11434: errors.New("GetExtendedTcpTable: failed")}
	recs, errs := NewModelScanner(host, person).Scan(context.Background(), modelCatalog())
	if len(errs) != 2 {
		t.Fatalf("errors %v, want the process list and the listeners", errs)
	}
	for _, r := range summariseModels(t, recs) {
		if r.app == "ollama" && r.userRef == "u_4444" && r.basis != "model_store" {
			t.Errorf("Ada's Ollama is %s with no listener table", r.basis)
		}
	}

	host = modelTree(t)
	host.profilesErr = errors.New("ProfileList unreadable")
	host.profiles = nil
	recs, errs = NewModelScanner(host, person).Scan(context.Background(), modelCatalog())
	if len(errs) != 1 || len(recs) != 0 {
		t.Fatalf("records %v, errors %v; want the profile error alone", recs, errs)
	}
}

func TestModelScannerWithoutRuntimesReadsNothing(t *testing.T) {
	host := modelTree(t)
	b := modelCatalog()
	b.Catalog = b.Catalog[2:]
	recs, errs := NewModelScanner(host, person).Scan(context.Background(), b)
	if len(recs) != 0 || len(errs) != 0 || len(host.infoCalls) != 0 || len(host.listenCalls) != 0 {
		t.Fatalf("records %v, errors %v, lookups %v %v; want none", recs, errs, host.infoCalls, host.listenCalls)
	}
}

func TestModelScannerStopsWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	recs, errs := NewModelScanner(modelTree(t), person).Scan(ctx, modelCatalog())
	if len(recs) != 0 || len(errs) != 1 || !errors.Is(errs[0], context.Canceled) {
		t.Fatalf("records %v, errors %v; want the cancellation alone", recs, errs)
	}
}

// Through the provider and the real emitter: one valid local_model envelope per runtime and user,
// with the model names; a store of more than 64 models carries the first 64 and counts one dropped.
func TestModelScanEmitsDiscoveryEnvelopes(t *testing.T) {
	host := modelTree(t)
	lms := filepath.Join(host.profiles[0].Dir, ".lmstudio", "models", "bulk")
	for i := range 70 {
		put(t, filepath.Join(lms, fmt.Sprintf("model-%02d", i), "weights.gguf"), "gguf")
	}
	dir, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 8, 9, 30, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	b := modelCatalog()
	pipe := &recordingPipeline{}
	em, err := discovery.New(discovery.Config{Pipeline: pipe, Dir: dir, Clock: clock, Bundles: func() *policy.Bundle { return b }, DeviceID: testDevice})
	if err != nil {
		t.Fatal(err)
	}
	p := New(Config{Scanners: []Scanner{NewModelScanner(host, person)}, Emitter: em, Bundles: func() *policy.Bundle { return b }, Clock: clock, After: newTicks().after})
	if err := p.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer p.Stop(context.Background())

	type envelope struct {
		Kind            string   `json:"kind"`
		Route           string   `json:"source"`
		ToolFingerprint string   `json:"tool_fingerprint"`
		DiscoveryType   string   `json:"discovery_type"`
		DetectionBasis  string   `json:"detection_basis"`
		AppVersion      string   `json:"app_version"`
		ModelNames      []string `json:"model_names"`
		UserRef         string   `json:"user_ref"`
		Direction       string   `json:"direction"`
	}
	got := map[string]envelope{}
	for _, raw := range pipe.raw {
		var e envelope
		if err := json.Unmarshal(raw, &e); err != nil {
			t.Fatal(err)
		}
		if e.Kind != "discovery" || e.Route != "inv.scan" || e.DiscoveryType != "local_model" || e.Direction != "none" {
			t.Errorf("envelope %+v is not a local_model discovery on inv.scan", e)
		}
		got[e.ToolFingerprint+" "+e.UserRef] = e
	}
	if len(pipe.raw) != 4 {
		t.Fatalf("%d envelopes, want 4: %v", len(pipe.raw), got)
	}
	ollama := got["app:ollama u_4444"]
	if ollama.DetectionBasis != "port_listen" || ollama.AppVersion != "0.40.2.0" ||
		!reflect.DeepEqual(ollama.ModelNames, []string{"jmorganca/custom:latest", "llama3.2:1b", "qwen2.5:0.5b"}) {
		t.Errorf("Ada's Ollama: %+v", ollama)
	}
	lmStudio := got["app:lm_studio u_4444"]
	if lmStudio.DetectionBasis != "model_store" || len(lmStudio.ModelNames) != maxModelNames || lmStudio.ModelNames[0] != "bulk/model-00" {
		t.Errorf("Ada's LM Studio: %s with %d names starting %v", lmStudio.DetectionBasis, len(lmStudio.ModelNames), lmStudio.ModelNames[:1])
	}
	if grace := got["app:ollama u_1001"]; grace.DetectionBasis != "model_store" || grace.ModelNames != nil {
		t.Errorf("Grace's Ollama: %+v", grace)
	}
	c := p.Counters().Cumulative()
	if c[protocol.CounterEmitted] != 4 || c[protocol.CounterDropped] != 1 || c[protocol.CounterErrors] != 0 {
		t.Fatalf("counters %v, want 4 emitted and 1 dropped", c)
	}
	if h := p.Health(); h.State != protocol.StateHealthy {
		t.Fatalf("health %s/%s, want healthy", h.State, h.Detail)
	}
}

var _ CountedScanner = (*ModelScanner)(nil)
