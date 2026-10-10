package deploy_test

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shadow-ai-capture/control-api/internal/apierr"
	"github.com/shadow-ai-capture/control-api/internal/deploy"
	"github.com/shadow-ai-capture/control-api/internal/enrol"
	"github.com/shadow-ai-capture/control-api/internal/store"
	"github.com/shadow-ai-capture/control-api/internal/store/storetest"
)

const (
	tenantA     = "5a3c0de0-7e57-4a11-9000-0000000d3a01"
	productCode = "{1C9E6E3A-2B4D-4F5A-9C8B-7D6E5F4A3B2C}"
	upgradeCode = "{7E9C2B7A-6D0E-4C6A-9F2B-1A6E6C2D44A1}"
	packageCode = "{0A1B2C3D-4E5F-4061-8273-8495A6B7C8D9}"
	endpoint    = "https://devices.eu.example.com"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// msiBytes stands in for the generic MSI: deterministic, a few blocks long and not block-aligned,
// so padding is exercised.
var msiBytes = bytes.Repeat([]byte("ShadowAICapture.msi payload "), 4099)

func writeRelease(t *testing.T, mutate func(*deploy.Release)) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, deploy.MSIFileName), msiBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(msiBytes)
	rel := deploy.Release{
		Version: "0.2.0", ProductCode: strings.ToLower(strings.Trim(productCode, "{}")), UpgradeCode: upgradeCode,
		PackageCode: packageCode, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(msiBytes)),
		Publisher: "Shadow AI Capture", Signed: false,
	}
	if mutate != nil {
		mutate(&rel)
	}
	raw, _ := json.Marshal(rel)
	if err := os.WriteFile(filepath.Join(dir, deploy.ReleaseFileName), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

type fakeScim struct {
	tokens  []deploy.ScimToken
	revoked []string
}

func (f *fakeScim) Create(_ context.Context, tenantID, label, createdBy string) (string, string, error) {
	id, _ := store.NewUUID()
	f.tokens = append(f.tokens, deploy.ScimToken{TokenID: id, Label: label, CreatedAt: time.Now()})
	return id, "sacscim_" + tenantID + ".secret", nil
}

func (f *fakeScim) Revoke(_ context.Context, tenantID, tokenID, revokedBy string) error {
	for _, tk := range f.tokens {
		if tk.TokenID == tokenID {
			f.revoked = append(f.revoked, tokenID)
			return nil
		}
	}
	return fmt.Errorf("scim token %s: %w", tokenID, deploy.ErrNotFound)
}

func (f *fakeScim) List(context.Context, string) ([]deploy.ScimToken, error) { return f.tokens, nil }

type rig struct {
	store *storetest.Memory
	scim  *fakeScim
	mux   *http.ServeMux
	now   time.Time
}

var admin = deploy.Principal{Tenant: tenantA, Actor: "admin@contoso.example", Subject: "conn:oid-1", Roles: []string{"viewer", "admin"}}

func newRig(t *testing.T, releaseDir string) *rig {
	t.Helper()
	r := &rig{store: storetest.New(), scim: &fakeScim{}, mux: http.NewServeMux(),
		now: time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)}
	r.store.AddTenant(store.Tenant{TenantID: tenantA, Status: "active", IngestEnabled: true})
	// The authenticator reads a test header naming which principal the "token" carries.
	auth := func(req *http.Request) (deploy.Principal, error) {
		switch req.Header.Get("X-Test-Principal") {
		case "admin":
			return admin, nil
		case "analyst":
			return deploy.Principal{Tenant: tenantA, Actor: "analyst@contoso.example", Roles: []string{"viewer", "analyst", "content_reader"}}, nil
		}
		return deploy.Principal{}, errors.New("no token")
	}
	h, err := deploy.NewHandler(r.store, auth, r.scim, deploy.Config{
		ReleaseDir: releaseDir, DeviceEndpoint: endpoint, ScimBaseURL: "https://app.example.com/scim/v2",
		Now: func() time.Time { return r.now }, Logger: quiet,
	})
	if err != nil {
		t.Fatal(err)
	}
	h.Register(r.mux)
	return r
}

func (r *rig) do(t *testing.T, principal, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if principal != "" {
		req.Header.Set("X-Test-Principal", principal)
	}
	rec := httptest.NewRecorder()
	r.mux.ServeHTTP(rec, req)
	return rec
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var e struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &e)
	return e.Error.Code
}

var routes = []struct{ method, path, body string }{
	{"GET", "/admin/v1/deployment", ""},
	{"POST", "/admin/v1/deployment/package", `{"format":"zip"}`},
	{"POST", "/admin/v1/deployment/keys/0f6a2b1c-3d4e-4f5a-8b6c-7d8e9f0a1b2c/revoke", ""},
	{"PUT", "/admin/v1/deployment/verification", `{"device_verification":"none"}`},
	{"POST", "/admin/v1/scim/tokens", `{"label":"entra"}`},
	{"POST", "/admin/v1/scim/tokens/0f6a2b1c-3d4e-4f5a-8b6c-7d8e9f0a1b2c/revoke", ""},
}

// TestEveryRouteRefusesANonAdmin: no token is 401, a token without the admin role is 403, and
// neither writes anything.
func TestEveryRouteRefusesANonAdmin(t *testing.T) {
	r := newRig(t, writeRelease(t, nil))
	for _, rt := range routes {
		if rec := r.do(t, "", rt.method, rt.path, rt.body); rec.Code != http.StatusUnauthorized || errorCode(t, rec) != apierr.CodeUnauthenticated {
			t.Errorf("%s %s without a token: %d %s", rt.method, rt.path, rec.Code, rec.Body)
		}
		if rec := r.do(t, "analyst", rt.method, rt.path, rt.body); rec.Code != http.StatusForbidden || errorCode(t, rec) != apierr.CodeForbidden {
			t.Errorf("%s %s as analyst: %d %s", rt.method, rt.path, rec.Code, rec.Body)
		}
	}
	if len(r.store.Audits()) != 0 || len(r.store.DeploymentKeys(tenantA)) != 0 || len(r.scim.tokens) != 0 {
		t.Fatal("a refused request wrote something")
	}
}

func download(t *testing.T, r *rig, format string) (*httptest.ResponseRecorder, string) {
	t.Helper()
	rec := r.do(t, "admin", "POST", "/admin/v1/deployment/package", `{"format":"`+format+`","label":"Pilot ring"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("package %s: %d %s", format, rec.Code, rec.Body)
	}
	return rec, rec.Header().Get("X-Sac-Deployment-Key-Id")
}

func unzip(t *testing.T, b []byte) map[string][]byte {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatalf("not a zip: %v", err)
	}
	out := map[string][]byte{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("%s: %v", f.Name, err)
		}
		out[f.Name] = data
	}
	return out
}

// checkTenantEnv asserts the tenant file holds exactly the tenant id, the device endpoint and the
// deployment key, CRLF, and returns
// the key.
func checkTenantEnv(t *testing.T, env []byte) string {
	t.Helper()
	text := string(env)
	if strings.Contains(strings.ReplaceAll(text, "\r\n", ""), "\n") || !strings.HasSuffix(text, "\r\n") {
		t.Fatalf("tenant.env is not CRLF throughout: %q", text)
	}
	values := map[string]string{}
	for _, line := range strings.Split(strings.TrimSuffix(text, "\r\n"), "\r\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("tenant.env line %q", line)
		}
		values[k] = v
	}
	if len(values) != 3 || values["SAC_TENANT_ID"] != tenantA || values["SAC_DEVICE_ENDPOINT"] != endpoint {
		t.Fatalf("tenant.env values = %v", values)
	}
	key := values["SAC_DEPLOYMENT_KEY"]
	if got, err := enrol.ParseDeploymentKey(key); err != nil || got != tenantA {
		t.Fatalf("SAC_DEPLOYMENT_KEY %q does not parse to the tenant: %v", key, err)
	}
	return key
}

func TestZipPackageContentsAndKey(t *testing.T) {
	r := newRig(t, writeRelease(t, nil))
	rec, keyID := download(t, r, "zip")
	if rec.Header().Get("Content-Type") != "application/zip" ||
		rec.Header().Get("Content-Disposition") != `attachment; filename="ShadowAICapture-0.2.0.zip"` {
		t.Fatalf("headers = %v", rec.Header())
	}
	files := unzip(t, rec.Body.Bytes())
	if len(files) != 3 || !bytes.Equal(files[deploy.MSIFileName], msiBytes) {
		t.Fatalf("zip entries = %d, msi equal = %v", len(files), bytes.Equal(files[deploy.MSIFileName], msiBytes))
	}
	key := checkTenantEnv(t, files[deploy.TenantEnvFileName])
	readme := string(files[deploy.ReadmeFileName])
	for _, want := range []string{"msiexec /i ShadowAICapture.msi /qn", deploy.UninstallCommand, `HKEY_LOCAL_MACHINE\SOFTWARE\ShadowAICapture, value Version`, "0.2.0"} {
		if !strings.Contains(readme, want) {
			t.Errorf("README lacks %q", want)
		}
	}
	if strings.Contains(readme, key) {
		t.Fatal("the README repeats the deployment key")
	}

	keys := r.store.DeploymentKeys(tenantA)
	if len(keys) != 1 || keys[0].KeyID != keyID || keys[0].KeyHash != enrol.HashDeploymentKey(key) ||
		keys[0].Label != "Pilot ring" || keys[0].CreatedBy != admin.Actor || keys[0].ExpiresAt != nil {
		t.Fatalf("stored key = %+v", keys)
	}
	audits := r.store.Audits()
	if len(audits) != 1 || audits[0].Action != "deployment_key.create" || audits[0].ActorType != store.ActorUser ||
		audits[0].ActorID != admin.Actor || audits[0].ObjectID != keyID {
		t.Fatalf("audits = %+v", audits)
	}
	if raw, _ := json.Marshal(audits[0].Detail); bytes.Contains(raw, []byte(key)) {
		t.Fatal("the audit row carries the key")
	}

	// Each download mints its own key.
	_, second := download(t, r, "zip")
	if second == keyID || len(r.store.DeploymentKeys(tenantA)) != 2 {
		t.Fatal("a second download reused the key")
	}
}

// TestIntuneWinRoundTrip opens the .intunewin as Intune and the device would: read Detection.xml,
// check the MAC over IV || ciphertext, decrypt AES-256-CBC, strip PKCS#7, check the digest and the
// size, and find the setup folder in the inner zip.
func TestIntuneWinRoundTrip(t *testing.T) {
	r := newRig(t, writeRelease(t, nil))
	rec, keyID := download(t, r, "intunewin")
	if rec.Header().Get("Content-Disposition") != `attachment; filename="ShadowAICapture-0.2.0.intunewin"` {
		t.Fatalf("Content-Disposition = %q", rec.Header().Get("Content-Disposition"))
	}
	pkg := rec.Body.Bytes()
	zr, err := zip.NewReader(bytes.NewReader(pkg), int64(len(pkg)))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
		if f.Method != zip.Store || f.Flags&0x8 != 0 {
			t.Errorf("%s: method %d flags %#x, want stored with sizes in the local header", f.Name, f.Method, f.Flags)
		}
	}
	if strings.Join(names, ",") != "IntuneWinPackage/Contents/IntunePackage.intunewin,IntuneWinPackage/Metadata/Detection.xml" {
		t.Fatalf("entries = %v", names)
	}
	outer := unzip(t, pkg)
	detection := outer["IntuneWinPackage/Metadata/Detection.xml"]
	var info deploy.ApplicationInfo
	if err := xml.Unmarshal(detection, &info); err != nil {
		t.Fatalf("Detection.xml: %v\n%s", err, detection)
	}
	for _, want := range []string{`xmlns:xsd="http://www.w3.org/2001/XMLSchema"`, `xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance"`, `ToolVersion="1.8.6.0"`} {
		if !bytes.Contains(detection, []byte(want)) {
			t.Errorf("Detection.xml lacks %s", want)
		}
	}
	if info.Name != "Shadow AI Capture" || info.FileName != "IntunePackage.intunewin" || info.SetupFile != "ShadowAICapture.msi" {
		t.Fatalf("ApplicationInfo = %+v", info)
	}
	ei := info.EncryptionInfo
	if ei.ProfileIdentifier != "ProfileVersion1" || ei.FileDigestAlgorithm != "SHA256" {
		t.Fatalf("EncryptionInfo = %+v", ei)
	}
	m := info.MsiInfo
	if m == nil || m.MsiProductCode != productCode || m.MsiProductVersion != "0.2.0" || m.MsiPackageCode != packageCode ||
		m.MsiUpgradeCode != upgradeCode || m.MsiExecutionContext != "System" || m.MsiRequiresLogon || m.MsiRequiresReboot ||
		!m.MsiIsMachineInstall || m.MsiIsUserInstall || !m.MsiIncludesServices || m.MsiIncludesODBCDataSource ||
		!m.MsiContainsSystemRegistryKeys || !m.MsiContainsSystemFolders || m.MsiPublisher != "Shadow AI Capture" {
		t.Fatalf("MsiInfo = %+v", m)
	}
	order := []string{"<Name>", "<UnencryptedContentSize>", "<FileName>", "<SetupFile>", "<EncryptionInfo>",
		"<EncryptionKey>", "<MacKey>", "<InitializationVector>", "<Mac>", "<ProfileIdentifier>", "<FileDigest>",
		"<FileDigestAlgorithm>", "<MsiInfo>", "<MsiProductCode>", "<MsiProductVersion>", "<MsiPackageCode>",
		"<MsiUpgradeCode>", "<MsiExecutionContext>", "<MsiRequiresLogon>", "<MsiRequiresReboot>",
		"<MsiIsMachineInstall>", "<MsiIsUserInstall>", "<MsiIncludesServices>", "<MsiIncludesODBCDataSource>",
		"<MsiContainsSystemRegistryKeys>", "<MsiContainsSystemFolders>", "<MsiPublisher>"}
	at := 0
	for _, el := range order {
		i := bytes.Index(detection[at:], []byte(el))
		if i < 0 {
			t.Fatalf("Detection.xml: %s missing or out of order", el)
		}
		at += i
	}

	b64 := func(s string, n int) []byte {
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil || (n > 0 && len(b) != n) {
			t.Fatalf("base64 %q: %d bytes, %v", s, len(b), err)
		}
		return b
	}
	key, macKey, iv, mac, digest := b64(ei.EncryptionKey, 32), b64(ei.MacKey, 32), b64(ei.InitializationVector, 16), b64(ei.Mac, 32), b64(ei.FileDigest, 32)
	enc := outer["IntuneWinPackage/Contents/IntunePackage.intunewin"]
	if !bytes.Equal(enc[:32], mac) || !bytes.Equal(enc[32:48], iv) {
		t.Fatal("the encrypted file does not start with Mac || IV")
	}
	h := hmac.New(sha256.New, macKey)
	h.Write(enc[32:])
	if !hmac.Equal(h.Sum(nil), mac) {
		t.Fatal("the MAC is not HMAC-SHA256(MacKey, IV || ciphertext)")
	}
	ct := enc[48:]
	if len(ct)%aes.BlockSize != 0 {
		t.Fatalf("ciphertext is %d bytes, not whole blocks", len(ct))
	}
	block, _ := aes.NewCipher(key)
	plain := make([]byte, len(ct))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(plain, ct)
	pad := int(plain[len(plain)-1])
	if pad < 1 || pad > aes.BlockSize || !bytes.Equal(plain[len(plain)-pad:], bytes.Repeat([]byte{byte(pad)}, pad)) {
		t.Fatalf("bad PKCS#7 padding %d", pad)
	}
	plain = plain[:len(plain)-pad]
	if sum := sha256.Sum256(plain); !bytes.Equal(sum[:], digest) {
		t.Fatal("FileDigest is not SHA-256 of the unencrypted inner zip")
	}
	if int64(len(plain)) != info.UnencryptedContentSize {
		t.Fatalf("UnencryptedContentSize %d, inner zip %d", info.UnencryptedContentSize, len(plain))
	}
	inner := unzip(t, plain)
	if len(inner) != 2 || !bytes.Equal(inner[deploy.MSIFileName], msiBytes) {
		t.Fatalf("inner zip entries = %d (want the MSI and the tenant file at the root)", len(inner))
	}
	envKey := checkTenantEnv(t, inner[deploy.TenantEnvFileName])
	if keys := r.store.DeploymentKeys(tenantA); len(keys) != 1 || keys[0].KeyID != keyID || keys[0].KeyHash != enrol.HashDeploymentKey(envKey) {
		t.Fatalf("stored key = %+v", keys)
	}

	// Fresh encryption keys per package.
	rec2, _ := download(t, r, "intunewin")
	var info2 deploy.ApplicationInfo
	_ = xml.Unmarshal(unzip(t, rec2.Body.Bytes())["IntuneWinPackage/Metadata/Detection.xml"], &info2)
	if info2.EncryptionInfo.EncryptionKey == ei.EncryptionKey || info2.EncryptionInfo.MacKey == ei.MacKey {
		t.Fatal("two packages share encryption keys")
	}
}

func TestPackageRefusals(t *testing.T) {
	r := newRig(t, writeRelease(t, nil))
	if rec := r.do(t, "admin", "POST", "/admin/v1/deployment/package", `{"format":"exe"}`); rec.Code != 400 || errorCode(t, rec) != apierr.CodeUnsupportedFormat {
		t.Errorf("bad format: %d %s", rec.Code, rec.Body)
	}
	if rec := r.do(t, "admin", "POST", "/admin/v1/deployment/package", `{"format":"zip","tenant":"x"}`); rec.Code != 400 {
		t.Errorf("unknown field: %d", rec.Code)
	}

	tampered := writeRelease(t, func(rel *deploy.Release) { rel.SHA256 = strings.Repeat("0", 64) })
	for name, dir := range map[string]string{"no release dir": "", "missing dir": filepath.Join(t.TempDir(), "absent"), "sha mismatch": tampered} {
		rr := newRig(t, dir)
		rec := rr.do(t, "admin", "POST", "/admin/v1/deployment/package", `{"format":"intunewin"}`)
		if rec.Code != http.StatusServiceUnavailable || errorCode(t, rec) != apierr.CodeReleaseUnavailable {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
		if len(rr.store.DeploymentKeys(tenantA)) != 0 {
			t.Errorf("%s: a key was stored for a package that was not built", name)
		}
	}
}

func TestSummary(t *testing.T) {
	r := newRig(t, writeRelease(t, nil))
	r.store.AddIdentityConnection(tenantA, store.IdentityConnection{Provider: "entra", Status: "active", EntraTenantID: "9b1c2d3e-4f50-4a6b-8c7d-0e1f2a3b4c5d"})
	r.store.SetScimSummary(tenantA, 42, 3, r.now.Add(-time.Hour))
	r.store.AddDevice(store.Device{TenantID: tenantA, DeviceID: "22222222-2222-4222-8222-222222222222", OS: "windows", EnrolledAt: r.now.Add(-2 * time.Hour)})
	_, keyID := download(t, r, "zip")
	if _, _, err := r.scim.Create(context.Background(), tenantA, "entra", "x"); err != nil {
		t.Fatal(err)
	}

	rec := r.do(t, "admin", "GET", "/admin/v1/deployment", "")
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	conn := got["connection"].(map[string]any)
	if conn["provider"] != "entra" || conn["status"] != "active" || got["device_verification"] != "none" {
		t.Fatalf("connection/verification = %v / %v", conn, got["device_verification"])
	}
	keys := got["deployment_keys"].([]any)
	k := keys[0].(map[string]any)
	if len(keys) != 1 || k["key_id"] != keyID || k["enrolment_count"] != float64(0) || k["created_by"] != admin.Actor {
		t.Fatalf("keys = %v", keys)
	}
	for _, field := range []string{"label", "created_at", "expires_at", "revoked_at", "last_used_at"} {
		if _, ok := k[field]; !ok {
			t.Errorf("key lacks %s", field)
		}
	}
	if _, ok := k["key_hash"]; ok {
		t.Fatal("the summary exposes the key hash")
	}
	scim := got["scim"].(map[string]any)
	if scim["users"] != float64(42) || scim["groups"] != float64(3) || scim["base_url"] != "https://app.example.com/scim/v2" || len(scim["tokens"].([]any)) != 1 {
		t.Fatalf("scim = %v", scim)
	}
	if dev := got["devices"].(map[string]any); dev["enrolled"] != float64(1) {
		t.Fatalf("devices = %v", dev)
	}
	if rel := got["release"].(map[string]any); rel["version"] != "0.2.0" || rel["product_code"] != productCode {
		t.Fatalf("release = %v", rel)
	}

	// A tenant with nothing yet reads as empty lists and nulls, not as an error.
	empty := newRig(t, "")
	rec = empty.do(t, "admin", "GET", "/admin/v1/deployment", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"connection":null`) || !strings.Contains(rec.Body.String(), `"deployment_keys":[]`) ||
		!strings.Contains(rec.Body.String(), `"release":null`) {
		t.Fatalf("empty summary: %d %s", rec.Code, rec.Body)
	}
}

func TestRevokeKey(t *testing.T) {
	r := newRig(t, writeRelease(t, nil))
	_, keyID := download(t, r, "zip")
	if rec := r.do(t, "admin", "POST", "/admin/v1/deployment/keys/"+keyID+"/revoke", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("revoke: %d %s", rec.Code, rec.Body)
	}
	if k := r.store.DeploymentKeys(tenantA)[0]; k.RevokedAt == nil || !k.RevokedAt.Equal(r.now) {
		t.Fatalf("key = %+v", k)
	}
	// A second revoke is not an error and does not move the first revocation or audit it twice.
	r.now = r.now.Add(time.Hour)
	if rec := r.do(t, "admin", "POST", "/admin/v1/deployment/keys/"+keyID+"/revoke", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("second revoke: %d", rec.Code)
	}
	revokes := 0
	for _, a := range r.store.Audits() {
		if a.Action == "deployment_key.revoke" {
			revokes++
			if a.ActorID != admin.Actor || a.ObjectID != keyID {
				t.Fatalf("revoke audit = %+v", a)
			}
		}
	}
	if revokes != 1 || !r.store.DeploymentKeys(tenantA)[0].RevokedAt.Equal(r.now.Add(-time.Hour)) {
		t.Fatalf("revoke audits = %d", revokes)
	}
	for _, id := range []string{"0f6a2b1c-3d4e-4f5a-8b6c-7d8e9f0a1b2c", "not-a-uuid"} {
		if rec := r.do(t, "admin", "POST", "/admin/v1/deployment/keys/"+id+"/revoke", ""); rec.Code != http.StatusNotFound {
			t.Errorf("unknown key %s: %d", id, rec.Code)
		}
	}
}

func TestVerificationSetting(t *testing.T) {
	r := newRig(t, "")
	put := func(body string) *httptest.ResponseRecorder {
		return r.do(t, "admin", "PUT", "/admin/v1/deployment/verification", body)
	}
	if rec := put(`{"device_verification":"intune"}`); rec.Code != http.StatusConflict || errorCode(t, rec) != apierr.CodeNoEntraConnection {
		t.Fatalf("intune with no entra connection: %d %s", rec.Code, rec.Body)
	}
	if rec := put(`{"device_verification":"maybe"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad value: %d", rec.Code)
	}
	r.store.AddIdentityConnection(tenantA, store.IdentityConnection{Provider: "entra", Status: "active", EntraTenantID: "9b1c2d3e-4f50-4a6b-8c7d-0e1f2a3b4c5d"})
	if rec := put(`{"device_verification":"intune"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("intune: %d %s", rec.Code, rec.Body)
	}
	v, _ := r.store.DeviceVerification(context.Background(), tenantA)
	if v.Mode != store.VerificationIntune {
		t.Fatalf("mode = %s", v.Mode)
	}
	audits := r.store.Audits()
	if len(audits) != 1 || audits[0].Action != "tenant.device_verification.set" || audits[0].ActorID != admin.Actor ||
		audits[0].Detail["device_verification"] != "intune" || audits[0].Detail["previous"] != "none" {
		t.Fatalf("audits = %+v", audits)
	}
}

func TestScimTokens(t *testing.T) {
	r := newRig(t, "")
	rec := r.do(t, "admin", "POST", "/admin/v1/scim/tokens", `{"label":"Entra provisioning"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var created map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	if created["token_id"] == "" || !strings.HasPrefix(created["token"], "sacscim_") || created["base_url"] != "https://app.example.com/scim/v2" {
		t.Fatalf("created = %v", created)
	}
	if rec := r.do(t, "admin", "POST", "/admin/v1/scim/tokens/"+created["token_id"]+"/revoke", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("revoke: %d %s", rec.Code, rec.Body)
	}
	if rec := r.do(t, "admin", "POST", "/admin/v1/scim/tokens/0f6a2b1c-3d4e-4f5a-8b6c-7d8e9f0a1b2c/revoke", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown token: %d", rec.Code)
	}
	var actions []string
	for _, a := range r.store.Audits() {
		actions = append(actions, a.Action)
		if a.ActorID != admin.Actor || a.ObjectID != created["token_id"] {
			t.Fatalf("audit = %+v", a)
		}
		if raw, _ := json.Marshal(a.Detail); bytes.Contains(raw, []byte(created["token"])) {
			t.Fatal("an audit row carries the SCIM token")
		}
	}
	if strings.Join(actions, ",") != "scim_token.create,scim_token.revoke" {
		t.Fatalf("audit actions = %v", actions)
	}

	none, err := deploy.NewHandler(storetest.New(), func(*http.Request) (deploy.Principal, error) { return admin, nil }, nil, deploy.Config{Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	none.Register(mux)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest("POST", "/admin/v1/scim/tokens", strings.NewReader(`{"label":"x"}`)))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("no SCIM store: %d", rr.Code)
	}
}

func TestReleaseValidation(t *testing.T) {
	for name, mutate := range map[string]func(*deploy.Release){
		"no version":       func(r *deploy.Release) { r.Version = "" },
		"bad product code": func(r *deploy.Release) { r.ProductCode = "not-a-guid" },
		"size mismatch":    func(r *deploy.Release) { r.Size = 1 },
		"no sha256":        func(r *deploy.Release) { r.SHA256 = "" },
	} {
		if _, _, err := deploy.LoadRelease(writeRelease(t, mutate), 0); !errors.Is(err, deploy.ErrReleaseUnavailable) {
			t.Errorf("%s: %v", name, err)
		}
	}
	rel, msi, err := deploy.LoadRelease(writeRelease(t, func(r *deploy.Release) { r.SHA256 = "sha256:" + strings.ToUpper(r.SHA256) }), 0)
	if err != nil || rel.ProductCode != productCode || !bytes.Equal(msi, msiBytes) {
		t.Fatalf("LoadRelease: %+v %v", rel, err)
	}
	if _, _, err := deploy.LoadRelease(writeRelease(t, nil), 100); !errors.Is(err, deploy.ErrReleaseUnavailable) {
		t.Errorf("over the size bound: %v", err)
	}
}
