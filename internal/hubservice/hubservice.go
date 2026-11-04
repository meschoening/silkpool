// Package hubservice is the Hub service, silkpool-hub serve, and the
// setup that comes before it, silkpool-hub init: the two that act on the
// Hub's host directly. The decisions behind it are in "Define how an
// operator sets up a Hub and connects GitHub",
// "Define node enrollment, removal, identity and Hub-agent transport",
// "Name the CLI and define its commands"
// and ADR 0013.
//
// The service runs as its own unprivileged system user, silkpool-hub,
// under systemd, with its state in StateDir and its socket in the
// runtime folder. It runs the Hub's loop (package hub) over its store,
// one listener that serves the agents' paths and the dashboard's, and
// the root-only socket the Hub's CLI asks through (package hubsocket),
// both over the operations of package hubops.
//
// The listener has three modes, which init picks and which are fixed
// afterwards:
//
//   - self-signed, the default: init makes a key and certificate in the
//     state folder, and enrollment codes pin the key;
//   - certificate files: systemd hands the service the operator's files
//     through LoadCredential=, in $CREDENTIALS_DIRECTORY, so root-owned
//     ones such as certbot's work as they are, and a path unit restarts
//     the Hub when either changes;
//   - behind a proxy: plain HTTP on loopback only, behind a proxy that
//     terminates TLS.
package hubservice

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/meschoening/silkpool/internal/hub"
	"github.com/meschoening/silkpool/internal/hubgithub"
	"github.com/meschoening/silkpool/internal/hubops"
	"github.com/meschoening/silkpool/internal/hubsocket"
	"github.com/meschoening/silkpool/internal/hubstore"
	"github.com/meschoening/silkpool/internal/node"
	"github.com/meschoening/silkpool/internal/secret"
	"github.com/meschoening/silkpool/internal/wire"
)

// Where the Hub lives on Linux, as packaging sets it up (ADR 0013): its
// state folder, systemd's StateDirectory=, and the folder of the units
// init writes. Init writes under a root, which is "/" outside tests.
const (
	StateDir = "/var/lib/silkpool-hub"
	UnitDir  = "/etc/systemd/system"
)

// The Hub's units: its service, and the path unit init writes in the
// certificate-files mode, with the service it starts.
const (
	Unit          = "silkpool-hub.service"
	CertsPathUnit = "silkpool-hub-certs.path"
	certsService  = "silkpool-hub-certs.service"
)

// The names of the certificate and its key: files in the state folder
// in the self-signed mode, and the credentials systemd hands the service
// in the certificate-files mode.
const (
	CertFile = "tls.crt"
	KeyFile  = "tls.key"
)

// Config is what the service needs.
type Config struct {
	// StateDir is the Hub's state folder, and Socket the path of its
	// socket, whose folder Serve makes if it's missing.
	StateDir string
	Socket   string
	// Credentials is the folder systemd hands the certificate files in,
	// $CREDENTIALS_DIRECTORY, which only the certificate-files mode needs.
	Credentials string
	// Version is the Hub's version.
	Version string
	// Log is where the service logs, or nil for slog.Default.
	Log *slog.Logger
	// GitHubAPI, GitHubWeb and GitHubHTTP reach GitHub, or are empty for
	// api.github.com and github.com. Only tests set them.
	GitHubAPI  string
	GitHubWeb  string
	GitHubHTTP *http.Client
	// Admit reports whether a caller's uid may use the socket, or is nil
	// for root only. Only tests set it.
	Admit func(uid int) bool
	// Dashboard, if set, serves the dashboard's paths: every path but the
	// agents'. Without one, they're not found.
	Dashboard func(*hubops.Ops) http.Handler
	// Listening, if set, is told the listener's address once the
	// listener and the socket are up.
	Listening func(net.Addr)
}

// Logger returns the Hub's log: slog's text lines, in UTC, at info, to
// w, which is stderr, for the journal.
func Logger(w io.Writer) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
		if a.Key == slog.TimeKey && len(groups) == 0 {
			a.Value = slog.TimeValue(a.Value.Time().UTC())
		}
		return a
	}}))
}

// Serve runs the Hub service until ctx ends, or one of its parts fails,
// which it returns. It refuses a Hub that init hasn't set up.
func Serve(ctx context.Context, c Config) error {
	log := c.Log
	if log == nil {
		log = slog.Default()
	}
	s, err := hubstore.Open(c.StateDir)
	if err != nil {
		return err
	}
	defer s.Close()
	settings := s.Hub().Settings
	if settings.HubID == "" {
		return errors.New("the Hub isn't set up: run sudo silkpool-hub init --url <its URL>")
	}
	ln, pin, err := listen(c, settings.Listener)
	if err != nil {
		return err
	}
	defer ln.Close()
	if err := os.MkdirAll(filepath.Dir(c.Socket), 0o755); err != nil {
		return err
	}
	sock, err := hubsocket.Listen(c.Socket)
	if err != nil {
		return err
	}
	defer sock.Close()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	gh := hubgithub.New(hubgithub.Config{Dir: c.StateDir, BaseURL: c.GitHubAPI, Web: c.GitHubWeb, HTTP: c.GitHubHTTP, Log: log})
	h := hub.New(s, hub.Config{GitHub: gh, Log: log})
	ops := hubops.New(hubops.Config{Hub: h, GitHub: gh, Web: c.GitHubWeb, Pin: pin, Version: c.Version, Protocol: wire.Protocol})
	var dashboard http.Handler
	if c.Dashboard != nil {
		dashboard = c.Dashboard(ops)
	}
	server := &http.Server{
		Handler:           routes(h.Handler(), dashboard),
		ReadHeaderTimeout: 30 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelInfo),
	}

	ended := make(chan error, 3)
	go func() { ended <- h.Run(ctx) }()
	go func() {
		ended <- (&hubsocket.Server{Ops: ops, Version: c.Version, Admit: c.Admit, Log: log}).Serve(ctx, sock)
	}()
	go func() {
		if err := server.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
			ended <- fmt.Errorf("the Hub's listener: %w", err)
			return
		}
		ended <- nil
	}()
	log.Info("the Hub is running", "version", c.Version, "url", settings.BaseURL, "listener", settings.Listener.Mode, "address", ln.Addr().String())
	if c.Listening != nil {
		c.Listening(ln.Addr())
	}
	var first error
	running := cap(ended)
	select {
	case <-ctx.Done():
	case first = <-ended:
		running--
	}
	cancel()
	closing, done := context.WithTimeout(context.Background(), 10*time.Second)
	defer done()
	server.Shutdown(closing)
	// Every part ends before the store closes.
	for range running {
		if err := <-ended; first == nil {
			first = err
		}
	}
	return first
}

// routes returns the listener's handler: the agents' paths, and every
// other path the dashboard's, or not found while there's none.
func routes(agents, dashboard http.Handler) http.Handler {
	if dashboard == nil {
		dashboard = http.NotFoundHandler()
	}
	mux := http.NewServeMux()
	mux.Handle(wire.ConnectPath, agents)
	mux.Handle(wire.EnrollPath, agents)
	mux.Handle("/", dashboard)
	return mux
}

// listen listens as the Hub's listener l says, and returns the pin of the
// key it serves in the self-signed mode, which enrollment codes carry.
func listen(c Config, l hubstore.Listener) (net.Listener, node.Pin, error) {
	var cfg *tls.Config
	var pin node.Pin
	switch l.Mode {
	case hubstore.SelfSigned:
		pair, err := readPair(c.StateDir)
		if err != nil {
			return nil, "", fmt.Errorf("the Hub's own key and certificate, which init made: %w", err)
		}
		cfg, pin = &tls.Config{Certificates: []tls.Certificate{pair}}, node.Pin(wire.PinOf(pair.Leaf))
	case hubstore.CertificateFiles:
		if c.Credentials == "" {
			return nil, "", errors.New("the Hub serves certificate files that systemd hands it through LoadCredential=, and $CREDENTIALS_DIRECTORY is unset: start it with systemctl start silkpool-hub")
		}
		pair, err := readPair(c.Credentials)
		if err != nil {
			return nil, "", fmt.Errorf("the certificate files systemd handed the Hub: %w", err)
		}
		cfg = &tls.Config{Certificates: []tls.Certificate{pair}}
	case hubstore.BehindProxy:
	default:
		return nil, "", fmt.Errorf("the Hub's listener mode %q isn't one it has", l.Mode)
	}
	ln, err := net.Listen("tcp", l.Address)
	if err != nil {
		return nil, "", err
	}
	if cfg != nil {
		cfg.MinVersion = tls.VersionTLS12
		ln = tls.NewListener(ln, cfg)
	}
	return ln, pin, nil
}

// readPair reads the certificate and key in dir, holding the key as a
// secret.
func readPair(dir string) (tls.Certificate, error) {
	certPEM, err := os.ReadFile(filepath.Join(dir, CertFile))
	if err != nil {
		return tls.Certificate{}, err
	}
	keyPEM, err := os.ReadFile(filepath.Join(dir, KeyFile))
	if err != nil {
		return tls.Certificate{}, err
	}
	return keyPair(certPEM, secret.New(string(keyPEM)))
}
