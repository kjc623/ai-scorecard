package localipc

import "golang.org/x/sys/unix"

// socketPeerUID reads SO_PEERCRED: the credentials the peer had when it connected.
func socketPeerUID(fd int) (uint32, error) {
	cred, err := unix.GetsockoptUcred(fd, unix.SOL_SOCKET, unix.SO_PEERCRED)
	if err != nil {
		return 0, err
	}
	return cred.Uid, nil
}
