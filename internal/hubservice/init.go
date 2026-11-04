package hubservice

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/meschoening/silkpool/internal/hubops"
	"github.com/meschoening/silkpool/internal/hubstore"
	"github.com/meschoening/silkpool/internal/node"
	"github.com/meschoening/silkpool/internal/secret"
)

// InitConfig is what init needs: its flags, and the machine it sets up.
type InitConfig struct {
	// Root is the root of the file system init writes into: "/" outside
	// tests.
	Root string
	// URL is the Hub's base URL. Cert and Key name certificate files,
	// both or neither, and BehindProxy picks plain HTTP on loopback: they
	// pick the listener's mode. Listen is the address to listen on, or
	// empty for the mode's default.
	URL         string
	Cert, Key   string
	BehindProxy bool
	Listen      string
	// Origin is init's origin, for history.
	Origin node.Origin
	Now    time.Time
	// Systemctl runs systemctl with args.
	Systemctl func(ctx context.Context, args ...string) error
	// NewSecret returns a new setup code, or is nil for a random one.
	// Only tests set it.
	NewSecret func() secret.Value
}

// The default addresses: a TLS mode listens on the URL's port, or 443,
// and behind a proxy on loopback.
const (
	defaultPort  = "443"
	proxyAddress = "127.0.0.1:8780"
)

// A SetUp is what init did: the Hub's URL and listener, the fingerprint
// of the certificate it made, if it made one, and the setup link, which
// JSON reveals.
type SetUp struct {
	URL         string          `json:"url"`
	Listener    hubops.Listener `json:"listener"`
	Fingerprint string          `json:"fingerprint,omitempty"`
	Link        secret.Value    `json:"-"`
	Expires     time.Time       `json:"expires"`
}

// MarshalJSON gives init's fields and the setup link.
func (s SetUp) MarshalJSON() ([]byte, error) {
	type fields SetUp
	return json.Marshal(struct {
		fields
		Link string `json:"link"`
	}{fields(s), s.Link.Reveal()})
}

// NotStarted fails an init that set the Hub up but couldn't start its
// service. The Hub is set up, and its setup link works once it runs.
type NotStarted struct {
	SetUp SetUp
	Err   error
}

func (e *NotStarted) Error() string {
	return fmt.Sprintf("the Hub is set up, but systemctl couldn't start it (%v): start it with systemctl enable --now silkpool-hub", e.Err)
}

func (e *NotStarted) Unwrap() error { return e.Err }

func refuse(code, format string, args ...any) error {
	return &hubops.Refusal{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Init sets up the Hub, which must not be set up yet: it writes the
// Hub's settings into its store, makes its key and self-signed
// certificate in the state folder, or the units that hand it the
// certificate files and restart it when they change, then starts the
// service as packaging says, with systemctl enable --now. A refusal is a
// *hubops.Refusal, and changes nothing.
func Init(ctx context.Context, c InitConfig) (SetUp, error) {
	base, err := hubops.CheckURL(c.URL)
	if err != nil {
		return SetUp{}, err
	}
	u, _ := url.Parse(base)
	mode := hubstore.SelfSigned
	switch {
	case c.BehindProxy:
		mode = hubstore.BehindProxy
	case c.Cert != "" || c.Key != "":
		mode = hubstore.CertificateFiles
	}
	addr, err := listenAddress(mode, u, c.Listen)
	if err != nil {
		return SetUp{}, err
	}
	var files certFiles
	if mode == hubstore.CertificateFiles {
		if files, err = checkCertFiles(c.Cert, c.Key, u.Hostname()); err != nil {
			return SetUp{}, err
		}
	}
	state := filepath.Join(c.Root, StateDir)
	if done, err := setUp(state); err != nil || done {
		if err == nil {
			err = refuse("set-up", "this Hub is set up already: for a new setup link, run silkpool-hub setup-code, and to change its URL, silkpool-hub set-url")
		}
		return SetUp{}, err
	}

	out := SetUp{URL: base, Listener: hubops.Listener{Mode: string(mode), Address: addr}}
	if mode == hubstore.SelfSigned {
		if out.Fingerprint, err = selfSign(state, u.Hostname(), c.Now); err != nil {
			return SetUp{}, err
		}
	}
	newSecret := c.NewSecret
	if newSecret == nil {
		newSecret = func() secret.Value { return secret.New(secret.CodePrefix + rand.Text()) }
	}
	code := newSecret()
	out.Expires = c.Now.Add(hubops.SetupLifetime).UTC()
	out.Link = secret.New(base + hubops.SetupPath + "?" + url.Values{"code": {code.Reveal()}}.Encode())
	settings := hubstore.Settings{
		HubID: rand.Text(), BaseURL: base, PoolLabel: "silkpool",
		Listener:  hubstore.Listener{Mode: mode, Address: addr},
		SetupCode: hubstore.SetupCode{Hash: hubops.SetupCodeHash(code), Expires: out.Expires},
	}
	if err := store(state, hubstore.SetSettings{Settings: settings, Origin: c.Origin}, c.Now); err != nil {
		return SetUp{}, err
	}
	if mode == hubstore.CertificateFiles {
		if err := writeUnits(filepath.Join(c.Root, UnitDir), files); err != nil {
			return out, err
		}
		for _, args := range [][]string{{"daemon-reload"}, {"enable", "--now", CertsPathUnit}} {
			if err := c.Systemctl(ctx, args...); err != nil {
				return out, &NotStarted{SetUp: out, Err: err}
			}
		}
	}
	if err := c.Systemctl(ctx, "enable", "--now", Unit); err != nil {
		return out, &NotStarted{SetUp: out, Err: err}
	}
	return out, nil
}

// listenAddress returns the address the listener of mode listens on, for
// the Hub at u, from --listen's value. Behind a proxy, it must be
// loopback.
func listenAddress(mode hubstore.ListenerMode, u *url.URL, listen string) (string, error) {
	if listen == "" {
		if mode == hubstore.BehindProxy {
			return proxyAddress, nil
		}
		port := u.Port()
		if port == "" {
			port = defaultPort
		}
		return ":" + port, nil
	}
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return "", refuse("invalid-listen", "the address %s won't do: write host:port, such as 127.0.0.1:8780, or :443", listen)
	}
	if mode == hubstore.BehindProxy {
		ip := net.ParseIP(host)
		if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return "", refuse("not-loopback", "behind a proxy, the Hub listens on loopback only, where the proxy reaches it, such as %s: not %s", proxyAddress, listen)
		}
	}
	return listen, nil
}

// certFiles are the certificate files init hands the service.
type certFiles struct{ cert, key string }

// unitPath is what a path in a unit file may hold, unquoted and without
// a specifier.
var unitPath = regexp.MustCompile(`^/[A-Za-z0-9/._+@=,:~-]+$`)

// checkCertFiles returns the certificate files as the units name them,
// once they're readable, a key pair, and a certificate for host.
func checkCertFiles(cert, key, host string) (certFiles, error) {
	var files certFiles
	for _, f := range []struct {
		to   *string
		path string
	}{{&files.cert, cert}, {&files.key, key}} {
		abs, err := filepath.Abs(f.path)
		if err != nil || f.path == "" {
			return certFiles{}, refuse("cert-files", "init takes both --cert and --key")
		}
		if !unitPath.MatchString(abs) {
			return certFiles{}, refuse("cert-path", "the path %s can't go in a systemd unit as it is: use one of letters, digits and /._+@=,:~-, without spaces", abs)
		}
		*f.to = abs
	}
	certPEM, err := os.ReadFile(files.cert)
	if err != nil {
		return certFiles{}, refuse("cert-files", "couldn't read the certificate: %v", err)
	}
	keyFile, err := os.ReadFile(files.key)
	if err != nil {
		return certFiles{}, refuse("cert-files", "couldn't read the key: %v", err)
	}
	pair, err := keyPair(certPEM, secret.New(string(keyFile)))
	if err != nil {
		return certFiles{}, refuse("cert-files", "the files %s and %s aren't a certificate and its key: %v", files.cert, files.key, err)
	}
	if err := pair.Leaf.VerifyHostname(host); err != nil {
		return certFiles{}, refuse("cert-host", "the certificate in %s isn't for %s, so nodes would refuse the Hub: %v", files.cert, host, err)
	}
	return files, nil
}

// keyPair parses a certificate and its key, which is a secret: no error
// quotes it.
func keyPair(certPEM []byte, key secret.Value) (tls.Certificate, error) {
	pair, err := tls.X509KeyPair(certPEM, []byte(key.Reveal()))
	if err != nil {
		return tls.Certificate{}, errors.New(secret.Scrub(err.Error()))
	}
	return pair, nil
}

// setUp reports whether the Hub in state is set up.
func setUp(state string) (bool, error) {
	if _, err := os.Stat(filepath.Join(state, hubstore.File)); errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	s, err := hubstore.Open(state)
	if err != nil {
		return false, err
	}
	defer s.Close()
	return s.Hub().Settings.HubID != "", nil
}

// store applies e to the Hub's store in state, which it makes, the Hub's
// account's alone, if it's missing.
func store(state string, e hubstore.Event, now time.Time) error {
	if err := os.MkdirAll(state, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(state, 0o700); err != nil {
		return err
	}
	s, err := hubstore.Open(state)
	if err != nil {
		return err
	}
	defer s.Close()
	_, err = s.Apply(e, now)
	return err
}

// certLifetime is how long the self-signed certificate lasts. Nodes pin
// its key, not the certificate, and browsers warn about it anyway.
const certLifetime = 10 * 365 * 24 * time.Hour

// selfSign makes the Hub's key and a self-signed certificate for host in
// state, and returns the certificate's SHA-256 fingerprint, for the
// operator to trust.
func selfSign(state, host string, now time.Time) (string, error) {
	if err := os.MkdirAll(state, 0o700); err != nil {
		return "", err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return "", err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host, Organization: []string{"Silkpool Hub"}},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(certLifetime),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if ip := net.ParseIP(host); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		tmpl.DNSNames = []string{host}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return "", err
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return "", err
	}
	keyPEM := secret.New(string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})))
	if err := writeFile(filepath.Join(state, KeyFile), []byte(keyPEM.Reveal())); err != nil {
		return "", err
	}
	if err := writeFile(filepath.Join(state, CertFile), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})); err != nil {
		return "", err
	}
	return fingerprint(der), nil
}

// fingerprint is a certificate's SHA-256 fingerprint as browsers show it:
// uppercase hex pairs between colons.
func fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	h := strings.ToUpper(hex.EncodeToString(sum[:]))
	pairs := make([]string, 0, len(sum))
	for i := 0; i < len(h); i += 2 {
		pairs = append(pairs, h[i:i+2])
	}
	return strings.Join(pairs, ":")
}

// writeFile writes b to path, its owner's alone, synced before it's
// renamed into place.
func writeFile(path string, b []byte) error {
	tmp := path + ".new"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// writeUnits writes, in dir, the drop-in that hands the Hub's service
// its certificate files through LoadCredential=, which systemd reads as
// root, and the path unit and its service that restart the Hub when
// either file changes.
func writeUnits(dir string, f certFiles) error {
	units := map[string]string{
		filepath.Join(Unit+".d", "tls.conf"): "# Written by silkpool-hub init: the certificate files the Hub serves, which\n" +
			"# systemd reads as root and hands the service in $CREDENTIALS_DIRECTORY.\n" +
			"[Service]\n" +
			"LoadCredential=" + CertFile + ":" + f.cert + "\n" +
			"LoadCredential=" + KeyFile + ":" + f.key + "\n",
		CertsPathUnit: "# Written by silkpool-hub init: restarts the Hub when its certificate files\n" +
			"# change, so it serves the renewed ones.\n" +
			"[Unit]\n" +
			"Description=Watch the Silkpool Hub's certificate files\n" +
			"\n" +
			"[Path]\n" +
			"PathChanged=" + f.cert + "\n" +
			"PathChanged=" + f.key + "\n" +
			"Unit=" + certsService + "\n" +
			"\n" +
			"[Install]\n" +
			"WantedBy=multi-user.target\n",
		certsService: "# Written by silkpool-hub init: " + CertsPathUnit + " starts it when the\n" +
			"# Hub's certificate files change.\n" +
			"[Unit]\n" +
			"Description=Restart the Silkpool Hub to serve its renewed certificate files\n" +
			"\n" +
			"[Service]\n" +
			"Type=oneshot\n" +
			"ExecStart=systemctl try-restart " + Unit + "\n",
	}
	for name, content := range units {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			return err
		}
	}
	return nil
}
