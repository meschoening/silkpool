package source

import (
	"fmt"
	"slices"
	"time"

	"github.com/meschoening/silkpool/internal/availability"
	"github.com/meschoening/silkpool/internal/node"
	"github.com/meschoening/silkpool/internal/version"
	"github.com/meschoening/silkpool/internal/wire"
)

// A Fixture is a node the fake can show, by name: one in each state the
// panel shows, with the shapes of slots, notices and auto-resume each
// state brings. Its machine, Hub, repositories and people are made up.
type Fixture struct {
	Name string
	// Label names it in the fake's menu.
	Label string
	// node is the node with its times set from at, in its machine's zone.
	node func(at time.Time, zone *time.Location) (node.Node, error)
}

// HubURL is the made-up Hub the fake's node is enrolled in.
const HubURL = "https://build-hub.example.com"

// Machine is the fake's made-up machine on goos: the Mac, or else the
// PC.
func Machine(goos string) wire.Machine {
	if goos == "darwin" {
		return wire.Machine{Hostname: "build-mac", OS: "darwin", OSVersion: "macOS 26.5", Arch: "arm64",
			AgentVersion: version.Version, Zone: "America/Chicago"}
	}
	return wire.Machine{Hostname: "build-pc", OS: "windows", OSVersion: "Windows 11 Pro 25H2", Arch: "amd64",
		AgentVersion: version.Version, Zone: "America/Chicago"}
}

// Find returns the fixture named name.
func Find(name string) (Fixture, bool) {
	i := slices.IndexFunc(Fixtures, func(x Fixture) bool { return x.Name == name })
	if i < 0 {
		return Fixture{}, false
	}
	return Fixtures[i], true
}

// The people and the repository the fixtures' commands and jobs name.
var (
	fromCLI   = node.Origin{Surface: node.CommandLine, Account: "alex"}
	fromTray  = node.Origin{Surface: node.Tray, Account: "alex"}
	fromHub   = node.Origin{Surface: node.Dashboard, Login: "alex-ops"}
	fromAuto  = node.Origin{Surface: node.AutoResume}
	repoScope = node.Scope{Owner: "acme", Repo: "web"}
)

// Fixtures are the nodes the fake can show, in its menu's order.
var Fixtures = []Fixture{
	// The node of a machine that isn't enrolled yet, as a new install's
	// agent reports it: the zero node, which Status gives with no Hub.
	{"not-enrolled", "Not enrolled", func(time.Time, *time.Location) (node.Node, error) {
		return node.Node{}, nil
	}},
	{"new", "Newly enrolled", func(at time.Time, zone *time.Location) (node.Node, error) {
		n, err := node.New(zone)
		if err != nil {
			return node.Node{}, err
		}
		n.Target, n.Scope, n.Connected = "web", repoScope, true
		return n, nil
	}},
	{"available", "Available", func(at time.Time, zone *time.Location) (node.Node, error) {
		n := onDuty(zone)
		n.Slots = []node.Slot{listening(1), listening(2)}
		n.IntentSet = set(at, 30, fromCLI, availability.Resume{Base: 6})
		return n, nil
	}},
	{"busy", "Busy, after an interrupted job", func(at time.Time, zone *time.Location) (node.Node, error) {
		n := onDuty(zone)
		n.Slots = []node.Slot{running(at, 1, "build", 12), listening(2)}
		n.IntentSet = set(at, 12, fromCLI, availability.Resume{Base: 6})
		n.Notices = []node.Notice{{ID: 41, Kind: node.NoticeInterruptedJob, Raised: at.Add(-18 * time.Minute), Slot: 2,
			Job: node.Job{Name: "test", Run: node.Run{Repository: "acme/web", ID: 18_000_123, Number: 95}}}}
		return n, nil
	}},
	{"between-jobs", "Between jobs", func(at time.Time, zone *time.Location) (node.Node, error) {
		n := onDuty(zone)
		n.Availability.SlotLimit = 1
		n.Slots = []node.Slot{{Number: 1, Phase: node.Requesting, RunnerlessSince: at.Add(-4 * time.Second)}}
		n.IntentSet = set(at, 30, fromCLI, availability.Resume{Base: 6})
		return n, nil
	}},
	{"starting", "Starting", func(at time.Time, zone *time.Location) (node.Node, error) {
		n := onDuty(zone)
		n.Availability.SlotLimit = 1
		n.Slots = []node.Slot{{Number: 1, Phase: node.Requesting}}
		return n, nil
	}},
	{"degraded", "Degraded", func(at time.Time, zone *time.Location) (node.Node, error) {
		n := onDuty(zone)
		n.Availability.SlotLimit = 1
		n.Slots = []node.Slot{{Number: 1, Phase: node.Free, Degraded: true, RunnerlessSince: at.Add(-14 * time.Minute),
			Failure: node.Failure{Reason: node.GitHubRefused, Attempts: 6, Since: at.Add(-14 * time.Minute), RetryAt: at.Add(50 * time.Second)}}}
		n.DegradedSince = at.Add(-13 * time.Minute)
		n.IntentSet = set(at, 90, fromAuto, availability.Resume{})
		return n, nil
	}},
	{"draining", "Draining", func(at time.Time, zone *time.Location) (node.Node, error) {
		n := onDuty(zone)
		n.Availability.Intent = availability.Paused
		n.Availability.ResumeAt = n.Availability.AutoResume.Next(at)
		n.Slots = []node.Slot{running(at, 1, "build", 31),
			{Number: 2, Phase: node.Hosting, Runner: node.Runner{Name: "build-2-cd32", Listening: true, Retirement: node.Retiring}}}
		n.IntentSet = set(at, 5, fromHub, availability.Pause{})
		return n, nil
	}},
	{"exposed", "Paused, with a slot exposed", func(at time.Time, zone *time.Location) (node.Node, error) {
		n := onDuty(zone)
		n.Availability.Intent = availability.PausedIndefinitely
		n.Availability.AutoResume.On = false
		detail := "it can open /srv/backups, which is readable by every account. Make it unreadable to others, or move it under a hidden folder"
		n.Slots = []node.Slot{{Number: 1, Phase: node.Free, Failure: node.Failure{Reason: node.SlotExposed, Attempts: 1, Since: at.Add(-time.Hour), Detail: detail}}}
		n.Notices = []node.Notice{{ID: 39, Kind: node.NoticeSlotExposed, Raised: at.Add(-time.Hour), Slot: 1, Detail: detail}}
		n.IntentSet = set(at, 60, fromTray, availability.TurnOffAutoResume{})
		return n, nil
	}},
	{"stopped", "Stopped now", func(at time.Time, zone *time.Location) (node.Node, error) {
		n := onDuty(zone)
		n.Availability.Intent = availability.PausedIndefinitely
		n.Slots = []node.Slot{{Number: 1, Phase: node.Resetting}}
		n.IntentSet = set(at, 2, fromTray, availability.EmergencyStop{}, node.CancelledJob{Slot: 1, Runner: "build-1-ab11", Job: "build"})
		return n, nil
	}},
	{"hub-offline", "Hub offline", func(at time.Time, zone *time.Location) (node.Node, error) {
		n := onDuty(zone)
		n.Slots = []node.Slot{running(at, 1, "build", 20), listening(2)}
		n.Connected = false
		return n, nil
	}},
}

// onDuty is a node On duty, connected, with two slots, and auto-resume
// at 7:30 AM on weekdays.
func onDuty(zone *time.Location) node.Node {
	return node.Node{
		Enrolled: true,
		Availability: availability.Node{
			Intent: availability.OnDuty,
			AutoResume: availability.AutoResume{On: true, Time: availability.TimeOfDay{Hour: 7, Minute: 30},
				Days: availability.DaysOf(time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday), Zone: zone},
			SlotLimit: 2,
			Revision:  7,
		},
		Target:    "web",
		Scope:     repoScope,
		Connected: true,
	}
}

// set is the entry of a command from origin, applied minutesAgo before
// at, which set the intent.
func set(at time.Time, minutesAgo int, origin node.Origin, e availability.Event, cancelled ...node.CancelledJob) node.Entry {
	return node.Entry{Seq: 40, At: at.Add(-time.Duration(minutesAgo) * time.Minute),
		What: node.AppliedCommand{ID: "earlier", Origin: origin, Event: e, Cancelled: cancelled}}
}

// running is slot number hosting a runner whose job started minutes
// before at.
func running(at time.Time, number int, job string, minutes int) node.Slot {
	return node.Slot{Number: number, Phase: node.Hosting, Runner: node.Runner{
		Name: fmt.Sprintf("build-%d-ab1%d", number, number), Listening: true, Version: "2.338.2",
		Job: job, JobSince: at.Add(-time.Duration(minutes) * time.Minute)}}
}

// listening is slot number with a runner listening for a job.
func listening(number int) node.Slot {
	return node.Slot{Number: number, Phase: node.Hosting, Runner: node.Runner{
		Name: fmt.Sprintf("build-%d-cd3%d", number, number), Listening: true, Version: "2.338.2"}}
}
