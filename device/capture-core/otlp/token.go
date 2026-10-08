package otlp

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"github.com/shadow-ai-capture/device/capture-core/state"
)

// TokenFile is the token's file name in the state directory.
const TokenFile = "otlp.token"

const tokenBytes = 32

// loadToken returns the hex token kept in path, creating it when the file is missing or does not
// hold one. A token file that is not protected is written again with the state directory's
// protection.
func loadToken(path string) (string, error) {
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		tok := strings.TrimSpace(string(b))
		if validToken(tok) {
			if state.CheckFile(path) != nil {
				if err := state.WriteFile(path, []byte(tok)); err != nil {
					return "", fmt.Errorf("otlp: protecting %s: %w", path, err)
				}
			}
			return tok, nil
		}
	case !errors.Is(err, fs.ErrNotExist):
		return "", fmt.Errorf("otlp: reading %s: %w", path, err)
	}
	raw := make([]byte, tokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(raw)
	if err := state.WriteFile(path, []byte(tok)); err != nil {
		return "", fmt.Errorf("otlp: writing %s: %w", path, err)
	}
	return tok, nil
}

func validToken(tok string) bool {
	raw, err := hex.DecodeString(tok)
	return err == nil && len(raw) == tokenBytes
}

// authorized reports whether an Authorization value is "Bearer <token>". The scheme is matched
// without regard to case, as HTTP authentication schemes are.
func authorized(header, token string) bool {
	scheme, cred, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(strings.TrimSpace(cred)), []byte(token)) == 1
}
