package hostinfo

import (
	"errors"
	"testing"
)

// sessionTable is a machine's sessions as interactiveSession asks about them: the connected ones and
// the ones someone is signed in to. asked records each session queried, in order.
type sessionTable struct {
	connected []uint32
	listErr   error
	users     map[uint32]bool
	queryErr  map[uint32]error
	asked     []uint32
	listed    bool
}

func (s *sessionTable) list() ([]uint32, error) {
	s.listed = true
	return s.connected, s.listErr
}

func (s *sessionTable) signedIn(id uint32) (bool, error) {
	s.asked = append(s.asked, id)
	if err := s.queryErr[id]; err != nil {
		return false, err
	}
	return s.users[id], nil
}

func TestInteractiveSessionIsTheConsoleWhileSomeoneIsSignedInThere(t *testing.T) {
	// A remote session connected beside a signed-in console (a Windows Server host) does not move the
	// choice off the console.
	s := &sessionTable{connected: []uint32{1, 3}, users: map[uint32]bool{1: true, 3: true}}
	got, err := interactiveSession(1, s.list, s.signedIn)
	if err != nil || got != 1 {
		t.Fatalf("interactiveSession = %d, %v; want the console's session 1", got, err)
	}
	if s.listed {
		t.Error("the sessions were listed although the console has a user")
	}
}

// The person is in session 2 through a Hyper-V enhanced session (a Remote Desktop connection has the
// same shape) and the console's session 5 is at the sign-in screen.
func TestInteractiveSessionIsTheConnectedRemoteSessionWhileTheConsoleIsAtTheSignInScreen(t *testing.T) {
	s := &sessionTable{connected: []uint32{2}, users: map[uint32]bool{2: true}}
	got, err := interactiveSession(5, s.list, s.signedIn)
	if err != nil || got != 2 {
		t.Fatalf("interactiveSession = %d, %v; want the remote session 2", got, err)
	}
}

func TestInteractiveSessionWithNoSessionAtTheConsole(t *testing.T) {
	s := &sessionTable{connected: []uint32{3}, users: map[uint32]bool{3: true}}
	got, err := interactiveSession(noConsoleSession, s.list, s.signedIn)
	if err != nil || got != 3 {
		t.Fatalf("interactiveSession = %d, %v; want the remote session 3", got, err)
	}
	for _, id := range s.asked {
		if id == noConsoleSession {
			t.Error("the absent console session was queried")
		}
	}
}

func TestInteractiveSessionNobodyAtTheDevice(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    *sessionTable
	}{
		{"no connected session", &sessionTable{}},
		// A connected session at the sign-in screen has nobody in it.
		{"connected but nobody signed in", &sessionTable{connected: []uint32{4}}},
		// The services' session is never the person's, whatever it reports.
		{"only the services' session", &sessionTable{connected: []uint32{0}, users: map[uint32]bool{0: true}}},
	} {
		if got, err := interactiveSession(1, tc.s.list, tc.s.signedIn); !errors.Is(err, ErrNoConsoleUser) {
			t.Errorf("%s: interactiveSession = %d, %v; want ErrNoConsoleUser", tc.name, got, err)
		}
	}
}

// Two people connected remotely and nobody at the console (a multi-session host): picking one would
// attribute the other's traffic to them.
func TestInteractiveSessionNamesNoOneAmongSeveralRemoteUsers(t *testing.T) {
	s := &sessionTable{connected: []uint32{2, 3}, users: map[uint32]bool{2: true, 3: true}}
	got, err := interactiveSession(1, s.list, s.signedIn)
	if !errors.Is(err, errSeveralRemoteUsers) {
		t.Fatalf("interactiveSession = %d, %v; want errSeveralRemoteUsers", got, err)
	}
}

func TestInteractiveSessionAsksAboutTheConsoleOnce(t *testing.T) {
	// The console's session is listed among the connected ones and has nobody in it.
	s := &sessionTable{connected: []uint32{1, 2}, users: map[uint32]bool{2: true}}
	if got, err := interactiveSession(1, s.list, s.signedIn); err != nil || got != 2 {
		t.Fatalf("interactiveSession = %d, %v; want session 2", got, err)
	}
	if len(s.asked) != 2 || s.asked[0] != 1 || s.asked[1] != 2 {
		t.Errorf("sessions asked about = %v, want [1 2]", s.asked)
	}
}

func TestInteractiveSessionStopsAtAQuestionItCannotAnswer(t *testing.T) {
	// A process without the service's privilege is told so, which the resolver answers with its own
	// user.
	s := &sessionTable{queryErr: map[uint32]error{1: ErrNotPermitted}}
	if _, err := interactiveSession(1, s.list, s.signedIn); !errors.Is(err, ErrNotPermitted) {
		t.Errorf("console query refused: err = %v, want ErrNotPermitted", err)
	}
	// A remote session that cannot be asked about may hold a second person, so the one found is not
	// taken as the only one.
	failed := errors.New("WTSQueryUserToken(3): the system failed")
	s = &sessionTable{connected: []uint32{2, 3}, users: map[uint32]bool{2: true}, queryErr: map[uint32]error{3: failed}}
	if got, err := interactiveSession(1, s.list, s.signedIn); !errors.Is(err, failed) {
		t.Errorf("remote query failed: interactiveSession = %d, %v; want the failure", got, err)
	}
	listFailed := errors.New("WTSEnumerateSessionsW: the system failed")
	s = &sessionTable{listErr: listFailed}
	if got, err := interactiveSession(1, s.list, s.signedIn); !errors.Is(err, listFailed) {
		t.Errorf("listing failed: interactiveSession = %d, %v; want the failure", got, err)
	}
}
