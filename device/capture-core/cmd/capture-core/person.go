package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/core"
	"github.com/shadow-ai-capture/device/capture-core/hostinfo"
	"github.com/shadow-ai-capture/device/protocol"
)

// Who observations are attributed to. The service runs as LocalSystem (root elsewhere), so its own
// account says nothing about who is using the device. Observations from the proxy routes are
// attributed to the interactive console user, re-read on a short interval so a sign-out, a sign-in
// or a switch of user moves the attribution with it; observations from the browser are attributed
// to the account the browser runs as, named by the native-messaging endpoint. Each person's
// user_ref is derived under the tenant's key from enrolment: from the UPN, else the Entra object
// id, else DOMAIN\user.

// personInterval is how often the console user is re-read. A session query is cheap; the slow
// directory lookup behind a missing UPN is remembered per account by hostinfo.Resolver.
const personInterval = 15 * time.Second

// unattributedUserRef is the user_ref of an observation no person can be named for: nobody is at
// the console, or the tenant has not issued the key a reference is derived under. The envelope
// requires a user_ref, and this one is visibly not a person's (a derived one is "u_...").
const unattributedUserRef = "unattributed"

// userSources and collectHostFacts are the operating-system seams the service reads through, so a
// test supplies a console user and an attestation instead of the machine's.
var (
	userSources      = hostinfo.SystemUserSources
	collectHostFacts = func() hostinfo.Facts { return hostinfo.Collect(hostinfo.SystemSources()) }
)

// people holds the current console user and the key references are derived under.
type people struct {
	resolver *hostinfo.Resolver

	mu      sync.Mutex
	key     []byte
	user    hostinfo.User
	present bool
	lastErr error
}

func newPeople(src hostinfo.UserSources) *people {
	return &people{resolver: hostinfo.NewResolver(src, time.Now)}
}

// refresh re-reads the console user and reports whether the person changed.
func (p *people) refresh() bool {
	u, err := p.resolver.Current()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lastErr = err
	if err != nil {
		changed := p.present
		p.user, p.present = hostinfo.User{}, false
		return changed
	}
	changed := !p.present || p.user != u
	p.user, p.present = u, true
	return changed
}

// setKey installs the tenant's user-reference key and reports whether it changed.
func (p *people) setKey(encoded string) (bool, error) {
	if encoded == "" {
		return false, nil
	}
	key, err := protocol.DecodeUserRefKey(encoded)
	if err != nil {
		return false, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	changed := string(p.key) != string(key)
	p.key = key
	return changed, nil
}

func (p *people) snapshot() (hostinfo.User, bool, []byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.user, p.present, p.key, p.lastErr
}

// personOf is the user_ref and clear name an observation by u is stamped with, and where the
// user_ref came from (for the health document).
func (s *service) personOf(u hostinfo.User, present bool, key []byte) (core.Person, string) {
	var p core.Person
	source := "unattributed"
	if present && key != nil {
		if ref, kind, err := u.Ref(key); err == nil {
			p.UserRef, source = ref, string(kind)
		}
	}
	if p.UserRef == "" {
		p.UserRef = unattributedUserRef
		if present && key == nil {
			source = "unattributed (no user_ref_key from enrolment)"
		}
	}
	if present && s.currentDeviceIdentity() == protocol.DeviceIdentityClear {
		p.SubjectName = u.DisplayName()
	}
	return p, source
}

// consolePerson is the person proxy observations are attributed to now.
func (s *service) consolePerson() (core.Person, string) {
	u, present, key, _ := s.people.snapshot()
	return s.personOf(u, present, key)
}

// peerPerson is the person a browser observation is attributed to: the account the browser runs
// as. When that is the console user, the console user's resolved identity (with its UPN) is used.
func (s *service) peerPerson(u hostinfo.User) core.Person {
	console, present, key, _ := s.people.snapshot()
	if present && strings.EqualFold(console.SID, u.SID) {
		p, _ := s.personOf(console, true, key)
		return p
	}
	if u.UPN == "" && u.SID != "" {
		u.UPN = hostinfo.UPNFromIdentityStore(hostinfo.SystemRegistry(), u.SID)
	}
	if u.ObjectID == "" {
		u.ObjectID, _ = hostinfo.ObjectIDFromSID(u.SID)
	}
	p, _ := s.personOf(u, u.SID != "" || u.Account != "", key)
	return p
}

// currentDeviceIdentity is the tenant's identity setting the device acts on now.
func (s *service) currentDeviceIdentity() protocol.DeviceIdentity {
	s.idMu.Lock()
	defer s.idMu.Unlock()
	if s.identityMode.Valid() {
		return s.identityMode
	}
	return protocol.DeviceIdentityClear
}

// adoptDeviceIdentity records a setting the server stated and re-stamps the identity, so a tenant
// that switches to hashed stops receiving the clear name from the next observation on. An empty
// or invalid value changes nothing.
func (s *service) adoptDeviceIdentity(mode protocol.DeviceIdentity) {
	if !mode.Valid() {
		return
	}
	s.idMu.Lock()
	changed := s.identityMode != mode
	s.identityMode = mode
	s.idMu.Unlock()
	if changed {
		s.publishIdentity()
	}
}

// publishIdentity installs the issued device and the current console user on the pipeline.
// Before enrolment nothing is installed: the pipeline stays fail-closed rather than stamping a
// placeholder.
func (s *service) publishIdentity() {
	c := s.issuedCredential()
	if c == nil || s.pipe == nil {
		return
	}
	p, _ := s.consolePerson()
	s.pipe.SetIdentity(core.Identity{TenantID: c.TenantID, DeviceID: c.DeviceID, UserRef: p.UserRef, SubjectName: p.SubjectName})
}

// refreshPerson re-reads the console user and re-stamps the identity when the person changed.
func (s *service) refreshPerson() {
	if !s.people.refresh() {
		return
	}
	s.publishIdentity()
	u, present, _, err := s.people.snapshot()
	_, source := s.consolePerson()
	switch {
	case present:
		s.log.Info("console user changed", "account", u.Account, "upn_resolved", u.UPN != "", "user_ref_source", source)
	case errors.Is(err, hostinfo.ErrNoConsoleUser):
		s.log.Info("nobody is signed in at the console; proxy observations are unattributed")
	default:
		s.log.Warn("console user unresolved; proxy observations are unattributed", "error", err)
	}
}

// watchPeople re-reads the console user until ctx ends or stop closes.
func (s *service) watchPeople(ctx context.Context, stop <-chan struct{}) {
	t := time.NewTicker(personInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-stop:
			return
		case <-t.C:
			s.refreshPerson()
		}
	}
}

// resolvedHostname is the OS hostname, or empty when the system states none.
func (s *service) resolvedHostname() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(h)
}

// clearHostname is the machine name to send while the device identity setting is clear.
func (s *service) clearHostname() string {
	if s.currentDeviceIdentity() != protocol.DeviceIdentityClear {
		return ""
	}
	return s.resolvedHostname()
}

// hostnameHash is the hashed machine name sent instead when the setting is hashed: lower-cased
// before hashing so two spellings of one machine collapse.
func (s *service) hostnameHash() string {
	h := s.resolvedHostname()
	if s.currentDeviceIdentity() != protocol.DeviceIdentityHashed || h == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(strings.ToLower(h)))
	return "sha256:" + hex.EncodeToString(sum[:])
}
