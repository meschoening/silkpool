package wording

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// The tray panel's words for what only the tray shows, from "Prototype
// the tray and menu-bar panel": its footer's Hub line, and the icon's
// tooltip.

// TrayHub is the tray panel's Hub line, from the URL of the Hub the node
// is enrolled in: "Hub · build-hub" while the node is connected, or
// "Hub offline for 12 min" and what that means while it isn't. The
// sentence doesn't mention the Hub, so the panel says it only here.
func (v View) TrayHub(hubURL string) (line, detail string) {
	if !v.Node.Connected {
		return "Hub offline for " + Duration(v.Clock.Now.Sub(v.HubSince)),
			"Running jobs carry on. A finished runner can't be replaced until it's back."
	}
	if name := hubName(hubURL); name != "" {
		return "Hub · " + name, ""
	}
	return "Hub", ""
}

// hubName is the Hub's name as the panel shows it: its host's first
// label, as a machine's name is shown, or its address when it's reached
// by one. It's empty if the URL names no host.
func hubName(hubURL string) string {
	u, err := url.Parse(hubURL)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	host := u.Hostname()
	if net.ParseIP(host) != nil {
		return host
	}
	name, _, _ := strings.Cut(host, ".")
	return name
}

// TrayTooltip is the tray icon's tooltip: "Silkpool · build-pc · Busy".
func (v View) TrayTooltip() string {
	return fmt.Sprintf("Silkpool · %s · %s", v.Name, v.State())
}

// A TrayDown is the tray panel while it can't read the agent, from
// "Prototype the tray and menu-bar panel" and "Connect the tray app to
// the agent's control channel": the word its chip and tooltip show
// beside the dashed ring, which isn't a node state, the sentence and what
// it means, and the footer's line.
type TrayDown struct {
	State, Lead, Sub, Foot string
}

// trayHere is the machine as the tray names it.
func trayHere(mac bool) string {
	if mac {
		return "this Mac"
	}
	return "this PC"
}

// trayStuck says what the tray can't do while it can't reach the agent.
const trayStuck = "Jobs already running carry on, but nothing can be paused, resumed or stopped from here until it's back."

// trayChecking is the footer while the tray dials the agent again.
const trayChecking = "Checking again every few seconds"

// TrayNotRunning is the panel while the agent's service isn't running.
func TrayNotRunning(mac bool) TrayDown {
	starts := "It starts with Windows, or start the Silkpool service in Services."
	if mac {
		starts = "It starts with the Mac."
	}
	return TrayDown{State: "Not running", Lead: fmt.Sprintf("Silkpool's service isn't running on %s.", trayHere(mac)),
		Sub: trayStuck + " " + starts, Foot: trayChecking}
}

// TrayNotAdmin is the panel when the agent's peer check refused the
// tray's account, which isn't an administrator's (ADR 0008).
func TrayNotAdmin(mac bool, account string) TrayDown {
	return TrayDown{State: "Needs an administrator account",
		Lead: fmt.Sprintf("Only administrators can use Silkpool on %s, and %s isn't one.", trayHere(mac), account),
		Sub:  "Sign in to an administrator account to see what it's doing and change it.", Foot: trayChecking}
}

// TrayNotTheService is the panel when something other than the agent
// holds its endpoint, at path, so the tray won't talk to it.
func TrayNotTheService(path string) TrayDown {
	return TrayDown{State: "Can't connect",
		Lead: fmt.Sprintf("Something other than Silkpool's service is listening at %s, so the tray won't talk to it.", path),
		Sub:  trayStuck, Foot: trayChecking}
}

// TrayCantAsk is the panel when the tray couldn't read the agent for
// another reason, err.
func TrayCantAsk(err error) TrayDown {
	return TrayDown{State: "Can't connect", Lead: Sentence(fmt.Sprintf("couldn't ask Silkpool's service: %v", err)),
		Sub: trayStuck, Foot: trayChecking}
}

// TrayTooltipDown is the tray icon's tooltip while it can't read the
// agent on the machine named name: "Silkpool · build-pc · Not running".
func TrayTooltipDown(name string, d TrayDown) string {
	return fmt.Sprintf("Silkpool · %s · %s", name, d.State)
}

// TrayRefused says the agent refused a command from the panel: a stale
// one, based on what the panel showed, which has changed since, says so,
// as the CLI's does; any other says why.
func (v View) TrayRefused(code, message string) string {
	if code == "stale" {
		return fmt.Sprintf("Not applied: %s changed since the panel showed it. Check it, then try again.", v.Name)
	}
	return fmt.Sprintf("Refused by %s: %s", v.Name, Sentence(message))
}

// TrayNotConfirmed says a command from the panel may or may not have
// applied: its connection dropped before the agent answered, twice, as
// the CLI's Not confirmed has it. The panel shows which, once it reads
// the agent again.
const TrayNotConfirmed = "Not confirmed: the connection to Silkpool's service dropped before it answered, twice. The panel shows whether it applied."

// TrayOtherVersion says the panel sent nothing to an agent of another
// version than the tray's, as the CLI sends nothing.
func TrayOtherVersion(tray, service string) string {
	return fmt.Sprintf("Not sent: this tray app is %s but the running service is %s. Quit the tray app and open it again.", tray, service)
}

// TrayCouldntSend says the panel couldn't send a command, because of err.
func TrayCouldntSend(err error) string {
	return Sentence(fmt.Sprintf("not sent: couldn't ask Silkpool's service: %v", err))
}
