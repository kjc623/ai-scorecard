package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/core"
	"github.com/shadow-ai-capture/device/capture-core/detect"
	"github.com/shadow-ai-capture/device/capture-core/hostinfo"
	"github.com/shadow-ai-capture/device/protocol"
)

// The person observations are attributed to (contract §4). The service runs as LocalSystem, so its
// own account says nothing about who is using the device; the person is the interactive console
// user, named by hostinfo and re-read on a short interval so a sign-out, a sign-in or a switch of
// user moves the attribution with it. Their user_ref is derived under the tenant's key from
// enrolment — from the UPN, else the Entra object id, else DOMAIN\user — and a configured
// SAC_USER_REF (the lab) wins over all of it.

// personInterval is how often the console user is re-read. A session query is cheap; the slow
// directory lookup behind a missing UPN is remembered per account by hostinfo.Resolver.
const personInterval = 15 * time.Second

// userSources and collectHostFacts are the operating-system seams the service reads through, so a
// test supplies a console user and an attestation instead of the machine's.
var (
	userSources      = hostinfo.SystemUserSources
	collectHostFacts = func() hostinfo.Facts { return hostinfo.Collect(hostinfo.SystemSources()) }
)

// people holds the current console user and the key their reference is derived under.
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
	if p == nil {
		return false
	}
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
	if p == nil || encoded == "" {
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
	if p == nil {
		return hostinfo.User{}, false, nil, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.user, p.present, p.key, p.lastErr
}

// personIdentity is the user_ref and the clear name an observation is stamped with now, and where
// the user_ref came from (for the health document).
func (s *service) personIdentity() (userRef, subject, source string) {
	u, present, key, _ := s.people.snapshot()
	switch {
	case strings.TrimSpace(s.cfg.UserRef) != "":
		userRef, source = s.cfg.UserRef, "configured"
	case present && key != nil:
		if ref, kind, err := u.Ref(key); err == nil {
			userRef, source = ref, string(kind)
		}
	}
	if userRef == "" {
		userRef, source = unattributedUserRef, "unattributed"
		if present && key == nil {
			source = "unattributed (no user_ref_key from enrolment)"
		}
	}
	if s.currentDeviceIdentity() == protocol.DeviceIdentityClear {
		switch {
		case strings.TrimSpace(s.cfg.SubjectName) != "":
			subject = strings.TrimSpace(s.cfg.SubjectName)
		case present:
			subject = u.DisplayName()
		}
	}
	return userRef, subject, source
}

// currentDeviceIdentity is the tenant's identity setting the device acts on now.
func (s *service) currentDeviceIdentity() protocol.DeviceIdentity {
	s.idMu.Lock()
	defer s.idMu.Unlock()
	if s.identityMode.Valid() {
		return s.identityMode
	}
	return s.cfg.deviceIdentityMode()
}

// adoptDeviceIdentity records a setting the server stated and re-stamps the identity, so a tenant
// that switches to 'hashed' stops receiving the clear name from the next observation on.
func (s *service) adoptDeviceIdentity(mode protocol.DeviceIdentity) {
	if !mode.Valid() {
		return
	}
	s.idMu.Lock()
	changed := s.identityMode != mode
	s.identityMode = mode
	s.idMu.Unlock()
	if s.health != nil {
		s.health.adoptDeviceIdentity(mode)
	}
	if changed {
		s.publishIdentity()
	}
}

// publishIdentity installs the device and the current person on the pipeline and proc.detect. A
// drain run has no device until enrolment issues one, and then nothing is installed: the pipeline
// stays fail-closed rather than stamping a placeholder.
func (s *service) publishIdentity() {
	if s.pipe == nil {
		return
	}
	s.idMu.Lock()
	tenant, device, ok := s.issuedTenant, s.issuedDevice, s.issued
	s.idMu.Unlock()
	if strings.TrimSpace(s.cfg.DeviceEndpoint) == "" {
		tenant, device, ok = s.cfg.TenantID, s.cfg.DeviceID, true
	}
	if !ok {
		return
	}
	userRef, subject, _ := s.personIdentity()
	s.pipe.SetIdentity(core.Identity{TenantID: tenant, DeviceID: device, UserRef: userRef, SubjectName: subject})
	if s.detect != nil {
		s.detect.SetIdentity(detect.Identity{TenantID: tenant, DeviceID: device, UserRef: userRef, SubjectName: subject})
	}
}

// refreshPerson re-reads the console user and re-stamps the identity when the person changed.
func (s *service) refreshPerson() {
	if s.people.refresh() {
		s.publishIdentity()
		u, present, _, err := s.people.snapshot()
		_, _, source := s.personIdentity()
		switch {
		case present:
			s.log.Info("console user changed", "account", u.Account, "upn_resolved", u.UPN != "", "user_ref_source", source)
		case errors.Is(err, hostinfo.ErrNoConsoleUser):
			s.log.Info("nobody is signed in at the console; observations are unattributed")
		default:
			s.log.Warn("console user unresolved; observations are unattributed", "error", err)
		}
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
