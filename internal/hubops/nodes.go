package hubops

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/meschoening/silkpool/internal/hubstore"
	"github.com/meschoening/silkpool/internal/node"
	"github.com/meschoening/silkpool/internal/pool"
	"github.com/meschoening/silkpool/internal/runnerapp"
	"github.com/meschoening/silkpool/internal/wording"
)

// A Node is a node as node list shows it.
type Node struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// OS and Arch are the node's, as its runners' labels name them, once
	// it has connected. They're empty while UnknownPlatform.
	OS   string `json:"os,omitempty"`
	Arch string `json:"arch,omitempty"`
	// UnknownPlatform reports that its agent stated a platform Silkpool
	// doesn't support. The Hub accepts it, but shows it only as unknown,
	// never as what the agent stated, and mints it no runners.
	UnknownPlatform bool `json:"unknown_platform,omitempty"`
	// State is the node's state as it last reported it, or empty if it
	// never has. While it isn't connected, it's Last known.
	State     string    `json:"state,omitempty"`
	Connected bool      `json:"connected"`
	Seen      time.Time `json:"seen,omitzero"`
	Target    string    `json:"target,omitempty"`
	Labels    []string  `json:"labels,omitempty"`
	// Jobs are the jobs its runners held, as it last reported them.
	Jobs []string `json:"jobs,omitempty"`
	// Removing reports that its removal has started, and Removed when it
	// became a tombstone.
	Removing bool      `json:"removing,omitempty"`
	Removed  time.Time `json:"removed,omitzero"`
}

// Platform returns the node's platform in words, "Windows · x64" or
// "Unknown platform", or empty if it has never connected.
func (n Node) Platform() string {
	if n.UnknownPlatform {
		return wording.UnknownPlatform
	}
	return wording.Platform(n.OS, n.Arch)
}

// A NodeChange is a node after a change, and whether it changed.
type NodeChange struct {
	Node    Node `json:"node"`
	Changed bool `json:"changed"`
}

// Nodes lists the nodes that aren't tombstones, in the order they
// enrolled.
func (o *Ops) Nodes(ctx context.Context) ([]Node, error) {
	h, err := o.state(ctx)
	if err != nil {
		return nil, err
	}
	out := []Node{}
	for _, n := range h.Pool.Nodes {
		if n.Removed.IsZero() {
			out = append(out, nodeOf(h, n))
		}
	}
	return out, nil
}

// Node returns the node with id, tombstones included, which is how a
// removal is followed to its end.
func (o *Ops) Node(ctx context.Context, id string) (Node, error) {
	h, err := o.state(ctx)
	if err != nil {
		return Node{}, err
	}
	n, ok := byID(h, id)
	if !ok {
		return Node{}, refuse("unknown-node", "the Hub has no node with the ID %s", id)
	}
	return nodeOf(h, n), nil
}

// Rename renames the node named name, under node.CheckName's rule, to a
// name no other node has, ignoring case.
func (o *Ops) Rename(ctx context.Context, from node.Origin, name, to string) (NodeChange, error) {
	return o.change(ctx, name, func(h hubstore.Hub, n pool.Node) (hubstore.Event, error) {
		if n.Name == to {
			return nil, nil
		}
		return pool.Rename{Node: n.ID, Name: to, Origin: from}, nil
	})
}

// SetTarget gives the node named name the target written as target.
// Its runners are retired and minted again there, busy ones once their
// jobs end.
func (o *Ops) SetTarget(ctx context.Context, from node.Origin, name, target string) (NodeChange, error) {
	return o.change(ctx, name, func(h hubstore.Hub, n pool.Node) (hubstore.Event, error) {
		t, err := findTarget(h, target)
		if err != nil || n.Target == t.Key {
			return nil, err
		}
		return pool.Retarget{Node: n.ID, Target: t.Key, Origin: from}, nil
	})
}

// AddLabels gives the node named name the custom labels, lowercase, on
// top of those it has. Its runners are retired and minted again with
// them.
func (o *Ops) AddLabels(ctx context.Context, from node.Origin, name string, labels []string) (NodeChange, error) {
	return o.change(ctx, name, func(h hubstore.Hub, n pool.Node) (hubstore.Event, error) {
		have := slices.Clone(h.Labels[n.ID])
		for _, l := range labels {
			l = normal(l)
			if err := CheckLabel(l, h.Settings.PoolLabel); err != nil {
				return nil, err
			}
			if !slices.Contains(have, l) {
				have = append(have, l)
			}
		}
		return o.labels(h, n, have, from), nil
	})
}

// RemoveLabels takes the custom labels from the node named name. A label
// it doesn't have changes nothing.
func (o *Ops) RemoveLabels(ctx context.Context, from node.Origin, name string, labels []string) (NodeChange, error) {
	return o.change(ctx, name, func(h hubstore.Hub, n pool.Node) (hubstore.Event, error) {
		have := slices.DeleteFunc(slices.Clone(h.Labels[n.ID]), func(l string) bool {
			return slices.ContainsFunc(labels, func(r string) bool { return normal(r) == l })
		})
		return o.labels(h, n, have, from), nil
	})
}

// labels returns the event that gives n labels, or nil if it has them.
func (o *Ops) labels(h hubstore.Hub, n pool.Node, labels []string, from node.Origin) hubstore.Event {
	if slices.Equal(labels, h.Labels[n.ID]) {
		return nil
	}
	return hubstore.SetLabels{Node: n.ID, Labels: labels, Origin: from}
}

// RemoveNode starts removing the node named name, with the removal's
// command ID id. A connected node finishes its jobs, wipes itself and
// confirms, and the Hub then makes it a tombstone; one that isn't
// connected is a tombstone at once.
func (o *Ops) RemoveNode(ctx context.Context, from node.Origin, name, id string) (NodeChange, error) {
	return o.change(ctx, name, func(h hubstore.Hub, n pool.Node) (hubstore.Event, error) {
		if n.Removing {
			return nil, nil
		}
		return pool.Remove{Node: n.ID, ID: id, Origin: from}, nil
	})
}

// change finds the node named name and applies the event f makes for it,
// if any, then returns the node as it is.
func (o *Ops) change(ctx context.Context, name string, f func(hubstore.Hub, pool.Node) (hubstore.Event, error)) (NodeChange, error) {
	o.changing.Lock()
	defer o.changing.Unlock()
	h, err := o.state(ctx)
	if err != nil {
		return NodeChange{}, err
	}
	n, err := findNode(h, strings.TrimSpace(name))
	if err != nil {
		return NodeChange{}, err
	}
	e, err := f(h, n)
	if err != nil {
		return NodeChange{}, err
	}
	if e == nil {
		return NodeChange{Node: nodeOf(h, n)}, nil
	}
	if err := o.apply(ctx, e); err != nil {
		return NodeChange{}, err
	}
	if h, err = o.state(ctx); err != nil {
		return NodeChange{}, err
	}
	n, _ = byID(h, n.ID)
	return NodeChange{Node: nodeOf(h, n), Changed: true}, nil
}

// byID returns the node with id, tombstones included.
func byID(h hubstore.Hub, id string) (pool.Node, bool) {
	i := slices.IndexFunc(h.Pool.Nodes, func(n pool.Node) bool { return n.ID == id })
	if i < 0 {
		return pool.Node{}, false
	}
	return h.Pool.Nodes[i], true
}

// nodeOf returns n as node list shows it.
func nodeOf(h hubstore.Hub, n pool.Node) Node {
	out := Node{
		ID: n.ID, Name: n.Name, Connected: n.Connected, Target: n.Target,
		Labels: slices.Clone(h.Labels[n.ID]), Removing: n.Removing,
	}
	if !n.Seen.IsZero() {
		out.Seen = n.Seen.UTC()
	}
	if !n.Removed.IsZero() {
		out.Removed = n.Removed.UTC()
	}
	out.OS, out.Arch = osArch(n.Platform)
	out.UnknownPlatform = unknownPlatform(n.Platform)
	if n.Mirror.Enrolled {
		v := wording.View{Node: n.Mirror, Name: n.Name}
		out.State, out.Jobs = v.State(), v.RunningJobs()
	}
	return out
}

// osArch returns the OS and architecture labels of a platform Silkpool
// supports, or empty ones for none and for any other.
func osArch(p runnerapp.Platform) (string, string) {
	if !slices.Contains(runnerapp.Platforms, p) {
		return "", ""
	}
	osPart, arch, _ := strings.Cut(string(p), "-")
	return map[string]string{"linux": "linux", "win": "windows", "osx": "macos"}[osPart], arch
}

// unknownPlatform reports whether p is a platform a node's agent stated
// that Silkpool doesn't support. The pool keeps any such platform as
// pool.UnknownPlatform, but a Hub upgraded from one that didn't kept it
// as stated, until the node connects again.
func unknownPlatform(p runnerapp.Platform) bool {
	return p != "" && !slices.Contains(runnerapp.Platforms, p)
}
