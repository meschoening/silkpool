//go:build unix

package control

import (
	"context"
	"errors"
	"net"
)

// errNoPeerCredentials fails the peer check on a Unix whose peer
// credentials Silkpool doesn't read.
var errNoPeerCredentials = errors.New("control: this OS's control channel isn't built yet")

// peerUID returns the uid of the process at the other end of c, from
// the socket's peer credentials. Each OS's file sets it.
var peerUID = func(net.Conn) (int, error) { return 0, errNoPeerCredentials }

// needsElevation ends a refusal of a request that needs elevation.
const needsElevation = "needs root: run it with sudo"

// identify returns the account of the process at the other end of c,
// by the uid the socket's peer credentials name.
func identify(_ context.Context, c net.Conn, lookup func(int) (Account, error)) (Account, error) {
	return identifyByUID(c, peerUID, lookup)
}

// dial connects to the control socket at path, and accepts it only if
// its peer credentials name serverUID.
func dial(ctx context.Context, path string, serverUID int) (net.Conn, error) {
	return dialByUID(ctx, path, serverUID, peerUID)
}
