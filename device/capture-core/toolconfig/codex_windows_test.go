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

// The system config is beside it.
func TestCodexConfigPath(t *testing.T) {
	want := filepath.Join(os.Getenv("ProgramData"), "OpenAI", "Codex", "config.toml")
	if got := codexConfigPath(); !strings.EqualFold(got, want) {
		t.Fatalf("config path = %q, want %q", got, want)
	}
}

// Every user config checked is a profile's .codex\config.toml.
func TestCodexUserConfigsAreInProfiles(t *testing.T) {
	for _, p := range codexUserConfigs() {
		if !strings.EqualFold(filepath.Base(p), "config.toml") || !strings.EqualFold(filepath.Base(filepath.Dir(p)), ".codex") {
			t.Fatalf("user config %q is not a profile's .codex config.toml", p)
		}
	}
}
