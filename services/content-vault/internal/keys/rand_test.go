package keys_test

import (
	"crypto/rand"
	"io"
)

// newRand returns the module's random source for tests. It exists as a function so a test that
// wants determinism can be changed in one place, and because keys.GenerateDEK takes an interface
// rather than crypto/rand directly.
func newRand(t interface{ Fatal(...any) }) io.Reader { return rand.Reader }
