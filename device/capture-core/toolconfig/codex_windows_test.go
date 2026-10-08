//go:build windows

package toolconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The system requirements file is in %ProgramData%\OpenAI\Codex.
func TestCodexRequirementsPath(t *testing.T) {
	want := filepath.Join(os.Getenv("ProgramData"), "OpenAI", "Codex", "requirements.toml")
	if got := codexRequirementsPath(); !strings.EqualFold(got, want) {
		t.Fatalf("requirements path = %q, want %q", got, want)
	}
}
