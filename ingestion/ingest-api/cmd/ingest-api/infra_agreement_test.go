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
// them equal. This is that test, and it is deliberately a *static* one: it reads the Bicep and this
// package's own source rather than a hand-maintained list, because a list is the thing that rots —
// the names were found drifted twice by hand before this existed.
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
// repository root, identified by azure/main.bicep. Walking rather than counting `../` keeps the test
// honest if the package moves.
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

// namesPassedTo returns the SAC_* names the deployment passes to one container app, read from the
// app's block in infra/main.bicep.
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

// TestDeploymentEnvironmentNamesAreRead is the F3 fix, asserted.
//
// Every SAC_* name infra/main.bicep passes to this app must be a name this binary reads. The
// deployment naming a variable nothing reads is the exact defect this test exists to prevent: the
// environment then looks configured and is not.
func TestDeploymentEnvironmentNamesAreRead(t *testing.T) {
	root := repoRootFromTest(t)
	passed := namesPassedTo(t, filepath.Join(root, "azure", "main.bicep"), "ingest-api")
	read := namesReadByThisBinary(t)

	var unread []string
	for name := range passed {
		if !read[name] {
			unread = append(unread, name)
		}
	}
	sort.Strings(unread)
	if len(unread) > 0 {
		t.Errorf("infra/main.bicep passes these to the ingest-api app and nothing in this binary reads them: %v\n"+
			"Either read them (and say at startup whether they are used) or stop passing them.", unread)
	}
	if len(passed) == 0 {
		t.Fatal("no SAC_* names found for the ingest-api app; the parser needs revisiting")
	}
}

// TestEveryReadNameIsEitherPassedOrDocumented is the other direction: a binary-only name must be a
// deliberate extension, listed here with its reason. Without this, a typo'd environment variable
// name would look supported in the binary and be ignored in every deployment.
//
// Two kinds of extension are allowed, and they are different things:
//   - imageDefaults: the image sets it, because a container platform cannot mount a repository path
//     and this deployment's Bicep has no command/args to pass one in;
//   - deploymentGaps: the name belongs to the deployment and infra/main.bicep does not pass it
//     today. These are reported on every run rather than merely tolerated, because "the binary
//     supports region pinning" is misleading while nothing tells it the region.
func TestEveryReadNameIsEitherPassedOrDocumented(t *testing.T) {
	root := repoRootFromTest(t)
	passed := namesPassedTo(t, filepath.Join(root, "azure", "main.bicep"), "ingest-api")
	read := namesReadByThisBinary(t)

	imageDefaults := map[string]string{
		EnvHTTPAddr:   "the image sets the listen address: a container's loopback is unreachable, and infra/main.bicep has no command/args to pass one in",
		EnvStore:      "the image sets the store mode; a deployment could override it once a driver exists",
		EnvSchema:     "the image stores the contract schema at a path a container can read; the repository path does not exist inside one",
		EnvRoutesFile: "same as SAC_SCHEMA: ref.route_fidelity is shipped in the image as data, never compiled in (§4.4)",
	}
	// EnvAuthModes, EnvTLSClientCertHeader and EnvTLSClientCAPEM are deliberately absent: the
	// deployment now passes all three to this app (azure/main.bicep ingestApp), which is the ADR 0020
	// forwarded x509 path. The entries that remain are real gaps, reported on every run.
	deploymentGaps := map[string]string{
		EnvRegion: "§12's region pinning: the binary refuses a tenant pinned to another region, but infra/main.bicep passes no region, so the check is inert in Azure until it does",
		// The forwarded path needs no server key pair, so only a deployment that terminates client
		// TLS at the origin (ADR 0019 mechanism A) would pass these.
		EnvTLSCertPEM: "the server certificate, only for an origin that terminates client TLS itself; the deployment uses the Application Gateway forwarded path",
		EnvTLSKeyPEM:  "the server private key — see " + EnvTLSCertPEM,
		// ADR 0020 decision 2's DPoP half: the deployment serves x509 only today, so these are still
		// unwired. DPoP also needs control-api's token material before it is usable.
		EnvDPoPTokenPublicPEM: "ADR 0020: the DPoP access-token verification key; the deployment serves x509 only today",
		EnvDPoPIssuer:         "ADR 0020: the DPoP access token `iss` — see " + EnvDPoPTokenPublicPEM,
		EnvDPoPAudience:       "ADR 0020: the DPoP access token `aud` — see " + EnvDPoPTokenPublicPEM,
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

	// Reported every run: a documented gap that nobody sees is a gap that becomes permanent.
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

	// An extension that has since been added to the Bicep is not an error, but the list must not
	// silently keep a stale reason.
	for name := range imageDefaults {
		if passed[name] {
			t.Logf("%s is now passed by infra/main.bicep as well as being an image default; the extension note is stale", name)
		}
	}
}
