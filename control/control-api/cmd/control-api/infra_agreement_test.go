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
// them equal. This is that test, and it is deliberately static: it reads azure/main.bicep and this
// package's own source rather than a hand-maintained list, because a list is the thing that rots.
//
// It runs inside `go test ./...`, which is where the acceptance gate looks, so it fails loudly on the
// commit that breaks the seam rather than at the next release.

var (
	bicepEnvRE = regexp.MustCompile(`SAC_[A-Z0-9_]+`)
	// Matches this package's declarations: EnvFoo = "SAC_FOO".
	goEnvRE = regexp.MustCompile(`Env\w*\s*=\s*"(SAC_[A-Z0-9_]+)"`)
	appRE   = regexp.MustCompile(`appName:\s*'([^']+)'`)
)

// repoRootFromTest walks up from the test's working directory (the package directory) to the
// repository root, identified by azure/main.bicep.
func repoRootFromTest(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "azure", "main.bicep")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not find azure/main.bicep above %s", dir)
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
		start := loc[0]
		end := len(text)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		if name != appName {
			continue
		}
		found = true
		for _, n := range bicepEnvRE.FindAllString(text[start:end], -1) {
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

// TestDeploymentEnvironmentNamesAreRead asserts every name the deployment passes to control-api is a
// name this binary reads. The deployment naming a variable nothing reads is the defect this exists to
// prevent: the environment then looks configured and is not.
func TestDeploymentEnvironmentNamesAreRead(t *testing.T) {
	root := repoRootFromTest(t)
	passed := namesPassedTo(t, filepath.Join(root, "azure", "main.bicep"), "control-api")
	read := namesReadByThisBinary(t)

	var unread []string
	for name := range passed {
		if !read[name] {
			unread = append(unread, name)
		}
	}
	sort.Strings(unread)
	if len(unread) > 0 {
		t.Errorf("infra/main.bicep passes these to the control-api app and nothing in this binary reads them: %v", unread)
	}
	if len(passed) == 0 {
		t.Fatal("no SAC_* names found for the control-api app; the parser needs revisiting")
	}
}

// TestEveryReadNameIsEitherPassedOrDocumented is the other direction: a binary-only name must be a
// deliberate extension, listed here with its reason.
func TestEveryReadNameIsEitherPassedOrDocumented(t *testing.T) {
	root := repoRootFromTest(t)
	passed := namesPassedTo(t, filepath.Join(root, "azure", "main.bicep"), "control-api")
	read := namesReadByThisBinary(t)

	imageDefaults := map[string]string{
		EnvHTTPAddr:          "the image sets the listen address: a container's loopback is unreachable, and infra/main.bicep has no command/args to pass one in",
		EnvStore:             "the image sets the store mode; a deployment could override it once a driver exists",
		EnvRole:              "the image sets the identity too, so a person can run the same binary locally",
		EnvCredentialTTL:     "the image sets the default credential life; a deployment could override it",
		EnvEnrolmentTokenTTL: "the image sets the default token life; the request path only verifies tokens the MDM profile carried",
	}
	deploymentGaps := map[string]string{
		EnvRegion:          "§12's region pinning: the binary refuses a tenant pinned to another region, but infra/main.bicep passes no region, so the check is inert in Azure until it does",
		EnvCACertPEM:       "ADR 0020 decision 3: the LocalCA certificate as PEM text, for the module's keyVaultEnv to inject. Every app in infra/main.bicep has keyVaultEnv: [] today, so a certificate-only deployment cannot yet supply a CA",
		EnvCAKeyPEM:        "ADR 0020 decision 3: the LocalCA private key as PEM text — see " + EnvCACertPEM,
		EnvTokenIssuer:     "§5.2: the access token's `iss`. The binary has a default only when configured; the deployment passes none",
		EnvTokenAudience:   "§5.2: the access token's `aud` — see " + EnvTokenIssuer,
		EnvDPoPTokenKeyPEM: "the access-token signing key. The binary refuses to start without it; infra/main.bicep has keyVaultEnv: [] today, so no deployment injects it yet",
		EnvVaultURL:         "docs/02 §5.5: content-vault's internal address, which control-api asks for an object key when it grants an upload. infra/main.bicep passes none, so content grants are disabled in Azure until it does",
		EnvUploadSigningKey: "docs/02 §10.3: the key the storage layer verifies an upload URL with. In Azure the upload credential is a storage user-delegation SAS instead, which this build does not mint — see " + EnvVaultURL,
	}

	var undocumented []string
	for name := range read {
		if passed[name] {
			continue
		}
		if _, ok := imageDefaults[name]; ok {
			continue
		}
		if _, ok := deploymentGaps[name]; ok {
			continue
		}
		undocumented = append(undocumented, name)
	}
	sort.Strings(undocumented)
	if len(undocumented) > 0 {
		t.Errorf("this binary reads names the deployment does not pass and that are not documented as deliberate extensions: %v", undocumented)
	}

	var gaps []string
	for name, why := range deploymentGaps {
		if !passed[name] {
			gaps = append(gaps, name+" ("+why+")")
		}
	}
	sort.Strings(gaps)
	for _, g := range gaps {
		t.Logf("DEPLOYMENT GAP: %s", g)
	}
	for name := range imageDefaults {
		if passed[name] {
			t.Logf("%s is now passed by infra/main.bicep as well as being an image default; the extension note is stale", name)
		}
	}
}
