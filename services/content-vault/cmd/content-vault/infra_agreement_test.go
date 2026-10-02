package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The deployment and the binary declare one vocabulary in two places, and nothing but a test keeps
// them equal. This is that test, and it reads the Bicep and this package's own source rather than a
// hand-maintained list: a list is the thing that rots, and the names were found drifted twice by hand
// before this existed. It runs inside `go test ./...`, where the acceptance gate looks.
//
// It is a sibling of the identical test in services/ingest-api/cmd/ingest-api: separate modules, no
// shared dependency, same discipline. lab/tools/check-config-agreement.mjs asserts the same property
// across both services, the Dockerfiles and the compose file in one report.

var (
	bicepEnvRE = regexp.MustCompile(`SAC_[A-Z0-9_]+`)
	goEnvRE    = regexp.MustCompile(`Env\w*\s*=\s*"(SAC_[A-Z0-9_]+)"`)
	appRE      = regexp.MustCompile(`appName:\s*'([^']+)'`)
)

// repoRootFromTest walks up from the package directory to the repository root, identified by
// infra/main.bicep, rather than counting `../` — so the test keeps working if the package moves.
func repoRootFromTest(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "infra", "main.bicep")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not find infra/main.bicep above %s", dir)
		}
		dir = parent
	}
}

// namesPassedTo returns the SAC_* names the deployment passes to one container app.
func namesPassedTo(t *testing.T, bicepPath, appName string) map[string]bool {
	t.Helper()
	b, err := os.ReadFile(bicepPath)
	if err != nil {
		t.Fatalf("read %s: %v", bicepPath, err)
	}
	text := string(b)
	locs := appRE.FindAllStringSubmatchIndex(text, -1)
	if len(locs) == 0 {
		t.Fatalf("%s declares no appName blocks; the parser needs revisiting", bicepPath)
	}
	found := false
	out := map[string]bool{}
	for i, loc := range locs {
		name := text[loc[2]:loc[3]]
		end := len(text)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		if name != appName {
			continue
		}
		found = true
		for _, n := range bicepEnvRE.FindAllString(text[loc[0]:end], -1) {
			out[n] = true
		}
	}
	if !found {
		t.Fatalf("%s has no container app named %q; if the app was renamed, rename it here too", bicepPath, appName)
	}
	return out
}

// namesReadByThisBinary reads the SAC_* names this package declares, from its own source.
func namesReadByThisBinary(t *testing.T) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	out := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		b, err := os.ReadFile(e.Name())
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		for _, m := range goEnvRE.FindAllStringSubmatch(string(b), -1) {
			out[m[1]] = true
		}
	}
	if len(out) == 0 {
		t.Fatal("no SAC_* names were found in this package; the parser needs revisiting")
	}
	return out
}

// TestDeploymentEnvironmentNamesAreRead is the F3 fix, asserted for the vault: every SAC_* name
// infra/main.bicep passes to this app must be a name this binary reads.
func TestDeploymentEnvironmentNamesAreRead(t *testing.T) {
	root := repoRootFromTest(t)
	passed := namesPassedTo(t, filepath.Join(root, "infra", "main.bicep"), "content-vault")
	read := namesReadByThisBinary(t)

	var unread []string
	for name := range passed {
		if !read[name] {
			unread = append(unread, name)
		}
	}
	sort.Strings(unread)
	if len(unread) > 0 {
		t.Errorf("infra/main.bicep passes these to the content-vault app and nothing in this binary reads them: %v", unread)
	}
	if len(passed) == 0 {
		t.Fatal("no SAC_* names found for the content-vault app; the parser needs revisiting")
	}
}

// TestEveryReadNameIsEitherPassedOrDocumented: a binary-only name must be a deliberate extension with
// a reason, or the next reader will assume the deployment passes it.
func TestEveryReadNameIsEitherPassedOrDocumented(t *testing.T) {
	root := repoRootFromTest(t)
	passed := namesPassedTo(t, filepath.Join(root, "infra", "main.bicep"), "content-vault")
	read := namesReadByThisBinary(t)

	// Image and laptop defaults. The vault's ingress acknowledgement is here twice over:
	// SAC_INTERNAL_ONLY is *passed* by the deployment (the app has internal ingress), and
	// SAC_ALLOW_NON_LOOPBACK is the image-level spelling of the same statement for a person running
	// the binary directly.
	extensions := map[string]string{
		EnvHTTPAddr:         "the image sets the listen address: a container's loopback is unreachable",
		EnvStore:            "the image sets the store mode",
		EnvKeyBackend:       "the image sets the key backend; the deployment's key custody arrives through SAC_KEYVAULT_URI once the kms backend exists",
		EnvAllowNonLoopback: "the image-level spelling of the acknowledgement; a deployment states the same fact as SAC_INTERNAL_ONLY=true",
	}

	var undocumented []string
	for name := range read {
		if passed[name] {
			continue
		}
		if _, ok := extensions[name]; !ok {
			undocumented = append(undocumented, name)
		}
	}
	sort.Strings(undocumented)
	if len(undocumented) > 0 {
		t.Errorf("this binary reads names the deployment does not pass and that are not documented as deliberate extensions: %v", undocumented)
	}

	// The vault's own inputs are not SAC_* and are not deployment parameters: they come from the
	// policy bundle and the key store. Naming them here keeps a reader from "fixing" them into the
	// deployment vocabulary.
	for _, serviceSpecific := range []string{"CONTENT_VAULT_SCOPE_TIERS", "CONTENT_VAULT_KMS_ENDPOINT", "CONTENT_VAULT_KMS_MODE"} {
		t.Logf("service-specific input, deliberately not a deployment parameter: %s", serviceSpecific)
	}
}
