//go:build windows

package winproxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"golang.org/x/sys/windows/registry"
)

const internetSettingsPath = `Software\Microsoft\Windows\CurrentVersion\Internet Settings`

// winRegistry binds a Registry to one user's Internet Settings key under HKEY_USERS.
type winRegistry struct{ key registry.Key }

func (w *winRegistry) GetString(name string) (string, bool, error) {
	v, _, err := w.key.GetStringValue(name)
	if err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return "", false, nil
		}
		return "", false, err
	}
	return v, true, nil
}

func (w *winRegistry) GetDWORD(name string) (uint32, bool, error) {
	v, _, err := w.key.GetIntegerValue(name)
	if err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return 0, false, nil
		}
		return 0, false, err
	}
	return uint32(v), true, nil
}

func (w *winRegistry) SetString(name, value string) error {
	return w.key.SetStringValue(name, value)
}

func (w *winRegistry) Delete(name string) error {
	if err := w.key.DeleteValue(name); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return err
	}
	return nil
}

func (w *winRegistry) Close() error { return w.key.Close() }

// openInternetSettings opens one user's Internet Settings key, creating it when absent. A logged-in
// user's hive is mounted under HKEY_USERS\<SID>, so the service (LocalSystem) can write it without
// impersonation. Only the Internet Settings key is ever opened, never created above it.
func openInternetSettings(sid string) (Registry, error) {
	k, _, err := registry.CreateKey(registry.USERS, sid+`\`+internetSettingsPath,
		registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return nil, fmt.Errorf("winproxy: opening Internet Settings for %s: %w", sid, err)
	}
	return &winRegistry{key: k}, nil
}

// signedInUsers lists the SIDs of logged-in users whose hives are loaded under HKEY_USERS. The
// service accounts and per-user class hives are not people.
func signedInUsers(context.Context) ([]string, error) {
	k, err := registry.OpenKey(registry.USERS, "", registry.ENUMERATE_SUB_KEYS)
	if err != nil {
		return nil, fmt.Errorf("winproxy: enumerating HKEY_USERS: %w", err)
	}
	defer k.Close()
	names, err := k.ReadSubKeyNames(-1)
	if err != nil {
		return nil, fmt.Errorf("winproxy: reading HKEY_USERS: %w", err)
	}
	var out []string
	for _, n := range names {
		if sid, ok := userSID(n); ok {
			out = append(out, sid)
		}
	}
	return out, nil
}

// userSID reports whether a HKEY_USERS subkey name is an interactive user account and returns the
// SID. Interactive users are local/domain accounts (S-1-5-21-…), Entra accounts (S-1-12-1-…) and
// Microsoft accounts (S-1-11-…). Service accounts and per-user class hives are excluded.
func userSID(name string) (string, bool) {
	if strings.HasSuffix(name, "_Classes") {
		return "", false
	}
	u := strings.ToUpper(name)
	switch {
	case strings.HasPrefix(u, "S-1-5-21-"), strings.HasPrefix(u, "S-1-12-1-"), strings.HasPrefix(u, "S-1-11-"):
		return name, true
	}
	return "", false
}

// maxPACBytes caps a fetched PAC body; a larger one is refused rather than inlined whole.
const maxPACBytes = 1 << 20

// fetchPAC fetches the body of an existing PAC URL so it can be inlined and delegated to.
func fetchPAC(ctx context.Context, u string) ([]byte, error) {
	if !strings.HasPrefix(strings.ToLower(u), "http://") && !strings.HasPrefix(strings.ToLower(u), "https://") {
		return nil, fmt.Errorf("winproxy: existing PAC URL %q is not http(s)", u)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("winproxy: existing PAC fetch returned %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxPACBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxPACBytes {
		return nil, fmt.Errorf("winproxy: existing PAC body exceeds %d bytes", maxPACBytes)
	}
	return body, nil
}
