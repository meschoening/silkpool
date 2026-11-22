package slot

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/meschoening/silkpool/internal/exposure"
)

// Helper runs silkpool as one of the helpers Silkpool starts as the
// service account on macOS, if args, the arguments after the program's
// name, name one: the runner's launcher (LaunchMode), the clean's kill
// helper (KillAllMode) or the exposure check's helper (CheckMode), which
// sends its report to the agent's socket when it's given one. It
// returns the exit code to exit with, and false if args name none of
// them.
func Helper(args []string) (code int, ok bool) {
	if len(args) == 0 {
		return 0, false
	}
	switch args[0] {
	case LaunchMode:
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, LaunchMode+" takes the runner root")
			return 2, true
		}
		fmt.Fprintln(os.Stderr, Launch(args[1]))
		return 1, true
	case KillAllMode:
		uid, err := strconv.Atoi(strings.Join(args[1:], " "))
		if err != nil {
			fmt.Fprintln(os.Stderr, KillAllMode+" takes the service account's UID")
			return 2, true
		}
		if err := KillAll(uid); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1, true
		}
		return 0, true
	case CheckMode:
		if len(args) == 3 {
			return sendReport(args[1], args[2]), true
		}
		return exposure.Main(args[1:], os.Stdout), true
	}
	return 0, false
}

// sendReport runs the exposure check's helper with the request req, and
// sends its report to the agent over the socket at sock, not to its
// output, whose path any job of the service account can read in
// launchctl print (#241). Then it waits for the agent to close the
// connection, so that it's still running while the agent checks it's the
// check's job.
func sendReport(req, sock string) int {
	var rep bytes.Buffer
	code := exposure.Main([]string{req}, &rep)
	if err := send(sock, rep.Bytes()); err != nil {
		fmt.Fprintln(os.Stderr, "can't send the report to the agent:", err)
		return 1
	}
	return code
}

// send sends b over the socket at sock, then waits for the other end to
// close the connection.
func send(sock string, b []byte) error {
	c, err := net.Dial("unix", sock)
	if err != nil {
		return err
	}
	defer c.Close()
	if _, err := c.Write(b); err != nil {
		return err
	}
	if err := c.(*net.UnixConn).CloseWrite(); err != nil {
		return err
	}
	_, err = io.Copy(io.Discard, c)
	return err
}
