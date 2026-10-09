//go:build windows

package toolconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The enterprise hooks file is in %ProgramData%\Cursor.
func TestCursorHooksPath(t *testing.T) {
	want := filepath.Join(os.Getenv("ProgramData"), "Cursor", "hooks.json")
	if got := cursorHooksPath(); !strings.EqualFold(got, want) {
		t.Fatalf("hooks path = %q, want %q", got, want)
	}
}
