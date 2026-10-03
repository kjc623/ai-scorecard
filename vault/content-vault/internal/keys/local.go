package keys

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"
)

// LocalKeyWrapper is the software key store: AES-256-GCM key wrapping with the KEKs held in this
// process's memory (and, optionally, a 0600 file for local development).
//
// It is the implementation the tests and local development exercise, and TOOLCHAIN-DECISION.md
// records why it exists: no cloud KMS is reachable from the offline build host. What it is *not*
// is a deployment answer, and the package README says so in the same sentence as the instruction
// to use it locally:
//
//   - the KEK lives in the vault's own process, so it does not give a customer-held tenant what
//     mode 3 promises (§6.2). A deployment claiming mode 2 or mode 3 with this backend would be
//     claiming a control that is not there;
//   - there is no HSM and no separate administrative domain, so the wrap operation's protection
//     is whatever protects the process's memory;
//   - the recovery window of A4 does not exist: Destroy is immediate, and a receipt can honestly
//     say so.
//
// Everything else about the hierarchy is real: per-tenant KEK with versions, per-object DEK
// wrapped under it, AAD binding to the row, rotation that re-wraps without re-encrypting, and a
// Destroy that makes every wrapped key unrecoverable.
type LocalKeyWrapper struct {
	mu        sync.RWMutex
	kek       map[string]map[string][]byte // kek_id -> version -> 32-byte key
	current   map[string]string            // kek_id -> current version
	destroyed map[string]string            // kek_id -> reason
	path      string                       // optional persistence (local development)
	rnd       io.Reader
	now       func() time.Time
}

// NewLocal returns an in-memory wrapper. Keys do not survive the process; that is deliberate for
// tests, and OpenLocal is the development variant that does.
func NewLocal() *LocalKeyWrapper {
	return &LocalKeyWrapper{
		kek:       map[string]map[string][]byte{},
		current:   map[string]string{},
		destroyed: map[string]string{},
		rnd:       rand.Reader,
		now:       time.Now,
	}
}

// NewLocalWithReader returns an in-memory wrapper whose key material comes from r. Tests use it to
// make key generation deterministic; production never does.
func NewLocalWithReader(r io.Reader) *LocalKeyWrapper {
	w := NewLocal()
	w.rnd = r
	return w
}

// Kind implements KeyWrapper.
func (l *LocalKeyWrapper) Kind() KeyKind { return KindLocalSoftware }

// OpenLocal loads a key file written by Persist, creating it if it does not exist. The file is
// 0600 and holds base64 key material: acceptable for local development, and the reason this
// backend is not a deployment answer.
func OpenLocal(path string) (*LocalKeyWrapper, error) {
	w := NewLocal()
	w.path = path
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return w, nil
	}
	if err != nil {
		return nil, fmt.Errorf("keys: reading %s: %w", path, err)
	}
	var doc localFile
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("keys: parsing %s: %w", path, err)
	}
	for id, versions := range doc.KEK {
		w.kek[id] = map[string][]byte{}
		for v, b64 := range versions {
			raw, err := base64.StdEncoding.DecodeString(b64)
			if err != nil {
				return nil, fmt.Errorf("keys: %s: key %s version %s is not base64: %w", path, id, v, err)
			}
			if len(raw) != KEKSize {
				return nil, fmt.Errorf("keys: %s: key %s version %s is %d bytes, want %d", path, id, v, len(raw), KEKSize)
			}
			w.kek[id][v] = raw
		}
	}
	w.current = doc.Current
	for id, reason := range doc.Destroyed {
		w.destroyed[id] = reason
	}
	return w, nil
}

type localFile struct {
	KEK       map[string]map[string]string `json:"kek"`
	Current   map[string]string            `json:"current"`
	Destroyed map[string]string            `json:"destroyed"`
}

// Persist writes the key file atomically. A no-op for an in-memory wrapper.
func (l *LocalKeyWrapper) Persist() error {
	l.mu.RLock()
	path := l.path
	doc := localFile{KEK: map[string]map[string]string{}, Current: map[string]string{}, Destroyed: map[string]string{}}
	for id, versions := range l.kek {
		doc.KEK[id] = map[string]string{}
		for v, raw := range versions {
			doc.KEK[id][v] = base64.StdEncoding.EncodeToString(raw)
		}
	}
	for id, v := range l.current {
		doc.Current[id] = v
	}
	now := l.now
	for id, reason := range l.destroyed {
		doc.Destroyed[id] = reason
	}
	l.mu.RUnlock()
	if path == "" {
		return nil
	}
	_ = now
	body, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(body, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// EnsureKEK creates version 1 for kekID if the store has never held it, and returns the current
// version. It is idempotent, so a caller may call it on every write.
func (l *LocalKeyWrapper) EnsureKEK(kekID string) (string, error) {
	if kekID == "" {
		return "", errors.New("keys: an empty kek_id cannot be created")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, gone := l.destroyed[kekID]; gone {
		return "", fmt.Errorf("%w: %s", ErrKeyDestroyed, kekID)
	}
	if v, ok := l.current[kekID]; ok {
		return v, nil
	}
	raw, err := GenerateKEK(l.rnd)
	if err != nil {
		return "", err
	}
	l.kek[kekID] = map[string][]byte{"1": raw}
	l.current[kekID] = "1"
	return "1", nil
}

// CurrentVersion implements KeyWrapper.
func (l *LocalKeyWrapper) CurrentVersion(_ context.Context, kekID string) (string, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if reason, gone := l.destroyed[kekID]; gone {
		return "", fmt.Errorf("%w: %s (%s)", ErrKeyDestroyed, kekID, reason)
	}
	v, ok := l.current[kekID]
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrUnknownKEK, kekID)
	}
	return v, nil
}

// Versions returns the versions this store holds for kekID, sorted oldest first. It exists for
// tests and for the rotation report; it is not on the KeyWrapper interface, because the vault has
// no operational need to enumerate key versions.
func (l *LocalKeyWrapper) Versions(kekID string) []string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]string, 0, len(l.kek[kekID]))
	for v := range l.kek[kekID] {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return versionLess(out[i], out[j]) })
	return out
}

// DestroyedReason reports whether a KEK has been destroyed and why.
func (l *LocalKeyWrapper) DestroyedReason(kekID string) (string, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	r, ok := l.destroyed[kekID]
	return r, ok
}

// Wrap implements KeyWrapper: AES-256-GCM under the current KEK version, with the row's identity
// as additional authenticated data.
func (l *LocalKeyWrapper) Wrap(_ context.Context, kekID string, aad AAD, dek []byte) (Wrapped, error) {
	if len(dek) != DEKSize {
		return Wrapped{}, fmt.Errorf("keys: data key is %d bytes, want %d", len(dek), DEKSize)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if reason, gone := l.destroyed[kekID]; gone {
		return Wrapped{}, fmt.Errorf("%w: %s (%s)", ErrKeyDestroyed, kekID, reason)
	}
	version, ok := l.current[kekID]
	if !ok {
		return Wrapped{}, fmt.Errorf("%w: %s", ErrUnknownKEK, kekID)
	}
	kek := l.kek[kekID][version]
	// The version on the returned value is authoritative: the caller records what it was given,
	// not what it hoped.
	aad.KEKID = kekID
	aad.KEKVersion = version
	if !aad.Complete() {
		return Wrapped{}, fmt.Errorf("keys: refusing to wrap without a complete AAD (tenant, object, key, version)")
	}
	sealed, err := sealGCM(kek, aad.Canonical(), dek, l.rnd)
	if err != nil {
		return Wrapped{}, err
	}
	return Wrapped{Bytes: sealed, KEKID: kekID, KEKVersion: version}, nil
}

// Unwrap implements KeyWrapper.
func (l *LocalKeyWrapper) Unwrap(_ context.Context, aad AAD, wrapped []byte) ([]byte, error) {
	if !aad.Complete() {
		return nil, fmt.Errorf("keys: refusing to unwrap without a complete AAD (tenant, object, key, version)")
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	if reason, gone := l.destroyed[aad.KEKID]; gone {
		// The terminal fact: the content is gone because its key is gone (§6.4).
		return nil, fmt.Errorf("%w: %s (%s)", ErrKeyDestroyed, aad.KEKID, reason)
	}
	versions, ok := l.kek[aad.KEKID]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnknownKEK, aad.KEKID)
	}
	kek, ok := versions[aad.KEKVersion]
	if !ok {
		return nil, fmt.Errorf("%w: %s version %s", ErrUnknownVersion, aad.KEKID, aad.KEKVersion)
	}
	dek, err := openGCM(kek, aad.Canonical(), wrapped)
	if err != nil {
		return nil, err
	}
	return dek, nil
}

// NewVersion implements KeyWrapper: it creates the next version and makes it current. Existing
// wrapped keys stay sealed under the versions they name, which is what makes rotation a re-wrap
// and never a re-encrypt.
func (l *LocalKeyWrapper) NewVersion(_ context.Context, kekID string) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if reason, gone := l.destroyed[kekID]; gone {
		return "", fmt.Errorf("%w: %s (%s)", ErrKeyDestroyed, kekID, reason)
	}
	versions, ok := l.kek[kekID]
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrUnknownKEK, kekID)
	}
	next := 1
	for v := range versions {
		if n, err := strconv.Atoi(v); err == nil && n >= next {
			next = n + 1
		}
	}
	raw, err := GenerateKEK(l.rnd)
	if err != nil {
		return "", err
	}
	nv := strconv.Itoa(next)
	versions[nv] = raw
	l.current[kekID] = nv
	return nv, nil
}

// Destroy implements KeyWrapper: every version goes, so every wrapped DEK of that tenant becomes
// unopenable. Nothing here is recoverable, and the receipt must say so rather than claiming a
// recovery window this backend does not have (A4).
func (l *LocalKeyWrapper) Destroy(_ context.Context, kekID, reason string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.kek[kekID]; !ok {
		if _, gone := l.destroyed[kekID]; gone {
			return nil // already destroyed: idempotent, and the receipt still stands
		}
		return fmt.Errorf("%w: %s", ErrUnknownKEK, kekID)
	}
	delete(l.kek, kekID)
	delete(l.current, kekID)
	l.destroyed[kekID] = reason
	return nil
}

func sealGCM(key, aad, plaintext []byte, rnd io.Reader) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rnd, nonce); err != nil {
		return nil, fmt.Errorf("keys: reading a nonce: %w", err)
	}
	out := make([]byte, 0, len(nonce)+len(plaintext)+gcm.Overhead())
	out = append(out, nonce...)
	return gcm.Seal(out, nonce, plaintext, aad), nil
}

func openGCM(key, aad, sealed []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(sealed) < gcm.NonceSize()+gcm.Overhead() {
		return nil, fmt.Errorf("%w: %d bytes is shorter than a nonce plus a tag", ErrMalformedWrapped, len(sealed))
	}
	nonce, body := sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():]
	plaintext, err := gcm.Open(nil, nonce, body, aad)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrAuthentication, err)
	}
	return plaintext, nil
}

func versionLess(a, b string) bool {
	na, erra := strconv.Atoi(a)
	nb, errb := strconv.Atoi(b)
	if erra == nil && errb == nil {
		return na < nb
	}
	return a < b
}

// KeyFilePath is where the local development wrapper keeps its keys. Local development only; the
// README states plainly that a deployment must not use this backend.
func KeyFilePath(dir string) string { return filepath.Join(dir, "local-kek.json") }
