//go:build !windows

package hostinfo

import (
	"errors"
	"os"
	"os/user"
	"strings"
)

// SystemSources reads what a non-Windows host states. There is no MDM enrolment, join state or
// readable firmware table here without privileges or new dependencies, so only the install id is
// read; the attestation is empty, which the enrolment omits.
func SystemSources() Sources {
	return Sources{MachineID: machineID}
}

// SystemUserSources has no console-session lookup on this platform, so the resolver falls back to
// this process's own user, as the agent always did here.
func SystemUserSources() UserSources {
	return UserSources{
		Console: func() (User, error) { return User{}, ErrUnsupported },
		Process: processUser,
	}
}

// SystemRegistry is nil: there is no registry.
func SystemRegistry() Registry { return nil }

func processUser() (User, error) {
	u, err := user.Current()
	if err != nil {
		return User{}, err
	}
	return User{SID: u.Uid, Account: u.Username}, nil
}

func machineID() (string, error) {
	for _, p := range []string{"/etc/machine-id", "/var/lib/dbus/machine-id"} {
		if b, err := os.ReadFile(p); err == nil {
			if id := strings.TrimSpace(string(b)); id != "" {
				return id, nil
			}
		}
	}
	return "", errors.New("hostinfo: no machine id")
}
