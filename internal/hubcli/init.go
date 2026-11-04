package hubcli

import (
	"context"
	"errors"
	"fmt"

	"github.com/meschoening/silkpool/internal/hubops"
	"github.com/meschoening/silkpool/internal/hubservice"
	"github.com/meschoening/silkpool/internal/node"
	"github.com/meschoening/silkpool/internal/wording"
)

// runInit sets the Hub up, the one command that works before its service
// runs: it writes the Hub's settings, makes its key and certificate or
// hands it the operator's, and starts it. Its flags pick the listener's
// mode, which is fixed afterwards.
func runInit(ctx context.Context, s *session, args []string) int {
	const synopsis = "silkpool-hub init --url <url> [--cert <file> --key <file> | --behind-proxy] [--listen <addr:port>] [--json]"
	fs := s.flags("init", synopsis)
	url := fs.String("url", "", "")
	cert := fs.String("cert", "", "")
	key := fs.String("key", "", "")
	proxy := fs.Bool("behind-proxy", false, "")
	listen := fs.String("listen", "", "")
	if _, code, ok := s.parse(fs, args, 0, 0); !ok {
		return code
	}
	switch {
	case *url == "":
		return s.usage(fs, "init needs --url, the Hub's https:// URL")
	case (*cert == "") != (*key == ""):
		return s.usage(fs, "--cert and --key go together")
	case *proxy && *cert != "":
		return s.usage(fs, "--behind-proxy serves plain HTTP, so it takes no certificate files")
	}
	if !s.env.Elevated {
		return s.say("not-elevated", "init sets up the Hub's service, so run it with sudo.", ExitCantAsk)
	}
	setUp, err := hubservice.Init(ctx, hubservice.InitConfig{
		Root: s.env.Root, URL: *url, Cert: *cert, Key: *key, BehindProxy: *proxy, Listen: *listen,
		Origin: node.Origin{Surface: node.HubCommandLine, Account: "root"}, Now: s.env.Now(),
		Systemctl: s.env.Systemctl, NewSecret: s.env.NewSecret,
	})
	var refusal *hubops.Refusal
	var notStarted *hubservice.NotStarted
	switch {
	case errors.As(err, &refusal):
		return s.say(refusal.Code, wording.Sentence(refusal.Message), ExitRefused)
	case errors.As(err, &notStarted):
		s.printSetUp(notStarted.SetUp, *cert, *key, false)
		return s.say("not-started", fmt.Sprintf("The Hub is set up, but systemctl couldn't start it (%v). Start it with: sudo systemctl enable --now silkpool-hub", notStarted.Err), ExitCantAsk)
	case err != nil:
		return s.say("failed", wording.Sentence("init couldn't set the Hub up: "+err.Error()), ExitCantAsk)
	}
	s.printSetUp(setUp, *cert, *key, true)
	return ExitOK
}

// printSetUp prints what init did, and the setup link.
func (s *session) printSetUp(u hubservice.SetUp, cert, key string, started bool) {
	if s.json {
		s.printJSON(u)
		return
	}
	s.out("The Hub is set up at %s, %s", u.URL, listenerWords(u.Listener, cert, key))
	if u.Fingerprint != "" {
		s.out("Its certificate's SHA-256 fingerprint, for your browser to trust:")
		s.out("  %s", u.Fingerprint)
	}
	if started {
		s.out("Started its service: systemctl enable --now silkpool-hub")
	}
	s.out("")
	s.setupLink(u.Link.Reveal(), hubops.SetupLink{Expires: u.Expires})
}
