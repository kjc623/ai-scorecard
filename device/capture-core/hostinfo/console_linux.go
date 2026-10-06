package hostinfo

import (
	"errors"

	"github.com/godbus/dbus/v5"
)

// errNoActiveSession reports that logind answered and the answer is "nobody": the seat has no active
// session. It is kept distinct from a D-Bus failure so the caller can tell an empty seat from a host
// where logind is not running at all.
var errNoActiveSession = errors.New("hostinfo: no active session on the seat")

// logindSession is the small part of systemd-logind's D-Bus API the console lookup needs: the uid of
// the active session on a seat. The D-Bus calls sit behind it so the selection rules are tested
// without a running logind.
type logindSession interface {
	activeSessionUID(seat string) (uint32, error)
	close()
}

// systemLogind talks to logind over a private system-bus connection, so closing it after a lookup
// never disturbs another user of the shared bus.
type systemLogind struct{ conn *dbus.Conn }

func newSystemLogind() (logindSession, error) {
	conn, err := dbus.SystemBusPrivate()
	if err != nil {
		return nil, err
	}
	if err := conn.Auth(nil); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if err := conn.Hello(); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return systemLogind{conn: conn}, nil
}

func (s systemLogind) close() { _ = s.conn.Close() }

// activeSessionUID asks the seat for its active session, then the session for its user. Seat.ActiveSession
// is a (so) variant (session id, session object path) and Session.User a (uo) variant (uid, user path).
func (s systemLogind) activeSessionUID(seat string) (uint32, error) {
	manager := s.conn.Object("org.freedesktop.login1", dbus.ObjectPath("/org/freedesktop/login1"))
	var seatPath dbus.ObjectPath
	if err := manager.Call("org.freedesktop.login1.Manager.GetSeat", 0, seat).Store(&seatPath); err != nil {
		return 0, classifyLogindError(err)
	}
	seatObject := s.conn.Object("org.freedesktop.login1", seatPath)
	var sessionID string
	var sessionPath dbus.ObjectPath
	if err := seatObject.Call("org.freedesktop.DBus.Properties.Get", 0,
		"org.freedesktop.login1.Seat", "ActiveSession").Store(&sessionID, &sessionPath); err != nil {
		return 0, classifyLogindError(err)
	}
	if sessionPath == "" || sessionPath == "/" {
		return 0, errNoActiveSession
	}
	sessionObject := s.conn.Object("org.freedesktop.login1", sessionPath)
	var uid uint32
	var userPath dbus.ObjectPath
	if err := sessionObject.Call("org.freedesktop.DBus.Properties.Get", 0,
		"org.freedesktop.login1.Session", "User").Store(&uid, &userPath); err != nil {
		return 0, classifyLogindError(err)
	}
	return uid, nil
}

// classifyLogindError turns logind's own "no such seat or session" answer into errNoActiveSession;
// every other failure (no service, no owner, transport) stays a D-Bus failure so a host without
// logind is reported unsupported rather than as an empty seat.
func classifyLogindError(err error) error {
	var e dbus.Error
	if errors.As(err, &e) {
		switch e.Name {
		case "org.freedesktop.login1.NoSuchSeat",
			"org.freedesktop.login1.NoSuchSession",
			"org.freedesktop.login1.NoSessionForUID":
			return errNoActiveSession
		}
	}
	return err
}

// openLogind and lookupUser are seams: production opens the system bus and resolves a uid through the
// account database, while a test supplies both.
var (
	openLogind = newSystemLogind
	lookupUser = UserOfUID
)

// consoleUser is the person signed in at the active local session on seat0, per systemd-logind. No
// active session, or a session owned by root, is ErrNoConsoleUser (attributed to nobody); a host
// without logind is ErrUnsupported, which the resolver answers with its own process user.
func consoleUser() (User, error) {
	return consoleUserFrom(openLogind, lookupUser)
}

func consoleUserFrom(open func() (logindSession, error), lookup func(uint32) (User, error)) (User, error) {
	session, err := open()
	if err != nil {
		return User{}, ErrUnsupported
	}
	defer session.close()
	uid, err := session.activeSessionUID("seat0")
	switch {
	case errors.Is(err, errNoActiveSession):
		return User{}, ErrNoConsoleUser
	case err != nil:
		return User{}, ErrUnsupported
	case uid == 0:
		return User{}, ErrNoConsoleUser
	}
	return lookup(uid)
}
