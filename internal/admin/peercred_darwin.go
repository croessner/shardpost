//go:build darwin

package admin

import (
	"net"

	"golang.org/x/sys/unix"
)

// peerUID returns the user ID of the process on the other end of c.
func peerUID(c *net.UnixConn) (uint32, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return 0, err
	}

	var (
		cred *unix.Xucred
		gerr error
	)

	if err := raw.Control(func(fd uintptr) {
		cred, gerr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	}); err != nil {
		return 0, err
	}

	if gerr != nil {
		return 0, gerr
	}

	return cred.Uid, nil
}
