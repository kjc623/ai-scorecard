//go:build !windows

package hostinfo

import (
	"errors"
	"os"
	"os/user"
	"strconv"
	"strings"
)

// SystemSources reads what a macOS or Linux host states without privileges or a management
// agent: the install id. The attestation is empty, which the enrolment omits.
func SystemSources() Sources {
	return Sources{MachineID: machineID}
}

// SystemUserSources names the console user where the platform says who it is (macOS), and falls
// back to this process's own user elsewhere.
func SystemUserSources() UserSources {
	return UserSources{Console: consoleUser, Process: processUser}
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

// UserOfUID names the account with the given uid: the identity of a local peer such as the
// browser's native-messaging relay.
func UserOfUID(uid uint32) (User, error) {
	id := strconv.FormatUint(uint64(uid), 10)
	u, err := user.LookupId(id)
	if err != nil {
		return User{SID: id}, err
	}
	return User{SID: id, Account: u.Username, Source: "process"}, nil
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
