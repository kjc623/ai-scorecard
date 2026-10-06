package main

import "golang.org/x/sys/unix"

// socketPeerUID reads LOCAL_PEERCRED: the credentials the peer had when it connected.
func socketPeerUID(fd int) (uint32, error) {
	cred, err := unix.GetsockoptXucred(fd, unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	if err != nil {
		return 0, err
	}
	return cred.Uid, nil
}
