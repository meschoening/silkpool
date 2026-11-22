// Package runnerinstall is the agent's I/O for the runner application on
// Linux, macOS and Windows: it downloads a version's package from
// GitHub, checks it against the SHA-256 the Hub sent, installs it in the
// agent's state, where only root or SYSTEM can reach it, and builds each
// runner's root from links to an install: hardlinks on Linux and
// Windows, APFS clones on macOS. The decision behind it is "Define how
// the agent installs, verifies and updates the runner application",
// recorded as ADR 0011.
//
// It carries out package node's effects and answers with its events:
// InstallRunnerApp becomes Install, which answers InstallReady or
// InstallFailed, DeleteInstall becomes Delete, and Found gives Started
// its Installs. The node owns the rest: when to retry, which install new
// runners start from, and which ones no runner uses any more.
package runnerinstall

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/meschoening/silkpool/internal/node"
	"github.com/meschoening/silkpool/internal/runnerapp"
	"github.com/meschoening/silkpool/internal/secret"
)

// Installs are a node's installs of the runner application, one folder
// per version, in a folder of the agent's state that only root can
// read, such as /var/lib/silkpool/runner, or on Windows only SYSTEM can
// use. Slots never see it: each runner reaches its install only through
// the links in its own root.
//
// The folder must be on the same filesystem as the slots' folders, so
// that their roots can be linked to an install: hardlinked on Linux and
// Windows, where a file NTFS won't link is copied, and cloned on macOS,
// where it must be an APFS volume. Installs owns
// everything in it: a folder named for a version is a complete, verified
// install, and anything else is what an install left when the agent
// stopped part-way.
//
// Install, Delete and Link may run at the same time for different
// versions. Found runs alone, when the agent starts.
type Installs struct {
	// Dir is the folder.
	Dir string
	// Platform is the node's platform: linux-x64, linux-arm64 or
	// osx-arm64, or on Windows win-x64 or win-arm64.
	Platform runnerapp.Platform
	// Client downloads the packages, or nil for http.DefaultClient.
	Client *http.Client
	// Account is the SID of the machine's Silkpool service account,
	// which every slot's runner runs as, such as
	// S-1-5-21-1004336348-1177238915-682003330-1001: on Windows only,
	// where the install's files let it read and run them, and each
	// runner's root is its. Linux and macOS name the account to Link.
	Account string
}

// MaxInstalls is how many installs a node keeps at most: the one new
// runners start from, and the one before it while a runner holding a job
// still uses it.
const MaxInstalls = 2

// Stall is how long a download may go without data before it fails.
const Stall = time.Minute

// Install downloads the runner application's package of app.Version for
// the node's platform from actions/runner's releases on GitHub, from a
// URL it builds itself. It checks the package's SHA-256 against
// app.SHA256 before extracting anything, extracts it under a temporary
// name and renames it into place, then deletes the package. It returns
// the node's event: InstallReady, or InstallFailed with a reason that
// completes "Couldn't update the runner application to 2.338.2: ...".
//
// The reason is scrubbed (secret.Scrub): it holds text Silkpool didn't
// write, such as GitHub's answers and the signed URLs it redirects to,
// and everything that shows it copies it from here.
//
// Install never retries: the node does, from node.FirstInstallRetry to
// node.MaxInstallRetry. A version already installed is ready at once,
// and a new one fails while MaxInstalls others are installed.
func (i Installs) Install(ctx context.Context, app node.InstallRunnerApp) node.Event {
	if err := i.install(ctx, app); err != nil {
		return node.InstallFailed{Version: app.Version, Reason: secret.Scrub(err.Error())}
	}
	return node.InstallReady{Version: app.Version}
}

func (i Installs) install(ctx context.Context, app node.InstallRunnerApp) error {
	if !runnerapp.Valid(app.Version) {
		return fmt.Errorf("%q isn't a version of the runner application", app.Version)
	}
	unpack, err := i.extractor()
	if err != nil {
		return err
	}
	if err := i.prepare(); err != nil {
		return failure(err)
	}
	installed, _, err := i.list()
	if err != nil {
		return failure(err)
	}
	if slices.Contains(installed, app.Version) {
		return nil
	}
	if len(installed) >= MaxInstalls {
		return errors.New("two other installs are still in use")
	}
	pkg, err := i.download(ctx, app)
	if err != nil {
		return err
	}
	defer os.Remove(pkg)
	tmp, err := os.MkdirTemp(i.Dir, ".install-"+app.Version+"-")
	if err != nil {
		return failure(err)
	}
	if err := unpack(pkg, tmp); err != nil {
		os.RemoveAll(tmp)
		return err
	}
	if err := i.finish(tmp, i.path(app.Version)); err != nil {
		os.RemoveAll(tmp)
		return failure(err)
	}
	return nil
}

// beforeRename runs with each finished extraction's folder, just before
// it's renamed into place. Only tests change it, to watch.
var beforeRename = func(tmp string) {}

// download downloads app's package into a temporary file in Dir, checks
// its SHA-256, and returns the file's path.
func (i Installs) download(ctx context.Context, app node.InstallRunnerApp) (string, error) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	watchdog := time.AfterFunc(Stall, func() { cancel(errStalled) })
	defer watchdog.Stop()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, runnerapp.URL(i.Platform, app.Version), nil)
	if err != nil {
		return "", err
	}
	client := i.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if context.Cause(ctx) == errStalled {
		return "", errStalled
	}
	if err != nil {
		return "", fmt.Errorf("couldn't reach GitHub: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GitHub answered %s for its package", resp.Status)
	}
	f, err := os.CreateTemp(i.Dir, ".download-"+app.Version+"-")
	if err != nil {
		return "", failure(err)
	}
	h := sha256.New()
	body := &body{r: resp.Body, watchdog: watchdog}
	_, err = io.Copy(io.MultiWriter(f, h), body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	switch {
	case context.Cause(ctx) == errStalled:
		// Even if the body then ended cleanly.
		err = errStalled
	case body.err != nil:
		err = fmt.Errorf("the download broke off: %w", body.err)
	case err != nil:
		err = failure(err)
	case hex.EncodeToString(h.Sum(nil)) != strings.ToLower(app.SHA256):
		err = errors.New("its package doesn't match the SHA-256 the Hub sent")
	}
	if err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// errStalled cancels a download that went Stall without data.
var errStalled = errors.New("no data came from GitHub for a minute")

// A body is a download's body. Data puts off its watchdog by another
// Stall, and the body remembers its read error, so that a failed
// download can be told from a failed write.
type body struct {
	r        io.Reader
	watchdog *time.Timer
	err      error
}

func (b *body) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if n > 0 {
		b.watchdog.Reset(Stall)
	}
	if err != nil && err != io.EOF {
		b.err = err
	}
	return n, err
}

// Found returns the versions installed, and deletes whatever an install
// left when the agent stopped part-way. The agent calls it when it
// starts, before any Install, for node.Started's Installs.
func (i Installs) Found() ([]string, error) {
	installed, leftovers, err := i.list()
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	for _, name := range leftovers {
		if err := os.RemoveAll(filepath.Join(i.Dir, name)); err != nil {
			return nil, err
		}
	}
	return installed, nil
}

// Delete deletes the install of version, which no runner uses any more.
// Runners already started from it keep their own links. Deleting an
// install that isn't there succeeds.
func (i Installs) Delete(version string) error {
	if !runnerapp.Valid(version) {
		return fmt.Errorf("%q isn't a version of the runner application", version)
	}
	return os.RemoveAll(i.path(version))
}

// list returns the names in Dir: the versions installed, and the
// leftovers of installs that stopped part-way.
func (i Installs) list() (installed, leftovers []string, err error) {
	entries, err := os.ReadDir(i.Dir)
	if err != nil {
		return nil, nil, err
	}
	for _, e := range entries {
		if e.IsDir() && runnerapp.Valid(e.Name()) {
			installed = append(installed, e.Name())
		} else {
			leftovers = append(leftovers, e.Name())
		}
	}
	return installed, leftovers, nil
}

// path returns where the install of version is.
func (i Installs) path(version string) string { return filepath.Join(i.Dir, version) }

// entryFailed describes err, which extracting the package's entry name
// met, or returns nil.
func entryFailed(name string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("couldn't extract %q from its package: %w", name, err)
}

func damaged(err error) error { return fmt.Errorf("its package is damaged: %w", err) }

// failure describes a local failure for the update notice.
func failure(err error) error {
	if errors.Is(err, syscall.ENOSPC) {
		return errors.New("the disk is full")
	}
	return err
}
