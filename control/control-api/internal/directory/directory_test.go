package directory

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// sliceSource is a Source over a fixed list, for the sync tests.
type sliceSource struct {
	name  string
	users []User
	err   error
}

func (s sliceSource) Name() string { return s.name }
func (s sliceSource) List(context.Context) ([]User, error) {
	return s.users, s.err
}

// fakeStore mirrors the two writes the synchroniser makes, and is where "never delete" is asserted.
type fakeStore struct {
	identity string
	rows     map[string]Row
}

func newFakeStore(identity string) *fakeStore {
	return &fakeStore{identity: identity, rows: map[string]Row{}}
}

func (f *fakeStore) DeviceIdentity(context.Context, string) (string, error) {
	if f.identity == "" {
		return "", ErrUnknownTenant
	}
	return f.identity, nil
}

func (f *fakeStore) UpsertUser(_ context.Context, _ string, row Row) error {
	f.rows[row.UserRef] = row
	return nil
}

func (f *fakeStore) RetireMissing(_ context.Context, _ string, present []string) (int64, error) {
	keep := map[string]bool{}
	for _, ref := range present {
		keep[ref] = true
	}
	var changed int64
	for ref, row := range f.rows {
		if !keep[ref] && row.Status != StatusInactive {
			row.Status = StatusInactive
			f.rows[ref] = row
			changed++
		}
	}
	return changed, nil
}

func testCipher(t *testing.T) *Cipher {
	t.Helper()
	c, err := NewCipher([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	return c
}

func TestFileSourceReadsUsersAndKeepsUnmapped(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "directory.json")
	doc := map[string]any{"users": []map[string]any{
		{"user_ref": "u_a", "directory_id": "dir-a", "display_name": "A Person", "department": "Engineering", "population": "staff"},
		{"user_ref": "u_b", "directory_id": "dir-b", "display_name": "B Person"}, // no department: unmapped
		{"user_ref": "u_c", "directory_id": "dir-c", "account_enabled": false},
	}}
	raw, _ := json.Marshal(doc)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	users, err := NewFileSource(path).List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(users) != 3 {
		t.Fatalf("users = %d, want 3", len(users))
	}
	if users[0].Department != "Engineering" || users[0].DisplayName != "A Person" {
		t.Fatalf("first user = %+v", users[0])
	}
	if users[1].Department != "" {
		t.Fatalf("a user with no department parsed as %q, want the empty unmapped value", users[1].Department)
	}
	if users[2].Status != StatusInactive {
		t.Fatalf("account_enabled=false parsed as %q, want inactive", users[2].Status)
	}
	if got := NewFileSource(path).Name(); got != "file" {
		t.Fatalf("Name = %q", got)
	}
}

func TestSyncUpsertsRetiresAndCountsUnmapped(t *testing.T) {
	store := newFakeStore("clear")
	syncer := &Syncer{Source: sliceSource{name: "test", users: []User{
		{UserRef: "u_eng", DirectoryID: "dir-eng", Department: "Engineering", DisplayName: "Eng Person", Status: StatusActive},
		{UserRef: "u_unmapped", DirectoryID: "dir-un", DisplayName: "Unmapped Person", Status: StatusActive},
		{UserRef: "u_legal", DirectoryID: "dir-legal", Department: "Legal", Status: StatusActive},
	}}, Store: store, Cipher: testCipher(t)}
	at := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	syncer.Now = func() time.Time { return at }

	res, err := syncer.Sync(context.Background(), "tenant-1")
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if res.Read != 3 || res.Synced != 3 || res.Unmapped != 1 || res.Skipped != 0 || res.Retired != 0 {
		t.Fatalf("first sync = %+v, want read 3 synced 3 unmapped 1 retired 0", res)
	}
	if row := store.rows["u_unmapped"]; row.Department != nil {
		t.Fatalf("unmapped user department = %v, want NULL", *row.Department)
	}
	if row := store.rows["u_eng"]; row.Department == nil || *row.Department != "Engineering" {
		t.Fatalf("mapped user department = %v", row.Department)
	}
	if row := store.rows["u_eng"]; row.DisplayName == nil || *row.DisplayName != "Eng Person" {
		t.Fatalf("clear tenant display name = %v, want stored", row.DisplayName)
	}
	sealed := store.rows["u_eng"].DirectoryObjectIDEnc
	if len(sealed) == 0 {
		t.Fatal("directory identifier was not sealed")
	}
	if got, err := testCipher(t).Open("tenant-1", sealed); err != nil || got != "dir-eng" {
		t.Fatalf("Open(sealed) = %q, %v; want dir-eng", got, err)
	}

	// A second run with the same source is idempotent and retires nothing.
	again, err := syncer.Sync(context.Background(), "tenant-1")
	if err != nil {
		t.Fatalf("second Sync: %v", err)
	}
	if again.Retired != 0 || again.Synced != 3 {
		t.Fatalf("second sync = %+v, want retired 0", again)
	}

	// A run that no longer returns Legal retires it without deleting or rewriting it.
	syncer.Source = sliceSource{name: "test", users: []User{
		{UserRef: "u_eng", DirectoryID: "dir-eng", Department: "Engineering", Status: StatusActive},
		{UserRef: "u_unmapped", DirectoryID: "dir-un", Status: StatusActive},
	}}
	third, err := syncer.Sync(context.Background(), "tenant-1")
	if err != nil {
		t.Fatalf("third Sync: %v", err)
	}
	if third.Retired != 1 {
		t.Fatalf("third sync retired %d, want 1", third.Retired)
	}
	legal := store.rows["u_legal"]
	if legal.Status != StatusInactive {
		t.Fatalf("departed user status = %q, want inactive", legal.Status)
	}
	if legal.Department == nil || *legal.Department != "Legal" {
		t.Fatalf("retirement rewrote the department = %v; history must stay attributable", legal.Department)
	}
	if len(store.rows) != 3 {
		t.Fatalf("retirement deleted a row: %d rows", len(store.rows))
	}
}

func TestSyncCountsSkippedUsersWithNoMapping(t *testing.T) {
	store := newFakeStore("clear")
	syncer := &Syncer{Source: sliceSource{name: "test", users: []User{
		{UserRef: "", DirectoryID: "dir-x", Department: "Engineering"},
		{UserRef: "u_known", DirectoryID: "dir-k", Department: "Engineering"},
	}}, Store: store, Cipher: testCipher(t)}

	res, err := syncer.Sync(context.Background(), "tenant-1")
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if res.Skipped != 1 || res.Synced != 1 || res.Read != 2 {
		t.Fatalf("sync = %+v, want read 2 synced 1 skipped 1", res)
	}
	if _, ok := store.rows[""]; ok {
		t.Fatal("a user with no mapping attribute was written under a blank key")
	}
}

func TestSyncRefusesAnEmptyRead(t *testing.T) {
	store := newFakeStore("clear")
	store.rows["u_old"] = Row{UserRef: "u_old", Status: StatusActive}
	syncer := &Syncer{Source: sliceSource{name: "test"}, Store: store, Cipher: testCipher(t)}

	if _, err := syncer.Sync(context.Background(), "tenant-1"); err == nil {
		t.Fatal("an empty source read was accepted; it would retire every row")
	}
	if store.rows["u_old"].Status != StatusActive {
		t.Fatal("the refusal still mutated a row")
	}
}

func TestSyncStoresNoDisplayNameForAHashedTenant(t *testing.T) {
	store := newFakeStore("hashed")
	syncer := &Syncer{Source: sliceSource{name: "test", users: []User{
		{UserRef: "u_a", DirectoryID: "dir-a", DisplayName: "Clear Name", Department: "Engineering"},
	}}, Store: store, Cipher: testCipher(t)}

	if _, err := syncer.Sync(context.Background(), "tenant-1"); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if row := store.rows["u_a"]; row.DisplayName != nil {
		t.Fatalf("a hashed tenant stored a clear display name: %v", *row.DisplayName)
	}
	if len(store.rows["u_a"].DirectoryObjectIDEnc) == 0 {
		t.Fatal("the directory identifier was not sealed for a hashed tenant")
	}
}

func TestCipherRoundTripsAndIsTenantScoped(t *testing.T) {
	c := testCipher(t)
	sealed, err := c.Seal("tenant-1", "dir-abc")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if got, err := c.Open("tenant-1", sealed); err != nil || got != "dir-abc" {
		t.Fatalf("Open = %q, %v", got, err)
	}
	if _, err := c.Open("tenant-2", sealed); err == nil {
		t.Fatal("a sealed identifier opened under another tenant's key")
	}
	if empty, err := c.Seal("tenant-1", ""); err != nil || empty != nil {
		t.Fatalf("Seal(\"\") = %v, %v; want nil", empty, err)
	}
	if _, err := NewCipher([]byte("short")); err == nil {
		t.Fatal("a short master key was accepted")
	}
}

func TestValidateRejectsDuplicateReferences(t *testing.T) {
	err := validate([]User{{UserRef: "u_a"}, {UserRef: "u_a"}})
	if err == nil {
		t.Fatal("a duplicated user_ref was accepted")
	}
}

func TestPgTextArrayEscapes(t *testing.T) {
	got := pgTextArray([]string{`u_a`, `u,comma`, `u"quote`, `u\back`})
	want := `{"u_a","u,comma","u\"quote","u\\back"}`
	if got != want {
		t.Fatalf("pgTextArray = %s, want %s", got, want)
	}
	if pgTextArray(nil) != "{}" {
		t.Fatalf("pgTextArray(nil) = %s, want {}", pgTextArray(nil))
	}
}

func TestGraphSourcePagesAndMapsTheConfiguredAttribute(t *testing.T) {
	var server *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/contoso.example/oauth2/v2.0/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse token form: %v", err)
		}
		if r.Form.Get("grant_type") != "client_credentials" || r.Form.Get("client_secret") != "sekret" {
			t.Errorf("token request = %v", r.Form)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	})
	mux.HandleFunc("/users", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("authorization"); got != "Bearer tok" {
			t.Errorf("authorization = %q, want Bearer tok", got)
		}
		if got := r.URL.Query().Get("$select"); !containsAll(got, "id", "displayName", "department", "employeeId") {
			t.Errorf("$select = %q, missing a configured attribute", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"value": []map[string]any{
				{"id": "obj-1", "displayName": "One Person", "department": "Engineering", "employeeId": "e-1", "accountEnabled": true},
				{"id": "obj-2", "displayName": "Two Person", "department": "Legal", "employeeId": "e-2", "accountEnabled": false},
			},
			"@odata.nextLink": server.URL + "/users-page-2",
		})
	})
	mux.HandleFunc("/users-page-2", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"value": []map[string]any{
				{"id": "obj-3", "displayName": "Three Person", "employeeId": "e-3", "accountEnabled": true},
			},
		})
	})
	server = httptest.NewServer(mux)
	defer server.Close()

	source := &GraphSource{
		TenantID: "contoso.example", ClientID: "app", ClientSecret: "sekret",
		UserRefAttribute: "employeeId", PopulationAttribute: "department",
		LoginBaseURL: server.URL, GraphBaseURL: server.URL, HTTPClient: server.Client(),
	}
	users, err := source.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(users) != 3 {
		t.Fatalf("users = %d, want 3 across two pages", len(users))
	}
	if users[0].UserRef != "e-1" || users[0].Department != "Engineering" || users[0].DisplayName != "One Person" {
		t.Fatalf("first user = %+v", users[0])
	}
	if users[1].Status != StatusInactive {
		t.Fatalf("disabled user status = %q, want inactive", users[1].Status)
	}
	if users[2].UserRef != "e-3" || users[2].Department != "" {
		t.Fatalf("third user = %+v", users[2])
	}
	if got := source.Name(); got != "entra" {
		t.Fatalf("Name = %q", got)
	}
}

func TestGraphSourceRefusesAMissingSecret(t *testing.T) {
	source := &GraphSource{TenantID: "t", ClientID: "c"}
	if _, err := source.List(context.Background()); err == nil {
		t.Fatal("a provider with no client secret started anyway")
	}
}

func containsAll(haystack string, needles ...string) bool {
	for _, n := range needles {
		if !strings.Contains(haystack, n) {
			return false
		}
	}
	return true
}

// TestSyncerNeedsItsCollaborators is the guard against a nil collaborator reaching a nil deref at
// the first write, which would be a crash in a scheduled job rather than an error it can report.
func TestSyncerNeedsItsCollaborators(t *testing.T) {
	if _, err := (&Syncer{}).Sync(context.Background(), "t"); err == nil {
		t.Fatal("an unconfigured syncer ran")
	}
}
