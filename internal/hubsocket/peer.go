package hubsocket

import (
	"errors"
	"net"
)

// errNoPeerCredentials fails the peer check on an OS the Hub doesn't run
// on.
var errNoPeerCredentials = errors.New("hubsocket: the Hub's socket is built for Linux only")

// peerUID returns the uid of the process at the other end of c, from the
// socket's peer credentials. Linux's file sets it.
var peerUID = func(net.Conn) (int, error) { return 0, errNoPeerCredentials }
