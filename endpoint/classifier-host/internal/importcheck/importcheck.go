// Package importcheck lets a test pin a package's complete import set, so that code evaluating
// signed rules or parsing an attacker-chosen document cannot start importing os, net or exec
// without a test failing.
package importcheck

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// AssertClosed fails the test when a non-test Go file in dir imports a package not in allowed.
func AssertClosed(t *testing.T, dir string, allowed ...string) {
	t.Helper()
	ok := map[string]bool{}
	for _, a := range allowed {
		ok[a] = true
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("importcheck: %v", err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || filepath.Ext(name) != ".go" || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("importcheck: %v", err)
		}
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatalf("importcheck: %s: %v", name, err)
			}
			if !ok[path] {
				t.Errorf("%s imports %q, outside the package's permitted set %v", name, path, allowed)
			}
		}
	}
}
