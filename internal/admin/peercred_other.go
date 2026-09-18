//go:build !linux && !darwin

package admin

import (
	"errors"
	"net"
)

// peerUID is not available on this platform; the socket admits no one.
func peerUID(*net.UnixConn) (uint32, error) {
	return 0, errors.New("peer credentials not supported on this platform")
}
