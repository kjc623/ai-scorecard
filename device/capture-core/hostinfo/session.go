package hostinfo

import (
	"errors"
	"fmt"
)

// noConsoleSession is what Windows answers for the physical console's session while no session is
// attached to it (WTSGetActiveConsoleSessionId).
const noConsoleSession = 0xFFFFFFFF

// errSeveralRemoteUsers: nobody is signed in at the console and more than one person is connected
// to a remote session, so none of them is the person at the device.
var errSeveralRemoteUsers = errors.New("hostinfo: nobody is signed in at the console and several people are connected remotely")

// interactiveSession chooses the Windows session whose user is the person at the device: the
// physical console's while someone is signed in there, else the one connected session someone is
// signed in to. A Hyper-V enhanced session and a Remote Desktop connection put the person in a
// remote session and leave the console at the sign-in screen. A disconnected session is not
// considered, because nobody is at it.
//
// connected lists the sessions in state WTSActive. signedIn reports whether someone is signed in to
// a session, and fails only when that cannot be told; a failure ends the choice, so a session that
// could not be asked about is never passed over for another.
func interactiveSession(console uint32, connected func() ([]uint32, error), signedIn func(uint32) (bool, error)) (uint32, error) {
	if console != noConsoleSession {
		ok, err := signedIn(console)
		if err != nil {
			return 0, err
		}
		if ok {
			return console, nil
		}
	}
	ids, err := connected()
	if err != nil {
		return 0, err
	}
	var found []uint32
	for _, id := range ids {
		// Session 0 is the services' session and never has a person in it.
		if id == 0 || id == console {
			continue
		}
		ok, err := signedIn(id)
		if err != nil {
			return 0, err
		}
		if ok {
			found = append(found, id)
		}
	}
	switch len(found) {
	case 0:
		return 0, ErrNoConsoleUser
	case 1:
		return found[0], nil
	default:
		return 0, fmt.Errorf("%w (sessions %v)", errSeveralRemoteUsers, found)
	}
}
