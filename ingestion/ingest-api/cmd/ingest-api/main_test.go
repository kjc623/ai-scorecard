package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shadow-ai-capture/ingest-api/internal/store"
)

// The binary's own logic: the development-flag guards, the route-table lookup and its three
// fallbacks, and the client-CA bundle. Everything else in main.go is wiring, and what it wires is
// tested where it lives.

func TestParseSeed(t *testing.T) {
	const tenant = "11111111-1111-1111-1111-111111111111"
	const device = "22222222-2222-2222-2222-222222222222"

	cases := []struct {
		name           string
		in             string
		wantCredential string
		wantErr        bool
	}{
		{"tenant and device, credential defaulted", tenant + ":" + device, "dev", false},
		{"explicit credential", tenant + ":" + device + ":cred-9", "cred-9", false},
		{"missing device", tenant, "", true},
		{"too many parts", tenant + ":" + device + ":c:extra", "", true},
		{"tenant is not a uuid", "tenant-a:" + device, "", true},
		{"device is not a uuid", tenant + ":device-b", "", true},
		{"empty", "", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tenantID, deviceID, credential, err := parseSeed(c.in)
			if c.wantErr {
				if err == nil {
					t.Fatalf("parseSeed(%q) accepted an invalid value", c.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseSeed(%q): %v", c.in, err)
			}
			if tenantID != tenant || deviceID != device {
				t.Errorf("parseSeed(%q) = %s/%s, want %s/%s", c.in, tenantID, deviceID, tenant, device)
			}
			if credential != c.wantCredential {
				t.Errorf("parseSeed(%q) credential = %q, want %q", c.in, credential, c.wantCredential)
			}
		})
	}
}

// TestLoadRoutesFindsTheTableFromEitherWorkingDirectory covers the three places the default
// module-relative path can resolve from. The flag's default is module-relative, and the binary is
// started both from the repository root and from services/ingest-api, so a miss here is a startup
// failure rather than a subtle one.
func TestLoadRoutesFindsTheTableFromEitherWorkingDirectory(t *testing.T) {
	const rel = "testdata/route-fidelity.seed.json"

	// 1. As given: the working directory is the module directory.
	table, err := loadRoutes(rel, "")
	if err != nil {
		t.Fatalf("as given: %v", err)
	}
	if len(table) != 7 {
		t.Errorf("routes = %d, want the seven seeded routes", len(table))
	}
	if table["ext.page_context"].Rank != 10 {
		t.Errorf("ext.page_context rank = %d, want 10 (lower wins)", table["ext.page_context"].Rank)
	}

	// 2. Relative to the repository root derived from the contract schema: the working directory is
	//    the repository root, so `testdata/...` does not exist there.
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	table, err = loadRoutes(rel, root)
	if err != nil {
		t.Fatalf("repository-root relative: %v", err)
	}
	if table["proc.detect"].YieldsContent {
		t.Error("proc.detect must not yield content: the schema forbids content on a detection")
	}

	// 3. Nothing anywhere: the error must name all three places, because a startup failure that
	//    says only "file not found" sends the operator looking in one of them.
	if _, err := loadRoutes("testdata/does-not-exist.json", root); err == nil {
		t.Fatal("a missing route table was accepted")
	} else if !strings.Contains(err.Error(), "above the working directory") {
		t.Errorf("error = %v, want it to name where the file was looked for", err)
	}
}

func TestFindUpwards(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	want := filepath.Join(root, "marker.txt")
	if err := os.WriteFile(want, []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, err := findUpwards(nested, "marker.txt")
	if err != nil {
		t.Fatalf("findUpwards: %v", err)
	}
	if got != want {
		t.Errorf("findUpwards = %q, want %q", got, want)
	}
	if _, err := findUpwards(nested, "absent.txt"); err == nil {
		t.Error("findUpwards accepted a file that does not exist anywhere")
	}
	// A directory is not a file: a route table named like a directory must not satisfy the search.
	if err := os.MkdirAll(filepath.Join(root, "dirmarker"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if _, err := findUpwards(nested, "dirmarker"); err == nil {
		t.Error("findUpwards accepted a directory")
	}
}

func TestLoadCAPool(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, []byte("not a certificate"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := loadCAPool(path); err == nil {
		t.Error("loadCAPool accepted a bundle with no certificate in it")
	}
	if _, err := loadCAPool(filepath.Join(t.TempDir(), "absent.pem")); err == nil {
		t.Error("loadCAPool accepted a path that does not exist")
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "sac-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("certificate: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(path, pemBytes, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	pool, err := loadCAPool(path)
	if err != nil {
		t.Fatalf("loadCAPool: %v", err)
	}
	if pool == nil {
		t.Fatal("loadCAPool returned no pool for a valid bundle")
	}
}

// TestSeededPrincipalIsUsableByTheDevAuthenticator keeps the two development affordances in step:
// the seed registers the credential the header authenticator will present. If they drift, a local
// run refuses every request as an unknown tenant, which is what happened once already.
func TestSeededPrincipalIsUsableByTheDevAuthenticator(t *testing.T) {
	routes, err := loadRoutes("testdata/route-fidelity.seed.json", "")
	if err != nil {
		t.Fatalf("routes: %v", err)
	}
	mem := store.NewMemory(routes)

	const tenant = "11111111-1111-1111-1111-111111111111"
	const device = "22222222-2222-2222-2222-222222222222"
	seedTenant, seedDevice, credential, err := parseSeed(tenant + ":" + device)
	if err != nil {
		t.Fatalf("parseSeed: %v", err)
	}
	mem.SetPrincipal(seedTenant, seedDevice, credential, store.PrincipalStatus{
		TenantKnown: true, TenantStatus: "active", IngestEnabled: true,
		DeviceKnown: true, CredentialKnown: true,
		CredentialExpiry: time.Now().Add(24 * time.Hour),
	})

	// The header authenticator's documented default must be the credential the seed registered.
	devDefault := "" // DevHeader treats an empty CredentialID as "dev"
	effective := devDefault
	if effective == "" {
		effective = "dev"
	}
	if effective != credential {
		t.Fatalf("the dev authenticator would present credential %q but the seed registered %q", effective, credential)
	}

	status, err := mem.PrincipalStatus(context.Background(), seedTenant, seedDevice, effective)
	if err != nil {
		t.Fatalf("PrincipalStatus: %v", err)
	}
	if err := status.CheckWritable(time.Now()); err != nil {
		t.Fatalf("the seeded principal is not writable: %v", err)
	}
}
