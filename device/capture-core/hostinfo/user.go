package hostinfo

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/shadow-ai-capture/device/protocol"
)

// The user lookups fail in three distinguishable ways, and the caller acts differently on each.
var (
	// ErrNoConsoleUser: nobody is signed in at the console. The device's traffic is unattributed;
	// the service account is never used in a person's place.
	ErrNoConsoleUser = errors.New("hostinfo: no user is signed in at the console")
	// ErrNotPermitted: this process may not ask (WTSQueryUserToken needs the service's privilege).
	// A console run falls back to its own user, which is the person who started it.
	ErrNotPermitted = errors.New("hostinfo: not permitted to query the console user")
	// ErrUnsupported: the platform has no such lookup (the console session, a connection's or a
	// process's owner).
	ErrUnsupported = errors.New("hostinfo: the lookup is not supported on this platform")
)

// User is the person at the device as the operating system names them.
type User struct {
	// SID is the account's security identifier (Windows), or the uid elsewhere.
	SID string
	// Account is DOMAIN\user (Windows) or the user name.
	Account string
	// UPN is the user principal name, only when the system resolved one; it always contains '@'.
	UPN string
	// ObjectID is the Entra object id an Entra account's SID encodes (S-1-12-1-…).
	ObjectID string
	// Source is "console" for the console session's user, "process" for this process's own user.
	Source string
}

// Ref derives the user_ref: from the UPN when one resolved, else from the Entra object
// id, else from DOMAIN\user. The directory derives the same value from what SCIM sends, so the
// order is the one most likely to meet a directory row.
func (u User) Ref(key []byte) (string, protocol.UserRefKind, error) {
	switch {
	case u.UPN != "":
		ref, err := protocol.DeriveUserRef(key, protocol.UserRefUPN, u.UPN)
		return ref, protocol.UserRefUPN, err
	case u.ObjectID != "":
		ref, err := protocol.DeriveUserRef(key, protocol.UserRefOID, u.ObjectID)
		return ref, protocol.UserRefOID, err
	case u.Account != "":
		ref, err := protocol.DeriveUserRef(key, protocol.UserRefAccount, u.Account)
		return ref, protocol.UserRefAccount, err
	default:
		return "", "", errors.New("hostinfo: the user has no upn, object id or account name")
	}
}

// DisplayName is the clear name a 'clear' tenant sees: the UPN, else the account name.
func (u User) DisplayName() string {
	if u.UPN != "" {
		return u.UPN
	}
	return u.Account
}

// ObjectIDFromSID returns the Entra object id an S-1-12-1 SID encodes: its four sub-authorities
// are the GUID's sixteen bytes, each little-endian, and the GUID is rendered from those bytes the
// Windows way (the first three fields little-endian). Any other SID carries no object id.
func ObjectIDFromSID(sid string) (string, bool) {
	const prefix = "S-1-12-1-"
	if !strings.HasPrefix(strings.ToUpper(sid), prefix) {
		return "", false
	}
	parts := strings.Split(sid[len(prefix):], "-")
	if len(parts) != 4 {
		return "", false
	}
	var b [16]byte
	for i, p := range parts {
		n, err := strconv.ParseUint(p, 10, 32)
		if err != nil {
			return "", false
		}
		binary.LittleEndian.PutUint32(b[i*4:], uint32(n))
	}
	return fmt.Sprintf("%08x-%04x-%04x-%x-%x",
		binary.LittleEndian.Uint32(b[0:4]), binary.LittleEndian.Uint16(b[4:6]), binary.LittleEndian.Uint16(b[6:8]),
		b[8:10], b[10:16]), true
}

// UPNFromIdentityStore reads the UPN Windows cached for an account at sign-in. It is how an Entra
// account's UPN is known without asking a directory (HKLM\…\IdentityStore\Cache\<SID>\IdentityCache\<SID>).
func UPNFromIdentityStore(reg Registry, sid string) string {
	if reg == nil || sid == "" {
		return ""
	}
	v, err := reg.String(identityStoreKey+`\`+sid+`\IdentityCache\`+sid, "UserName")
	if err != nil {
		return ""
	}
	return cleanUPN(v)
}

// cleanUPN keeps a value only when it has the shape of a principal name; a bare account name in its
// place would derive a 'upn' ref no directory holds.
func cleanUPN(v string) string {
	v = strings.TrimSpace(v)
	at := strings.IndexByte(v, '@')
	if at <= 0 || at == len(v)-1 || strings.ContainsAny(v, `\ `) {
		return ""
	}
	return v
}

// serviceAccountSIDs are the accounts a service runs as: LocalSystem, LocalService and
// NetworkService on Windows, root (uid 0) elsewhere. A run that falls back to its own process user
// never reports one of these as the person.
var serviceAccountSIDs = map[string]bool{"S-1-5-18": true, "S-1-5-19": true, "S-1-5-20": true, "0": true}

// UserSources are the lookups Resolver drives. Console names the console session's user with what
// is cheap to read (SID, account, cached UPN); UPN asks the directory for that user's principal name,
// which can be slow, so the resolver calls it once per user and retries it rarely; Process names
// this process's own user, the fallback for a console run that may not query the session.
type UserSources struct {
	Console func() (User, error)
	UPN     func(User) (string, error)
	Process func() (User, error)
}

// Resolver names the interactive user and remembers the slow half of the answer per account, so a
// frequent re-evaluation costs one session query.
type Resolver struct {
	src        UserSources
	clock      func() time.Time
	upnTimeout time.Duration
	retryAfter time.Duration

	mu      sync.Mutex
	upns    map[string]string
	tried   map[string]time.Time
	lastErr error
}

// NewResolver builds a resolver over src. A directory lookup that has not answered in upnTimeout is
// abandoned for this round and tried again after retryAfter.
func NewResolver(src UserSources, clock func() time.Time) *Resolver {
	if clock == nil {
		clock = time.Now
	}
	return &Resolver{src: src, clock: clock, upnTimeout: 10 * time.Second, retryAfter: 10 * time.Minute,
		upns: map[string]string{}, tried: map[string]time.Time{}}
}

// Current returns the interactive user: the console session's, or, when this process may not ask,
// its own user unless that is a service account.
func (r *Resolver) Current() (User, error) {
	if r.src.Console == nil {
		return User{}, ErrUnsupported
	}
	u, err := r.src.Console()
	switch {
	case err == nil:
		u.Source = "console"
	case errors.Is(err, ErrNotPermitted) || errors.Is(err, ErrUnsupported):
		if r.src.Process == nil {
			return User{}, err
		}
		p, perr := r.src.Process()
		if perr != nil {
			return User{}, perr
		}
		if serviceAccountSIDs[strings.ToUpper(p.SID)] {
			return User{}, ErrNoConsoleUser
		}
		p.Source = "process"
		u = p
	default:
		return User{}, err
	}
	if u.ObjectID == "" {
		u.ObjectID, _ = ObjectIDFromSID(u.SID)
	}
	u.UPN = cleanUPN(u.UPN)
	if u.UPN == "" {
		u.UPN = r.directoryUPN(u)
	}
	return u, nil
}

func (r *Resolver) directoryUPN(u User) string {
	if r.src.UPN == nil || u.SID == "" {
		return ""
	}
	r.mu.Lock()
	if upn, ok := r.upns[u.SID]; ok {
		r.mu.Unlock()
		return upn
	}
	if at, ok := r.tried[u.SID]; ok && r.clock().Sub(at) < r.retryAfter {
		r.mu.Unlock()
		return ""
	}
	r.tried[u.SID] = r.clock()
	r.mu.Unlock()

	type answer struct {
		upn string
		err error
	}
	ch := make(chan answer, 1)
	go func() {
		upn, err := r.src.UPN(u)
		ch <- answer{cleanUPN(upn), err}
	}()
	select {
	case a := <-ch:
		if a.upn == "" {
			r.setErr(a.err)
			return ""
		}
		r.mu.Lock()
		r.upns[u.SID] = a.upn
		r.mu.Unlock()
		return a.upn
	case <-time.After(r.upnTimeout):
		r.setErr(errors.New("hostinfo: directory lookup of the user principal name timed out"))
		return ""
	}
}

func (r *Resolver) setErr(err error) {
	r.mu.Lock()
	r.lastErr = err
	r.mu.Unlock()
}

// LastLookupError is the most recent reason a directory UPN lookup gave nothing, for diagnostics.
func (r *Resolver) LastLookupError() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastErr
}
