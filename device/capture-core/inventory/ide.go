package inventory

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/shadow-ai-capture/device/capture-core/core"
	"github.com/shadow-ai-capture/device/capture-core/discovery"
	"github.com/shadow-ai-capture/device/capture-core/hostinfo"
	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/protocol"
)

const (
	// maxExtensionsFile caps each extensions.json read; a larger file is not read.
	maxExtensionsFile = 4 << 20
	// maxPluginXML caps the META-INF/plugin.xml entry read from a JetBrains plugin jar.
	maxPluginXML   = 1 << 20
	pluginXMLEntry = "META-INF/plugin.xml"
)

// vsCodeFamily is the IDEs that keep a profile's extensions as VS Code does: one folder per
// extension in <profile>\<data folder>\extensions, listed in that folder's extensions.json.
var vsCodeFamily = []struct{ hostApp, dataFolder string }{
	{"app:vscode", ".vscode"},
	{"app:cursor", ".cursor"},
	{"app:windsurf", ".windsurf"},
}

// jetBrainsHostApp is the host of every JetBrains IDE's plugins.
const jetBrainsHostApp = "app:jetbrains"

// ProfileLister lists the user profiles a scan reads.
type ProfileLister interface {
	Profiles() ([]Profile, error)
}

// IDEScanner finds the catalog's IDE extensions in each user profile: VS Code, Cursor and Windsurf
// extensions, and JetBrains IDE plugins. It reads files only; it never loads or runs an extension.
type IDEScanner struct {
	profiles ProfileLister
	person   func(hostinfo.User) core.Person
}

// NewIDEScanner returns the IDE extension scanner over the profiles; person names a profile owner's
// person.
func NewIDEScanner(profiles ProfileLister, person func(hostinfo.User) core.Person) *IDEScanner {
	return &IDEScanner{profiles: profiles, person: person}
}

// Scan implements Scanner.
func (s *IDEScanner) Scan(ctx context.Context, b *policy.Bundle) ([]discovery.Record, []error) {
	return s.ScanCounted(ctx, b, nil)
}

// ScanCounted implements CountedScanner: each extension list, extension folder and plugin descriptor
// the scan reads counts observed on counters.
func (s *IDEScanner) ScanCounted(ctx context.Context, b *policy.Bundle, counters *core.CounterSet) ([]discovery.Record, []error) {
	sc := &ideScan{b: b, counters: counters}
	profiles, err := s.profiles.Profiles()
	if err != nil {
		sc.fail("listing the user profiles", err)
	}
	for _, p := range profiles {
		if ctx.Err() != nil {
			sc.errs = append(sc.errs, ctx.Err())
			break
		}
		person := s.person(p.User)
		for _, ide := range vsCodeFamily {
			sc.vsCode(&person, ide.hostApp, filepath.Join(p.Dir, ide.dataFolder, "extensions"))
		}
		sc.jetBrains(&person, filepath.Join(p.Dir, "AppData", "Roaming", "JetBrains"))
	}
	return sc.recs, sc.errs
}

// ideScan is one pass of the IDE scanner.
type ideScan struct {
	b        *policy.Bundle
	counters *core.CounterSet
	recs     []discovery.Record
	errs     []error
}

func (sc *ideScan) fail(what string, err error) {
	sc.errs = append(sc.errs, fmt.Errorf("%s: %w", what, err))
}

func (sc *ideScan) observe() {
	if sc.counters != nil {
		sc.counters.Add(protocol.CounterObserved)
	}
}

// extension is one installed extension or plugin as its IDE lists it.
type extension struct{ id, version string }

// emit records the catalog's apps among one IDE's extensions, once per app and version.
func (sc *ideScan) emit(person *core.Person, hostApp string, exts []extension) {
	seen := map[string]bool{}
	for _, e := range exts {
		ver := version(e.version)
		for _, app := range sc.b.AppByExtensionID(strings.TrimSpace(e.id)) {
			if seen[app+"\x00"+ver] {
				continue
			}
			seen[app+"\x00"+ver] = true
			owner := *person
			sc.recs = append(sc.recs, discovery.Record{
				Type:    protocol.DiscoveryTypeIDEExtension,
				Basis:   protocol.DetectionBasisExtensionScan,
				AppKey:  app,
				Version: ver,
				HostApp: hostApp,
				Person:  &owner,
			})
		}
	}
}

// vsCode reads one VS Code-family extensions folder: its extensions.json, the IDE's own list of the
// profile's extensions, or, where there is none, the folder names.
func (sc *ideScan) vsCode(person *core.Person, hostApp, dir string) {
	file := filepath.Join(dir, "extensions.json")
	data, err := readFileCapped(file, maxExtensionsFile)
	if errors.Is(err, fs.ErrNotExist) {
		sc.emit(person, hostApp, sc.extensionFolders(dir))
		return
	}
	if err != nil {
		sc.fail("reading "+file, err)
		return
	}
	sc.observe()
	exts, err := parseExtensionsJSON(data)
	if err != nil {
		sc.fail("decoding "+file, err)
		return
	}
	sc.emit(person, hostApp, exts)
}

// extensionFolders lists the extensions an extensions folder holds by their folder names.
func (sc *ideScan) extensionFolders(dir string) []extension {
	entries, err := readDir(dir)
	if err != nil {
		sc.fail("reading "+dir, err)
	}
	var out []extension
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if id, ver, ok := parseExtensionFolder(e.Name()); ok {
			sc.observe()
			out = append(out, extension{id, ver})
		}
	}
	return out
}

// parseExtensionsJSON reads the entries of an extensions.json: an array of
// {"identifier":{"id":...},"version":...,"location":...,"relativeLocation":...,"metadata":...}.
// An empty file is an empty list.
func parseExtensionsJSON(data []byte) ([]extension, error) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return nil, nil
	}
	var stored []struct {
		Identifier struct {
			ID string `json:"id"`
		} `json:"identifier"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(data, &stored); err != nil {
		return nil, err
	}
	out := make([]extension, 0, len(stored))
	for _, e := range stored {
		if e.Identifier.ID != "" {
			out = append(out, extension{e.Identifier.ID, e.Version})
		}
	}
	return out, nil
}

// extensionFolderName is how VS Code names an extension's folder: <publisher>.<name>-<version>,
// then -<target platform> for a platform-specific build.
var extensionFolderName = regexp.MustCompile(`^([^.]+\..+)-(\d+\.\d+\.\d+)(-(.+))?$`)

func parseExtensionFolder(name string) (id, ver string, ok bool) {
	m := extensionFolderName.FindStringSubmatch(name)
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

// jetBrains reads the plugins of each JetBrains IDE configuration under root, one folder per product
// and version (<Product><Version>\plugins\<plugin>).
func (sc *ideScan) jetBrains(person *core.Person, root string) {
	products, err := readDir(root)
	if err != nil {
		sc.fail("reading "+root, err)
	}
	var exts []extension
	for _, product := range products {
		if !product.IsDir() {
			continue
		}
		plugins := filepath.Join(root, product.Name(), "plugins")
		entries, err := readDir(plugins)
		if err != nil {
			sc.fail("reading "+plugins, err)
		}
		for _, plugin := range entries {
			if !plugin.IsDir() {
				continue
			}
			if e, ok := sc.plugin(filepath.Join(plugins, plugin.Name())); ok {
				exts = append(exts, e)
			}
		}
	}
	sc.emit(person, jetBrainsHostApp, exts)
}

// plugin reads a plugin's id and version from the META-INF/plugin.xml of the first jar in its lib
// folder that has one, trying the jars named after the plugin's folder first.
func (sc *ideScan) plugin(dir string) (extension, bool) {
	lib := filepath.Join(dir, "lib")
	entries, err := readDir(lib)
	if err != nil {
		sc.fail("reading "+lib, err)
	}
	var jars []string
	for _, e := range entries {
		if !e.IsDir() && strings.EqualFold(filepath.Ext(e.Name()), ".jar") {
			jars = append(jars, e.Name())
		}
	}
	prefix := strings.ToLower(filepath.Base(dir))
	slices.SortStableFunc(jars, func(a, b string) int {
		pa, pb := strings.HasPrefix(strings.ToLower(a), prefix), strings.HasPrefix(strings.ToLower(b), prefix)
		switch {
		case pa && !pb:
			return -1
		case pb && !pa:
			return 1
		}
		return strings.Compare(a, b)
	})
	for _, name := range jars {
		jar := filepath.Join(lib, name)
		data, err := readJarEntry(jar, pluginXMLEntry, maxPluginXML)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			sc.fail("reading "+pluginXMLEntry+" in "+jar, err)
			continue
		}
		sc.observe()
		e, err := parsePluginXML(data)
		if err != nil {
			sc.fail("decoding "+pluginXMLEntry+" in "+jar, err)
			return extension{}, false
		}
		return e, e.id != ""
	}
	return extension{}, false
}

// parsePluginXML reads a plugin descriptor's <id> and <version>. A descriptor without an <id> is
// identified by its <name>, as the IDE identifies it.
func parsePluginXML(data []byte) (extension, error) {
	var d struct {
		XMLName xml.Name `xml:"idea-plugin"`
		ID      string   `xml:"id"`
		Name    string   `xml:"name"`
		Version string   `xml:"version"`
	}
	if err := xml.Unmarshal(data, &d); err != nil {
		return extension{}, err
	}
	id := strings.TrimSpace(d.ID)
	if id == "" {
		id = strings.TrimSpace(d.Name)
	}
	return extension{id, strings.TrimSpace(d.Version)}, nil
}

// readJarEntry reads one entry of a jar of at most limit bytes uncompressed; fs.ErrNotExist when
// the jar has no such entry.
func readJarEntry(jar, entry string, limit int64) ([]byte, error) {
	if err := regularFile(jar); err != nil {
		return nil, err
	}
	r, err := zip.OpenReader(jar)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	for _, f := range r.File {
		if f.Name != entry {
			continue
		}
		if f.UncompressedSize64 > uint64(limit) {
			return nil, fmt.Errorf("larger than %d bytes", limit)
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		return readAtMost(rc, limit)
	}
	return nil, fs.ErrNotExist
}

// readFileCapped reads a regular file of at most limit bytes.
func readFileCapped(file string, limit int64) ([]byte, error) {
	if err := regularFile(file); err != nil {
		return nil, err
	}
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readAtMost(f, limit)
}

// regularFile checks a path is a regular file before it is opened, so that opening it cannot block
// on a device or a pipe.
func regularFile(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return errors.New("not a regular file")
	}
	return nil
}

func readAtMost(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("larger than %d bytes", limit)
	}
	return data, nil
}
