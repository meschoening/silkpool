// Command silkpool-hub is Silkpool's Hub binary: the Hub CLI that the
// operator runs as root on the Hub's host, and, through a mode hidden
// from its help, the Hub service, silkpool-hub serve, which only systemd
// runs. See "Name the CLI and define its commands".
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/meschoening/silkpool/internal/dashboard"
	"github.com/meschoening/silkpool/internal/hubcli"
	"github.com/meschoening/silkpool/internal/hubops"
	"github.com/meschoening/silkpool/internal/hubservice"
	"github.com/meschoening/silkpool/internal/hubsocket"
	"github.com/meschoening/silkpool/internal/version"
)

// account is the system user the Hub service runs as, which packaging
// makes (ADR 0013).
const account = "silkpool-hub"

func main() {
	args := os.Args[1:]
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if len(args) > 0 && args[0] == "serve" {
		os.Exit(serve(ctx, args[1:]))
	}
	os.Exit(hubcli.Run(ctx, args, hubcli.Env{
		Stdin:         os.Stdin,
		Stdout:        os.Stdout,
		Stderr:        os.Stderr,
		StdinTerminal: terminal(os.Stdin),
		Socket:        hubsocket.Path,
		ServerUID:     uidOf(account),
		Version:       version.Version,
		Zone:          time.Local,
		Now:           time.Now,
		Root:          "/",
		Elevated:      os.Geteuid() == 0,
		Systemctl:     systemctl,
	}))
}

// serve runs the Hub service in the foreground, as its systemd unit
// does, as the Hub's own account, and tells systemd it's ready once its
// listener and socket are up. It finds its folders where systemd says:
// the state folder in $STATE_DIRECTORY, the socket's in
// $RUNTIME_DIRECTORY, and the certificate files in
// $CREDENTIALS_DIRECTORY.
func serve(ctx context.Context, args []string) int {
	if len(args) > 0 {
		fmt.Fprintln(os.Stderr, "usage: silkpool-hub serve")
		return 2
	}
	log := hubservice.Logger(os.Stderr)
	if os.Geteuid() == 0 {
		fmt.Fprintln(os.Stderr, "silkpool-hub serve: the Hub runs as its own account, "+account+", as its systemd unit starts it, not as root")
		return 1
	}
	state := first(os.Getenv("STATE_DIRECTORY"), hubservice.StateDir)
	socket := filepath.Join(first(os.Getenv("RUNTIME_DIRECTORY"), filepath.Dir(hubsocket.Path)), filepath.Base(hubsocket.Path))
	err := hubservice.Serve(ctx, hubservice.Config{
		StateDir:    state,
		Socket:      socket,
		Credentials: os.Getenv("CREDENTIALS_DIRECTORY"),
		Version:     version.Version,
		Log:         log,
		Dashboard: func(ops *hubops.Ops) http.Handler {
			return dashboard.New(dashboard.Config{Ops: ops, Log: log})
		},
		Listening: func(net.Addr) {
			if err := notify("READY=1"); err != nil {
				log.Warn("couldn't tell systemd the Hub is ready", "err", err)
			}
		},
	})
	if err != nil {
		log.Error("the Hub stopped", "err", err)
		return 1
	}
	return 0
}

// first returns the first of the folders systemd lists, separated by
// colons, or def if it lists none.
func first(dirs, def string) string {
	if d, _, _ := strings.Cut(dirs, ":"); d != "" {
		return d
	}
	return def
}

// uidOf returns the uid of the account name, or -1 if there's none, so
// no server is taken for the Hub.
func uidOf(name string) int {
	u, err := user.Lookup(name)
	if err != nil {
		return -1
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return -1
	}
	return uid
}

// systemctl runs systemctl with args, and fails with what it printed.
func systemctl(ctx context.Context, args ...string) error {
	out, err := exec.CommandContext(ctx, "systemctl", args...).CombinedOutput()
	if err != nil {
		if msg := strings.TrimSpace(string(out)); msg != "" {
			return errors.New(msg)
		}
		return err
	}
	return nil
}

// terminal reports whether f is a terminal.
func terminal(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}
