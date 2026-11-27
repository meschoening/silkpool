package icon

// A Face is what the tray icon shows: its state's glyph, and its tooltip.
type Face struct {
	Glyph, Tooltip string
}

// A Setter is a tray icon, as Wails' SystemTray is: SetImage sets its
// image to an Icon's, as the OS shows it, and SetTooltip its tooltip.
type Setter interface {
	SetImage(Icon)
	SetTooltip(string)
}

// Shown is the Face a tray icon was last sent. Its zero value has sent
// nothing.
type Shown struct {
	face Face
	sent bool
}

// Show sends s what of f differs from what it was last sent, or all of
// it if all. The tooltip goes after the image, and again after each
// image: Windows hides the tooltip of an icon whose image changes
// without it. Wails v3.0.0 registers the icon with NOTIFYICON_VERSION_4,
// which hides the standard tooltip unless a call says NIF_SHOWTIP, and
// it changes the image with a call that says only NIF_ICON (#364).
func (sh *Shown) Show(s Setter, f Face, all bool) {
	all = all || !sh.sent
	image := all || f.Glyph != sh.face.Glyph
	tooltip := image || f.Tooltip != sh.face.Tooltip
	sh.face, sh.sent = f, true
	if image {
		s.SetImage(For(f.Glyph))
	}
	if tooltip {
		s.SetTooltip(f.Tooltip)
	}
}
