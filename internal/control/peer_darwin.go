package control

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

func init() {
	peerUID = darwinPeerUID
	// On macOS, the socket is in /var/run, and administrators are the
	// members of admin (ADR 0008).
	Path = "/var/run/silkpool/agent.sock"
	AdminGroups = []string{"admin"}
}

// darwinPeerUID reads the uid of c's peer through LOCAL_PEERCRED, which
// the kernel records when the connection is made.
func darwinPeerUID(c net.Conn) (int, error) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return 0, fmt.Errorf("control: %T isn't a Unix socket", c)
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0, err
	}
	var cred *unix.Xucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if credErr != nil {
		return 0, fmt.Errorf("control: reading the peer's credentials: %w", credErr)
	}
	return int(cred.Uid), nil
}
