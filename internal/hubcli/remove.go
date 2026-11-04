package hubcli

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/meschoening/silkpool/internal/hubops"
	"github.com/meschoening/silkpool/internal/hubsocket"
	"github.com/meschoening/silkpool/internal/wording"
)

// runNodeRemove removes a node, once the operator confirms, in the
// dashboard's words for a node that's connected or one that isn't, then
// follows the removal until the node is a tombstone. Ctrl+C only stops
// watching.
func runNodeRemove(ctx context.Context, s *session, args []string) int {
	fs := s.flags("node remove", "silkpool-hub node remove <name> [--yes] [--json]")
	yes := fs.Bool("yes", false, "")
	rest, code, ok := s.parse(fs, args, 1, 1)
	if !ok {
		return code
	}
	name := rest[0]
	asJSON := s.json
	s.json = false
	var l struct{ Nodes []hubops.Node }
	if code := s.ask(ctx, hubsocket.Request{Op: "node.list"}, &l); code != ExitOK {
		return code
	}
	i := slices.IndexFunc(l.Nodes, func(n hubops.Node) bool { return strings.EqualFold(n.Name, name) })
	if i >= 0 && !*yes {
		n := l.Nodes[i]
		if !asJSON {
			s.out("%s", wording.HubRemoveConfirm(n.Name, n.Connected, n.Jobs))
		}
		if ok, code := s.confirm("node remove", wording.RemoveNodeQuestion(n.Name)); !ok {
			return code
		}
	}
	s.json = asJSON
	var r hubops.NodeChange
	if code := s.ask(ctx, hubsocket.Request{Op: "node.remove", Name: name, ID: s.env.NewID()}, &r); code != ExitOK || s.json {
		return code
	}
	return s.follow(ctx, r.Node)
}

// follow prints how far n's removal goes, each step once, until n is a
// tombstone.
func (s *session) follow(ctx context.Context, n hubops.Node) int {
	if !n.Removed.IsZero() {
		s.out("%s", wording.Removed(n.Name))
		return ExitOK
	}
	s.out("%s", wording.Removing(n.Name))
	last := ""
	for n.Removed.IsZero() {
		if step := wording.RemovalStep(n.State, n.Jobs); step != last {
			s.out("%s", step)
			last = step
		}
		if err := s.env.Wait(ctx); err != nil {
			s.out("%s", wording.StoppedWatching)
			return ExitOK
		}
		var got struct{ Node hubops.Node }
		if code := s.ask(ctx, hubsocket.Request{Op: "node.get", ID: n.ID}, &got); code != ExitOK {
			if errors.Is(ctx.Err(), context.Canceled) {
				s.out("%s", wording.StoppedWatching)
				return ExitOK
			}
			return code
		}
		n = got.Node
	}
	s.out("%s", wording.Removed(n.Name))
	return ExitOK
}
