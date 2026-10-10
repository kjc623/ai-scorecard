package deploy_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shadow-ai-capture/device/protocol"

	"github.com/shadow-ai-capture/control-api/internal/apierr"
	"github.com/shadow-ai-capture/control-api/internal/deploy"
)

// statementFor is an agent-release.json for the test MSI; its signature is the device's to check.
func statementFor(version, file string, size int64) string {
	sum := sha256.Sum256(msiBytes)
	return fmt.Sprintf(`{"algorithm":"ed25519","payload":{"type":"agent_release","platform":"windows-amd64","version":%q,"file":%q,"sha256":%q,"size":%d},"signature":"c2ln"}`+"\n",
		version, file, hex.EncodeToString(sum[:]), size)
}

func agentMux(t *testing.T, statement string, authErr error) *http.ServeMux {
	t.Helper()
	dir := writeRelease(t, nil)
	if statement != "" {
		if err := os.WriteFile(filepath.Join(dir, deploy.AgentReleaseFileName), []byte(statement), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mux := http.NewServeMux()
	deploy.NewAgents(dir, quiet).Register(mux, func(*http.Request) (string, string, error) {
		return tenantA, "device-1", authErr
	})
	return mux
}

func TestAgentReleaseServesTheStatementAsSigned(t *testing.T) {
	want := statementFor("0.2.0", deploy.MSIFileName, int64(len(msiBytes)))
	rec := get(agentMux(t, want, nil), http.MethodGet, protocol.AgentReleasePath)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("release: %d %s %s", rec.Code, rec.Header().Get("Content-Type"), rec.Body)
	}
	if rec.Body.String() != strings.TrimSpace(want) {
		t.Fatalf("the statement was not served byte for byte:\n%s", rec.Body)
	}
}

func TestAgentPackageServesTheMSIWithRanges(t *testing.T) {
	mux := agentMux(t, statementFor("0.2.0", deploy.MSIFileName, int64(len(msiBytes))), nil)
	rec := get(mux, http.MethodGet, protocol.AgentPackagePath)
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), msiBytes) {
		t.Fatalf("package: %d, %d bytes", rec.Code, rec.Body.Len())
	}
	req := httptest.NewRequest(http.MethodGet, protocol.AgentPackagePath, nil)
	req.Header.Set("Range", "bytes=100-")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusPartialContent || !bytes.Equal(rec.Body.Bytes(), msiBytes[100:]) {
		t.Fatalf("range: %d, %d bytes", rec.Code, rec.Body.Len())
	}
}

func TestAgentRoutesRefuseAnUnauthenticatedDevice(t *testing.T) {
	mux := agentMux(t, statementFor("0.2.0", deploy.MSIFileName, int64(len(msiBytes))),
		apierr.New(http.StatusUnauthorized, apierr.CodeUnauthenticated, "a device certificate is required"))
	for _, path := range []string{protocol.AgentReleasePath, protocol.AgentPackagePath} {
		if rec := get(mux, http.MethodGet, path); rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s: %d, want 401", path, rec.Code)
		}
	}
}

func TestAgentReleaseIsUnavailableWhenTheStatementDisagreesWithTheRelease(t *testing.T) {
	size := int64(len(msiBytes))
	for name, statement := range map[string]string{
		"missing":       "",
		"other version": statementFor("0.1.9", deploy.MSIFileName, size),
		"other file":    statementFor("0.2.0", "Other.msi", size),
		"other size":    statementFor("0.2.0", deploy.MSIFileName, size+1),
		"not json":      "release",
	} {
		t.Run(name, func(t *testing.T) {
			mux := agentMux(t, statement, nil)
			for _, path := range []string{protocol.AgentReleasePath, protocol.AgentPackagePath} {
				rec := get(mux, http.MethodGet, path)
				if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), apierr.CodeReleaseUnavailable) {
					t.Fatalf("%s: %d %s, want 404 %s", path, rec.Code, rec.Body, apierr.CodeReleaseUnavailable)
				}
			}
		})
	}
}
