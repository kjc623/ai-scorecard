package hostinfo

import (
	"os"
	"syscall"
)

// consoleUser is the owner of /dev/console, which macOS gives to the user signed in at the
// console (root while the login window is showing).
func consoleUser() (User, error) {
	st, err := os.Stat("/dev/console")
	if err != nil {
		return User{}, ErrUnsupported
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return User{}, ErrUnsupported
	}
	if sys.Uid == 0 {
		return User{}, ErrNoConsoleUser
	}
	return UserOfUID(sys.Uid)
}
