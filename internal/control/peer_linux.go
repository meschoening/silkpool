package control

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

func init() { peerUID = linuxPeerUID }

// linuxPeerUID reads the uid of c's peer through SO_PEERCRED, which the
// kernel records when the connection is made.
func linuxPeerUID(c net.Conn) (int, error) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return 0, fmt.Errorf("control: %T isn't a Unix socket", c)
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0, err
	}
	var cred *unix.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if credErr != nil {
		return 0, fmt.Errorf("control: reading the peer's credentials: %w", credErr)
	}
	return int(cred.Uid), nil
}
