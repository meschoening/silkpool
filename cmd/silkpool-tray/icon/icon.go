// Package icon draws the tray and menu-bar icon of each state, from the
// shared UI package's glyphs: a still frame of each, since the icon
// changes with the state and never moves.
//
// On macOS every icon is a template, black with alpha, which the menu
// bar tints to match itself, except Degraded's amber disc, the one
// colored state, as "Prototype the tray and menu-bar panel" decided.
// Windows gets a dark glyph for a light taskbar and a white one for a
// dark taskbar.
//
// It doesn't import Wails, so its tests run in the default lane.
package icon

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math"
	"sync"

	"github.com/meschoening/silkpool/internal/ui"
)

// An Icon is one state's icon, as PNGs.
type Icon struct {
	// Mac is the menu-bar icon: 44 px, a 16 pt glyph in the 22 pt status
	// item at 2x. Template reports that it's a template.
	Mac      []byte
	Template bool
	// Light is the notification-area icon for a light taskbar, and Dark
	// for a dark one: 32 px.
	Light, Dark []byte
}

// Glyphs are the glyphs an icon is drawn for: each state's, and the
// dashed ring of a node the tray can't read.
var Glyphs = []string{
	ui.GlyphAvailable, ui.GlyphBusy, ui.GlyphDraining, ui.GlyphDrained, ui.GlyphStarting, ui.GlyphDegraded, ui.GlyphLastKnown,
}

// For returns glyph's icon, or the dashed ring's for a glyph it doesn't
// know.
func For(glyph string) Icon {
	icons := drawn()
	if i, ok := icons[glyph]; ok {
		return i
	}
	return icons[ui.GlyphLastKnown]
}

// The colors: the template's black, the inks of the token CSS, and its
// amber in light and dark.
var (
	black      = rgba{0, 0, 0, 1}
	white      = rgba{1, 1, 1, 1}
	ink        = hex(0x1C1C1E)
	amberLight = hex(0xB86A0B)
	amberDark  = hex(0xF0AE4E)
	onAmber    = hex(0x1A1206)
)

var drawn = sync.OnceValue(func() map[string]Icon {
	icons := map[string]Icon{}
	for _, g := range Glyphs {
		icons[g] = Icon{
			// The menu bar is light or dark, so the amber is the brighter
			// one, which reads on both.
			Mac:      draw(44, 32, shapes(g, black, amberDark, onAmber)),
			Template: g != ui.GlyphDegraded,
			Light:    draw(32, 30, shapes(g, ink, amberLight, white)),
			Dark:     draw(32, 30, shapes(g, white, amberDark, onAmber)),
		}
	}
	return icons
})

type rgba struct{ r, g, b, a float64 }

func hex(h uint32) rgba {
	return rgba{float64(h>>16&0xff) / 255, float64(h>>8&0xff) / 255, float64(h&0xff) / 255, 1}
}

// A shape is part of a glyph: the color at a point of its 16-unit box,
// or false outside it.
type shape func(x, y float64) (rgba, bool)

func disc(cx, cy, r float64, c rgba) shape {
	return func(x, y float64) (rgba, bool) { return c, math.Hypot(x-cx, y-cy) <= r }
}

// ring is a circle of radius r stroked width wide, centred in the box.
// dash and gap, along the circumference, dash it, starting at 12 o'clock
// and running clockwise; upTo, if not 0, keeps only that fraction of it.
func ring(r, width, dash, gap, upTo float64, c rgba) shape {
	circ := 2 * math.Pi * r
	if dash > 0 {
		// A whole number of dashes, so there's no seam at the top.
		k := circ / math.Round(circ/(dash+gap)) / (dash + gap)
		dash, gap = dash*k, gap*k
	}
	return func(x, y float64) (rgba, bool) {
		if math.Abs(math.Hypot(x-8, y-8)-r) > width/2 {
			return c, false
		}
		a := math.Atan2(x-8, -(y - 8))
		if a < 0 {
			a += 2 * math.Pi
		}
		s := a / (2 * math.Pi) * circ
		if upTo > 0 && s > upTo*circ {
			return c, false
		}
		if dash > 0 && math.Mod(s, dash+gap) > dash {
			return c, false
		}
		return c, true
	}
}

// capsule is a line from (x1,y1) to (x2,y2) with round ends.
func capsule(x1, y1, x2, y2, r float64, c rgba) shape {
	return func(x, y float64) (rgba, bool) {
		dx, dy := x2-x1, y2-y1
		t := max(0, min(1, ((x-x1)*dx+(y-y1)*dy)/(dx*dx+dy*dy)))
		return c, math.Hypot(x-(x1+t*dx), y-(y1+t*dy)) <= r
	}
}

func faded(c rgba, a float64) rgba {
	c.a *= a
	return c
}

// shapes are glyph's, as the shared UI package's glyph template draws
// it, in fg, with Degraded's disc in amber and its mark in onAmber.
// Draining is drawn halfway through emptying.
func shapes(glyph string, fg, amber, onAmber rgba) []shape {
	switch glyph {
	case ui.GlyphAvailable:
		return []shape{disc(8, 8, 4.5, fg)}
	case ui.GlyphBusy:
		return []shape{ring(6, 1.5, 0, 0, 0, fg), disc(8, 8, 3, fg)}
	case ui.GlyphDraining:
		return []shape{ring(6, 1.5, 0, 0, 0, faded(fg, .3)), ring(6, 1.5, 0, 0, .5, fg), disc(8, 8, 3, fg)}
	case ui.GlyphDrained:
		return []shape{ring(6, 1.5, 0, 0, 0, fg)}
	case ui.GlyphStarting:
		return []shape{ring(6, 1.5, 2.2, 3, 0, fg)}
	case ui.GlyphDegraded:
		return []shape{disc(8, 8, 7, amber), capsule(8, 4.45, 8, 8.15, .85, onAmber), disc(8, 11.5, 1.05, onAmber)}
	}
	return []shape{ring(6, 1.5, 1.4, 2.6, 0, fg)}
}

// draw renders shapes as a size by size PNG, the 16-unit box scaled to
// glyph pixels and centred, with 4x4 samples a pixel.
func draw(size, glyph int, shapes []shape) []byte {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	scale := float64(glyph) / 16
	off := float64(size-glyph) / 2
	const ss = 4
	for py := range size {
		for px := range size {
			var r, g, b, a float64
			for sy := range ss {
				for sx := range ss {
					x := (float64(px) + (float64(sx)+.5)/ss - off) / scale
					y := (float64(py) + (float64(sy)+.5)/ss - off) / scale
					// Each shape over the last, premultiplied.
					var cr, cg, cb, ca float64
					for _, s := range shapes {
						c, ok := s(x, y)
						if !ok {
							continue
						}
						cr = c.r*c.a + cr*(1-c.a)
						cg = c.g*c.a + cg*(1-c.a)
						cb = c.b*c.a + cb*(1-c.a)
						ca = c.a + ca*(1-c.a)
					}
					r, g, b, a = r+cr, g+cg, b+cb, a+ca
				}
			}
			n := float64(ss * ss)
			r, g, b, a = r/n, g/n, b/n, a/n
			if a > 0 {
				r, g, b = r/a, g/a, b/a
			}
			img.SetNRGBA(px, py, color.NRGBA{u8(r), u8(g), u8(b), u8(a)})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

func u8(v float64) uint8 { return uint8(math.Round(max(0, min(1, v)) * 255)) }
