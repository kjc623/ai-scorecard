package hostinfo

import (
	"errors"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/protocol"
)

// The vectors are written by hand from real GUIDs (an Entra account's SID is
// S-1-12-1 followed by the object id's sixteen bytes as four little-endian sub-authorities).
// The second is the published example of the conversion (an Entra object id and the SID Windows
// gives the account), so the rule is checked against Windows' own behaviour, not only this code.
func TestObjectIDFromSID(t *testing.T) {
	for _, tc := range []struct{ sid, want string }{
		// 6F9619FF-8B86-D011-B42D-00C04FC964FF: Data1 0x6F9619FF = 1872108031; Data3:Data2 =
		// 0xD0118B86 = 3490810758; Data4 B4 2D 00 C0 | 4F C9 64 FF read little-endian =
		// 0xC0002DB4 = 3221237172 and 0xFF64C94F = 4284795215.
		{"S-1-12-1-1872108031-3490810758-3221237172-4284795215", "6f9619ff-8b86-d011-b42d-00c04fc964ff"},
		{"S-1-12-1-1943430372-1249052806-2496021943-3034400218", "73d664e4-0886-4a73-b745-c694da45ddb4"},
	} {
		got, ok := ObjectIDFromSID(tc.sid)
		if !ok || got != tc.want {
			t.Errorf("ObjectIDFromSID(%s) = %q, %v; want %s", tc.sid, got, ok, tc.want)
		}
	}
	for _, sid := range []string{"S-1-5-21-1004336348-1177238915-682003330-1001", "S-1-12-1-1-2-3", "S-1-12-1-a-b-c-d", "S-1-5-18"} {
		if got, ok := ObjectIDFromSID(sid); ok {
			t.Errorf("ObjectIDFromSID(%s) = %q; a SID that is not an Entra account carries no object id", sid, got)
		}
	}
}

// The precedence against the DeriveUserRef vectors protocol pins (computed outside
// Go), so the device meets the directory's derivation, not just its own.
func TestUserRefPrecedence(t *testing.T) {
	key, err := protocol.DecodeUserRefKey("AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8")
	if err != nil {
		t.Fatal(err)
	}
	entraSID := "S-1-12-1-1872108031-3490810758-3221237172-4284795215"
	oid, _ := ObjectIDFromSID(entraSID)
	for _, tc := range []struct {
		name string
		user User
		kind protocol.UserRefKind
		want string
	}{
		{"upn wins over oid and account", User{SID: entraSID, ObjectID: oid, Account: `AzureAD\AdaLovelace`, UPN: "Ada.Lovelace@Contoso.com"}, protocol.UserRefUPN, "u_ed0bf663359a09dab5105ed60a56e835"},
		{"oid when no upn resolved", User{SID: entraSID, ObjectID: oid, Account: `AzureAD\AdaLovelace`}, protocol.UserRefOID, "u_a504db1d5d49e461d150819d7d9286e8"},
		{"account when neither", User{SID: "S-1-5-21-1-2-3-1001", Account: `DESKTOP-01\Kyle`}, protocol.UserRefAccount, "u_07eff34d88f27c2bd74a0274623831b7"},
	} {
		ref, kind, err := tc.user.Ref(key)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if ref != tc.want || kind != tc.kind {
			t.Errorf("%s: Ref = %s (%s), want %s (%s)", tc.name, ref, kind, tc.want, tc.kind)
		}
	}
	if _, _, err := (User{}).Ref(key); err == nil {
		t.Error("a user with nothing to derive from produced a reference")
	}
}

func TestDisplayNamePrefersTheUPN(t *testing.T) {
	if n := (User{Account: `CONTOSO\ada`, UPN: "ada@contoso.com"}).DisplayName(); n != "ada@contoso.com" {
		t.Errorf("DisplayName = %q, want the UPN", n)
	}
	if n := (User{Account: `DESKTOP-01\kyle`}).DisplayName(); n != `DESKTOP-01\kyle` {
		t.Errorf("DisplayName = %q, want the account", n)
	}
}

func TestUPNFromIdentityStore(t *testing.T) {
	sid := "S-1-12-1-1872108031-3490810758-3221237172-4284795215"
	reg := newFakeRegistry()
	reg.strs[identityStoreKey+`\`+sid+`\IdentityCache\`+sid+"|UserName"] = "ada.lovelace@contoso.com"
	if upn := UPNFromIdentityStore(reg, sid); upn != "ada.lovelace@contoso.com" {
		t.Fatalf("UPNFromIdentityStore = %q", upn)
	}
	reg.strs[identityStoreKey+`\`+sid+`\IdentityCache\`+sid+"|UserName"] = `AzureAD\AdaLovelace`
	if upn := UPNFromIdentityStore(reg, sid); upn != "" {
		t.Fatalf("UPNFromIdentityStore = %q; a value that is not a principal name is not a UPN", upn)
	}
}

func TestResolverConsoleUser(t *testing.T) {
	sid := "S-1-12-1-1872108031-3490810758-3221237172-4284795215"
	lookups := 0
	r := NewResolver(UserSources{
		Console: func() (User, error) { return User{SID: sid, Account: `AzureAD\AdaLovelace`}, nil },
		UPN: func(User) (string, error) {
			lookups++
			return "ada.lovelace@contoso.com", nil
		},
	}, nil)
	for i := 0; i < 3; i++ {
		u, err := r.Current()
		if err != nil {
			t.Fatal(err)
		}
		if u.Source != "console" || u.UPN != "ada.lovelace@contoso.com" || u.ObjectID != "6f9619ff-8b86-d011-b42d-00c04fc964ff" {
			t.Fatalf("Current = %+v", u)
		}
	}
	if lookups != 1 {
		t.Fatalf("the directory was asked %d times for one account; the answer is kept per SID", lookups)
	}
}

func TestResolverRetriesAFailedDirectoryLookupOnlyAfterAWhile(t *testing.T) {
	now := time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)
	lookups := 0
	r := NewResolver(UserSources{
		Console: func() (User, error) { return User{SID: "S-1-5-21-1-2-3-1104", Account: `CONTOSO\ada`}, nil },
		UPN: func(User) (string, error) {
			lookups++
			if lookups == 1 {
				return "", errors.New("the domain controller is unreachable")
			}
			return "ada@contoso.com", nil
		},
	}, func() time.Time { return now })

	if u, _ := r.Current(); u.UPN != "" {
		t.Fatalf("UPN = %q after a failed lookup", u.UPN)
	}
	if u, _ := r.Current(); u.UPN != "" || lookups != 1 {
		t.Fatalf("a failed lookup was retried immediately (%d lookups)", lookups)
	}
	now = now.Add(11 * time.Minute)
	if u, _ := r.Current(); u.UPN != "ada@contoso.com" {
		t.Fatalf("UPN = %q after the retry interval", u.UPN)
	}
}

func TestResolverFallsBackToThisProcessUserOnlyWhenItMayNotAsk(t *testing.T) {
	process := func() (User, error) { return User{SID: "S-1-5-21-1-2-3-1001", Account: `DESKTOP-01\kyle`}, nil }

	r := NewResolver(UserSources{Console: func() (User, error) { return User{}, ErrNotPermitted }, Process: process}, nil)
	u, err := r.Current()
	if err != nil || u.Source != "process" || u.Account != `DESKTOP-01\kyle` {
		t.Fatalf("a console run that may not query the session = %+v, %v; want its own user", u, err)
	}

	r = NewResolver(UserSources{Console: func() (User, error) { return User{}, ErrNoConsoleUser }, Process: process}, nil)
	if _, err := r.Current(); !errors.Is(err, ErrNoConsoleUser) {
		t.Fatalf("with nobody at the console err = %v; the service's own account must not stand in", err)
	}

	system := func() (User, error) { return User{SID: "S-1-5-18", Account: `NT AUTHORITY\SYSTEM`}, nil }
	r = NewResolver(UserSources{Console: func() (User, error) { return User{}, ErrUnsupported }, Process: system}, nil)
	if _, err := r.Current(); !errors.Is(err, ErrNoConsoleUser) {
		t.Fatalf("LocalSystem was reported as the person (err = %v)", err)
	}
}
