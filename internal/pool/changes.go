package pool

import (
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/meschoening/silkpool/internal/node"
)

// The Hub's settings changes, which the operator makes from the
// dashboard or the Hub's CLI, as "Define Silkpool's events, audit trail,
// notices and redaction"
// lists them. Each writes one entry with the origin it came from, and a
// change to what's already set writes none.

// CodeMade records that the operator made the enrollment code whose ID
// is Code. The ID isn't secret, so it enrolls nothing.
type CodeMade struct{ Code string }

// CodeRevoked records that the operator revoked the unused enrollment
// code whose ID is Code.
type CodeRevoked struct{ Code string }

// AddedTarget records that the operator added Target. A change of its
// scope writes none.
type AddedTarget struct{ Target string }

// RemovedTarget records that the operator removed Target.
type RemovedTarget struct{ Target string }

// RenameTaken refuses renaming a node to a name another node has,
// ignoring case, and suggests a free one as NameTaken does for an
// enrollment. A tombstone's name is free.
type RenameTaken struct{ Name, Suggestion string }

func (e RenameTaken) Error() string {
	return fmt.Sprintf("the name %s is taken: rename the node %s, or another name", e.Name, e.Suggestion)
}

// rename renames n, and writes it, unless it's a tombstone or already
// has the name. A name that breaks the rule or is taken is refused.
func (a *applying) rename(n *Node, e Rename) {
	if !n.Removed.IsZero() || n.Name == e.Name {
		return
	}
	if err := node.CheckName(e.Name); err != nil {
		a.emit(ChangeRefused{Reason: err})
		return
	}
	if a.p.taken(e.Name, n.ID) {
		a.emit(ChangeRefused{Reason: RenameTaken{Name: e.Name, Suggestion: a.p.suggest(e.Name, n.ID)}})
		return
	}
	a.store(Entry{Node: n.ID, At: a.now, Origin: e.Origin, What: Renamed{From: n.Name, To: e.Name}})
	n.Name = e.Name
}

// Renamed records that the operator renamed the entry's node From one
// name To another.
type Renamed struct{ From, To string }

// The Hub's store keeps a node's custom labels, the pool label and the
// GitHub connections, so it writes their entries, in these kinds.

// LabelsChanged records that the operator changed the entry's node's
// custom labels From one set To another.
type LabelsChanged struct{ From, To []string }

// PoolLabelChanged records that the operator changed the pool label, the
// label every runner of the Hub carries, From one To another, or to its
// first.
type PoolLabelChanged struct{ From, To string }

// GitHubConnectionAdded records that the operator added a GitHub
// connection: the private App of the GitHub account Account, installed
// on it.
type GitHubConnectionAdded struct{ Account string }

// UnknownTarget refuses giving a node a target the Hub doesn't hold.
type UnknownTarget struct{ Target string }

func (e UnknownTarget) Error() string {
	return fmt.Sprintf("the Hub has no target %q: add it first", e.Target)
}

// retarget gives n its new target, sends it its Hub settings if it's
// connected, retires its runners, and writes it, unless n is a tombstone
// or already has the target. A target the pool doesn't hold is refused.
func (a *applying) retarget(n *Node, e Retarget) {
	if !n.Removed.IsZero() || n.Target == e.Target {
		return
	}
	if _, ok := a.p.Targets[e.Target]; !ok {
		a.emit(ChangeRefused{Reason: UnknownTarget{Target: e.Target}})
		return
	}
	a.store(Entry{Node: n.ID, At: a.now, Origin: e.Origin, What: Retargeted{From: n.Target, To: e.Target}})
	n.Target = e.Target
	if n.Connected {
		a.emit(Send{Node: n.ID, Message: a.p.settings(*n)})
	}
	a.emit(RetireRunners{Node: n.ID})
}

// Retargeted records that the operator moved the entry's node From one
// target To another, or to its first.
type Retargeted struct{ From, To string }

// Sign-ins to the dashboard are written too, under the Connections
// filter: every one of the operator's, and a refused one, by a GitHub
// user who isn't the operator, at most once per login every
// SignInRefusedEvery, however often they retry.
const SignInRefusedEvery = time.Hour

// SignedIn records that the operator signed in to the dashboard, from
// the remote address Address. The entry's origin holds their GitHub
// login.
type SignedIn struct{ Address string }

// SignInRefused records that the Hub refused a sign-in to the dashboard
// by the GitHub user Login, who isn't the operator, from the remote
// address Address. It never signed in, so the entry's origin holds no
// login.
type SignInRefused struct{ Login, Address string }

// refuseSignIn writes a refused sign-in, unless the Hub wrote one for
// its login within SignInRefusedEvery.
func (a *applying) refuseSignIn(e RefuseSignIn) {
	login := strings.ToLower(e.Login)
	if _, ok := a.p.SignInsRefused[login]; ok {
		return
	}
	a.p.SignInsRefused = maps.Clone(a.p.SignInsRefused)
	if a.p.SignInsRefused == nil {
		a.p.SignInsRefused = map[string]time.Time{}
	}
	a.p.SignInsRefused[login] = a.now
	a.store(Entry{At: a.now, Origin: node.Origin{Surface: node.Dashboard}, What: SignInRefused(e)})
}

// forgetSignIns drops the refused sign-ins written SignInRefusedEvery
// ago or longer, which no longer hold back an entry.
func (p *Pool) forgetSignIns(now time.Time) {
	old := func(_ string, at time.Time) bool { return !now.Before(at.Add(SignInRefusedEvery)) }
	for login, at := range p.SignInsRefused {
		if old(login, at) {
			p.SignInsRefused = maps.Clone(p.SignInsRefused)
			maps.DeleteFunc(p.SignInsRefused, old)
			if len(p.SignInsRefused) == 0 {
				p.SignInsRefused = nil
			}
			return
		}
	}
}
