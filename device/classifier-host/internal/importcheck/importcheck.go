// Package importcheck is a test-only helper for the structural import assertions: a signed rule
// file, and a document handed to the parser child, must not be able to reach the network, the
// spool, the key material or a shell. The platform sandbox of §10 is one answer to that; "the code
// that evaluates them cannot name those packages at all" is a second, cheaper one, and it is the
// one a unit test can hold.
package importcheck

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// Imports returns every import path of the non-test Go files in dir.
func Imports(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("importcheck: reading %s: %v", dir, err)
	}
	seen := map[string]bool{}
	var out []string
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || filepath.Ext(name) != ".go" || len(name) > 8 && name[len(name)-8:] == "_test.go" {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("importcheck: parsing %s: %v", name, err)
		}
		for _, imp := range f.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatalf("importcheck: %s: bad import path %s", name, imp.Path.Value)
			}
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	return out
}

// AssertClosed fails the test when dir imports anything outside allowed. The allowlist is the
// point: it is a statement of what this code is *permitted* to depend on, so adding a dependency
// is a deliberate act rather than a side effect.
func AssertClosed(t *testing.T, dir string, allowed ...string) {
	t.Helper()
	ok := map[string]bool{}
	for _, a := range allowed {
		ok[a] = true
	}
	for _, imp := range Imports(t, dir) {
		if !ok[imp] {
			t.Errorf("%s imports %q, which is not in this package's closed dependency set %v", dir, imp, allowed)
		}
	}
}

// Forbidden is the set no component that evaluates attacker-influenced signed data or parses a
// document may import.
var Forbidden = []string{"os", "os/exec", "net", "net/http", "syscall", "io/ioutil", "plugin", "unsafe", "crypto/tls"}
