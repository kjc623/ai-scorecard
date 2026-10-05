package identity

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestParseRS256PinsTheAlgorithm(t *testing.T) {
	seg := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	payload := seg(`{"iss":"x"}`)
	for name, header := range map[string]string{
		"none":     `{"alg":"none"}`,
		"hs256":    `{"alg":"HS256","kid":"k1"}`,
		"es256":    `{"alg":"ES256","kid":"k1"}`,
		"ps256":    `{"alg":"PS256","kid":"k1"}`,
		"no alg":   `{"kid":"k1"}`,
		"critical": `{"alg":"RS256","kid":"k1","crit":["exp"]}`,
	} {
		if _, err := parseRS256(seg(header) + "." + payload + "." + seg("sig")); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
	if _, err := parseRS256(seg(`{"alg":"RS256","kid":"k1"}`) + "." + payload + "." + seg("sig")); err != nil {
		t.Fatalf("RS256: %v", err)
	}
}

func TestInviteTokenFormat(t *testing.T) {
	tenant := "AAAAAAAA-0000-4000-8000-000000000001"
	tok, err := MintInviteToken(strings.ToLower(tenant))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tok, InvitePrefix+strings.ToLower(tenant)+".") {
		t.Fatalf("token = %s", tok)
	}
	got, err := ParseInviteToken(tok)
	if err != nil || got != strings.ToLower(tenant) {
		t.Fatalf("parse = %q, %v", got, err)
	}
	if h := HashToken(tok); !strings.HasPrefix(h, "sha256:") || len(h) != 71 {
		t.Fatalf("hash = %s", h)
	}
	for _, bad := range []string{"", "sac1." + strings.ToLower(tenant) + ".abc", InvitePrefix + "not-a-uuid.abc", tok[:len(tok)-2], tok + "x"} {
		if _, err := ParseInviteToken(bad); err == nil {
			t.Fatalf("%q parsed", bad)
		}
	}
	if _, err := MintInviteToken("nope"); err == nil {
		t.Fatal("minted for a non-uuid tenant")
	}
}

func TestResolveRoles(t *testing.T) {
	cases := []struct {
		idp     []string
		roleMap map[string]string
		grants  []string
		want    string
	}{
		{[]string{"analyst", "Global Admin"}, nil, nil, "analyst"},
		{[]string{"g1"}, map[string]string{"g1": "admin", "g2": "viewer"}, nil, "admin"},
		{[]string{"viewer"}, map[string]string{"g1": "admin"}, nil, ""},
		{nil, nil, []string{"content_reader", "bogus"}, "content_reader"},
		{[]string{"admin", "viewer"}, nil, []string{"viewer"}, "viewer,admin"},
	}
	for i, c := range cases {
		if got := strings.Join(resolveRoles(c.idp, c.roleMap, c.grants), ","); got != c.want {
			t.Fatalf("case %d: %q, want %q", i, got, c.want)
		}
	}
}
