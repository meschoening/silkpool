// Package ui is Silkpool's shared UI package, as ADR 0009 and "Choose
// the desktop shell and dashboard frontend stack"
// decided: the Go templates of what the Hub's dashboard and the tray app
// both show of a node, the token CSS, and the live-update script. The
// script morphs the parts a surface pushes into its page: the dashboard's
// server-sent events, or the tray app's Wails events, which its page
// hands to silkpool.apply(html). The look
// is direction A, "Quiet", from "Set the design direction for
// Silkpool's interfaces":
// grey by default, amber only for Degraded and red only for Stop now,
// motion only while a change is in progress, and light and dark
// following the OS.
//
// The templates are the parts: "glyph", "chip", "sentence", "slots",
// "notices", "auto-resume" and "auto-resume-editor". Each surface clones
// Templates, adds its own pages around them, and fills them: the Hub
// from its node mirror, the tray app from the agent's status stream.
// The words come from package wording, so the CLI, the tray app and the
// dashboard can't drift.
//
// It holds no Hub code: it imports only package node and the packages
// that write its words, so nodes never carry the dashboard.
package ui

import (
	"embed"
	"html/template"
	"io/fs"
	"strings"
	"sync"
	"time"

	"github.com/meschoening/silkpool/internal/availability"
	"github.com/meschoening/silkpool/internal/node"
	"github.com/meschoening/silkpool/internal/wording"
)

//go:embed templates/*.tmpl
var templateFiles embed.FS

//go:embed assets
var assetFiles embed.FS

// StyleSheet is the token CSS's path in Assets, and Script the
// live-update script's.
const (
	StyleSheet = "silkpool.css"
	Script     = "live.js"
)

// Assets are the files a page loads beside its HTML: the token CSS and
// the live-update script.
func Assets() fs.FS {
	sub, err := fs.Sub(assetFiles, "assets")
	if err != nil {
		panic(err)
	}
	return sub
}

var parsed = sync.OnceValue(func() *template.Template {
	return template.Must(template.New("ui").Funcs(Funcs).ParseFS(templateFiles, "templates/*.tmpl"))
})

// Templates returns a fresh copy of the shared templates, for a surface
// to add its pages to. Its pages can use Funcs too.
func Templates() *template.Template { return template.Must(parsed().Clone()) }

// Funcs are the functions the shared templates use.
var Funcs = template.FuncMap{
	// glyph is a glyph to draw: its name, as Glyph returns one, and its
	// size in pixels.
	"glyph": func(name string, size int) Glyph { return Glyph{Name: name, Size: size} },
	// sentence ends a line of words as a sentence.
	"sentence": wording.Sentence,
	"join":     strings.Join,
	"weekdays": func() any { return Weekdays },
	// autoResume is auto-resume's settings in one line.
	"autoResume": wording.AutoResume,
	// zoneName is a zone's IANA name, or empty.
	"zoneName": func(z *time.Location) string {
		if z == nil {
			return ""
		}
		return z.String()
	},
}

// A Glyph is a state's glyph, by name, drawn at Size pixels: available
// (a solid dot), busy (a dot in a ring), draining (the ring empties),
// drained (an empty ring), starting (a dotted ring that turns), degraded
// (an amber disc with "!") or lastknown (a dashed ring).
type Glyph struct {
	Name string
	Size int
}

// The glyphs' names.
const (
	GlyphAvailable = "available"
	GlyphBusy      = "busy"
	GlyphDraining  = "draining"
	GlyphDrained   = "drained"
	GlyphStarting  = "starting"
	GlyphDegraded  = "degraded"
	GlyphLastKnown = "lastknown"
)

// stateGlyphs are each state's glyph.
var stateGlyphs = map[node.State]string{
	node.Available: GlyphAvailable,
	node.Busy:      GlyphBusy,
	node.Draining:  GlyphDraining,
	node.Drained:   GlyphDrained,
	node.Starting:  GlyphStarting,
	node.Degraded:  GlyphDegraded,
}

// A Node is a node as the shared templates show it: its words, and
// what the surface knows of it.
type Node struct {
	wording.View
	// LastKnown reports that the words come from the node's last report
	// before the surface lost it, as the dashboard has them of a node
	// that isn't connected: its glyph is the dashed ring, and the rest is
	// greyed. Since is when it was lost.
	LastKnown bool
	Since     time.Time
	// Post is the path the node's forms post to: Post + "/dismiss" for a
	// notice, Post + "/auto-resume" for auto-resume.
	Post string
	// Locked reports that the node can't be sent a command now, so its
	// controls are disabled.
	Locked bool
}

// Glyph returns the glyph of the node's state as it's shown, or the
// last-known one.
func (n Node) Glyph() string {
	if n.LastKnown {
		return GlyphLastKnown
	}
	return stateGlyphs[n.Shown()]
}

// Tone returns the class the node's state word is shown with:
// "degraded" in amber, "lastknown" and "drained" in grey, or empty.
func (n Node) Tone() string {
	switch {
	case n.LastKnown:
		return "lastknown"
	case n.Shown() == node.Degraded:
		return "degraded"
	case n.Shown() == node.Drained:
		return "drained"
	}
	return ""
}

// A Row is a slot's row, with its glyph.
type Row struct {
	wording.Row
	Glyph string
}

// Rows returns the node's slot rows, each with its slot's glyph, which
// reuses the node states' glyphs.
func (n Node) Rows() []Row {
	var rows []Row
	for _, r := range n.View.Rows() {
		row := Row{Row: r, Glyph: GlyphStarting}
		for _, s := range n.Node.Slots {
			if s.Number != r.Slot {
				continue
			}
			row.Glyph = slotGlyph(s, r)
			// How long a job has run isn't known of a last-known report.
			if n.LastKnown && r.Right != "" && s.Runner.Job != "" {
				row.Right = n.Clock.Started(s.Runner.JobSince)
			}
		}
		rows = append(rows, row)
	}
	return rows
}

// slotGlyph returns the glyph of slot s, whose row is r: busy while it
// runs a job, available while it listens, starting while a runner gets
// ready or the slot resets, degraded once it's failing past its grace,
// draining while a runner retires or a job is cancelled or the slot's
// account is deleted, and drained once it's free with nothing to do.
func slotGlyph(s node.Slot, r wording.Row) string {
	run := s.Runner
	switch {
	case r.Amber || s.Degraded:
		return GlyphDegraded
	case s.Phase == node.Hosting && run.Job != "" && (run.Retirement == node.Cancelling || run.Retirement == node.StoppingLost):
		return GlyphDraining
	case s.Phase == node.Hosting && run.Job != "":
		return GlyphBusy
	case s.Phase == node.Hosting && run.Retirement != node.NotRetiring:
		return GlyphDraining
	case s.Phase == node.Hosting && run.Listening:
		return GlyphAvailable
	case s.Phase == node.DeletingAccount || s.Phase == node.RetryingDelete:
		return GlyphDraining
	case s.Phase == node.Deleted || r.Main == "No runner":
		return GlyphDrained
	}
	return GlyphStarting
}

// An Editor is the auto-resume editor, open with a draft of the
// settings: its time, days and zone, a preview of what the draft means,
// and the zones it offers. Revision is the node's revision the draft is
// based on, which the edit is refused without.
type Editor struct {
	Post     string
	Draft    availability.AutoResume
	Preview  string
	Zones    []string
	Revision int64
}

// Weekdays are the editor's day toggles, Monday first: the day, and its
// letter.
var Weekdays = []struct {
	Day    time.Weekday
	Name   string
	Letter string
}{
	{time.Monday, "Monday", "M"}, {time.Tuesday, "Tuesday", "T"}, {time.Wednesday, "Wednesday", "W"}, {time.Thursday, "Thursday", "T"},
	{time.Friday, "Friday", "F"}, {time.Saturday, "Saturday", "S"}, {time.Sunday, "Sunday", "S"},
}
