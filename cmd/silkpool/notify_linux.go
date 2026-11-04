package main

import (
	"net"
	"os"
)

// notify sends state to systemd through the socket NOTIFY_SOCKET names,
// as sd_notify does, if it names one: a unit of Type=notify waits for
// READY=1. An abstract socket's name starts with "@".
func notify(state string) error {
	path := os.Getenv("NOTIFY_SOCKET")
	if path == "" {
		return nil
	}
	if path[0] == '@' {
		path = "\x00" + path[1:]
	}
	c, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		return err
	}
	defer c.Close()
	_, err = c.Write([]byte(state))
	return err
}
