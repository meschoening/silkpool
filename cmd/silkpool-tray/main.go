// Command silkpool-tray is Silkpool's tray app on Windows and macOS (ADR
// 0009): an icon in the notification area or the menu bar that shows the
// node's state, and the panel it opens. It's a Wails v3 app in a module
// of its own, so Wails and its cgo stay out of the main module, whose
// binaries build without cgo.
//
// The panel is web content in the system web view, served through
// Wails' Go asset handler, so nothing listens on TCP. Package panel
// renders it, and the app pushes the parts that change as Wails events.
//
// It reads and commands the agent through the CLI's control-channel
// client, cli.Link, with its check that the server runs as root or
// SYSTEM, from "Connect the tray app to the agent's control channel".
// Package lifecycle decides when it quits and relaunches:
//
//	silkpool-tray [--sign-in] [--fixture <name>]
//
// --sign-in says the sign-in entries started it, so in an account that
// isn't an administrator's it exits at once and shows nothing. --fixture
// shows a fixture state instead of the agent's, which the menu then
// picks, to check the panel by hand.
//
// It carries what "Test a Wails v3 tray app on macOS and Windows" found:
//
//   - the macOS icon is set through a direct AppKit call, as a template
//     in every state but the amber Degraded one (icon_darwin.go);
//   - the window's buttons are hidden on Windows, where a frameless tool
//     window keeps its close button otherwise;
//   - the panel's window is made when the icon is first clicked, so no
//     hidden window takes the keyboard's focus as the app starts on
//     Windows;
//   - the icon is made once the app has started, and nothing touches it
//     before: one made earlier starts on a goroutine of Wails' own, which
//     raced the readings that set its image (#286);
//   - the menu is updated once the icon is made, which groups its radio
//     items on Windows;
//   - the window is as tall as the panel, so its buttons have room to
//     wrap, as they do on macOS;
//   - the exe carries version info (package versioninfo);
//   - the corners are the OS's: 8 px on Windows 11, 12 px on macOS.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"os"
	"runtime"
	"sync"
	"time"
	_ "time/tzdata" // Windows has no zoneinfo for Go to read.

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"

	"github.com/meschoening/silkpool/cmd/silkpool-tray/icon"
	"github.com/meschoening/silkpool/cmd/silkpool-tray/lifecycle"
	"github.com/meschoening/silkpool/cmd/silkpool-tray/panel"
	"github.com/meschoening/silkpool/cmd/silkpool-tray/source"
	"github.com/meschoening/silkpool/internal/cli"
	"github.com/meschoening/silkpool/internal/control"
	"github.com/meschoening/silkpool/internal/version"
)

// offset is the gap between the panel and the tray icon or the menu bar.
func offset() int {
	if runtime.GOOS == "windows" {
		return 12
	}
	return 6
}

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	fail := func(err error) {
		log.Error("silkpool-tray: " + err.Error())
		os.Exit(1)
	}
	flags := flag.NewFlagSet("silkpool-tray", flag.ContinueOnError)
	signIn := flags.Bool(lifecycle.SignInFlag, false, "the sign-in entries started it")
	fixture := flags.String("fixture", "", "show the fixture state named `name`, to check the panel by hand")
	if err := flags.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}
	// The binary's path, read before an upgrade can rename it aside, is
	// where the new one is once it has.
	exe, err := os.Executable()
	if err != nil {
		log.Warn("can't tell where the tray app's binary is, so it can't relaunch after an upgrade: " + err.Error())
	}
	host, _ := os.Hostname()
	a := app{
		life: &lifecycle.Lifecycle{SignIn: *signIn},
		host: host,
		relaunch: func() error {
			if exe == "" {
				return errNoBinary
			}
			cmd := lifecycle.Command(runtime.GOOS, exe, os.Args[1:])
			if err := cmd.Start(); err != nil {
				return err
			}
			return cmd.Process.Release()
		},
		log: log,
	}
	if *fixture != "" {
		fake, err := source.NewFake(source.Machine(runtime.GOOS), source.HubURL, *fixture, time.Now)
		if err != nil {
			fail(err)
		}
		a.src, a.send, a.fake = fake, fake, fake
	} else {
		link := cli.NewLink(cli.Env{Socket: control.Path, Version: version.Version})
		a.src, a.send = link, link
	}
	if err := a.run(); err != nil {
		fail(err)
	}
}

// errNoBinary fails a relaunch when the app couldn't tell where its
// binary is.
var errNoBinary = errors.New("the tray app doesn't know where its binary is")

// An app is what the tray app runs on: where it reads the agent and
// sends its commands, which may be a fake's fixtures that the menu picks,
// and when it quits or relaunches.
type app struct {
	src  source.Source
	send panel.Sender
	fake *source.Fake
	life *lifecycle.Lifecycle
	// host is the machine's host name, which the panel shows until it
	// reads the agent's status.
	host string
	// relaunch starts the app again from its binary.
	relaunch func() error
	log      *slog.Logger
}

// A tray is the running app: the icon, its menu, and the panel's window.
type tray struct {
	app   *application.App
	icon  *application.SystemTray
	panel *panel.Panel
	log   *slog.Logger

	mu sync.Mutex
	// window is the panel's, made when the icon is first clicked.
	window *application.WebviewWindow
	// glyph is the icon's, as last set.
	glyph string
}

// run runs the tray app until it quits. The first reading decides
// whether it shows at all: started at sign-in in an account that isn't an
// administrator's, it quits before it shows anything.
func (a app) run() error {
	log, fake := a.log, a.fake
	ctx, stop := context.WithCancel(context.Background())
	readings := make(chan cli.Reading)
	watched := make(chan struct{})
	defer func() { stop(); <-watched }()
	go func() {
		defer close(watched)
		a.src.Watch(ctx, func(r cli.Reading) {
			select {
			case readings <- r:
			case <-ctx.Done():
			}
		})
	}()
	first := <-readings
	if a.life.Next(first) == lifecycle.Quit {
		return nil
	}
	p, err := panel.New(runtime.GOOS, a.host, time.Now, a.send, first)
	if err != nil {
		return err
	}

	t := &tray{panel: p, log: log}
	t.app = application.New(application.Options{
		Name:        "Silkpool",
		Description: "Silkpool's tray app",
		Assets:      application.AssetOptions{Handler: p},
		// A menu-bar app: no Dock icon or app menu.
		Mac:    application.MacOptions{ActivationPolicy: application.ActivationPolicyAccessory},
		Logger: log,
	})
	t.app.Event.On("panel:height", t.fit)

	// The icon is made once the app has started, and nothing touches it
	// or the window before: Wails starts an icon made earlier on a
	// goroutine of its own as the app starts, which raced the readings
	// that set the icon, as the race detector showed (#286). Made now,
	// it's running when New returns.
	ready := make(chan struct{})
	var once sync.Once
	t.app.Event.OnApplicationEvent(events.Common.ApplicationStarted, func(*application.ApplicationEvent) {
		once.Do(func() {
			t.start(fake)
			close(ready)
		})
	})

	go func() {
		select {
		case <-ready:
		case <-ctx.Done():
			return
		}
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case r := <-readings:
				switch a.life.Next(r) {
				case lifecycle.Quit:
					t.app.Quit()
					return
				case lifecycle.Relaunch:
					log.Info("the agent is now " + r.Version + ", so the tray app relaunches from its new binary")
					if err := a.relaunch(); err != nil {
						log.Error("can't relaunch the tray app: " + err.Error())
					} else {
						t.app.Quit()
						return
					}
				}
				if err := p.Show(r); err != nil {
					log.Error("can't show the status: " + err.Error())
				}
			case <-tick.C:
			}
			t.setIcon(false)
			t.push()
		}
	}()
	return t.app.Run()
}

// start makes the icon, with its menu, and sets its image, once the app
// has started.
func (t *tray) start(fake *source.Fake) {
	ti := t.app.SystemTray.New()
	// Kept before its click handler is set, which reads it.
	t.mu.Lock()
	t.icon = ti
	t.mu.Unlock()
	menu := t.menu(fake)
	ti.SetMenu(menu)
	ti.OnClick(func() { go t.toggle() })
	ti.OnRightClick(ti.ShowMenu)
	// Wails' Windows tray groups radio items only on an update, so until
	// then the one checked at start ignores clicks once another is picked.
	menu.Update()
	t.setIcon(true)
}

// menu is the icon's right-click menu: the fixture states, while the
// source is a fake, then Quit.
func (t *tray) menu(fake *source.Fake) *application.Menu {
	m := t.app.NewMenu()
	if fake != nil {
		states := m.AddSubmenu("Fixture state")
		for _, x := range source.Fixtures {
			states.AddRadio(x.Label, x.Name == fake.Showing()).OnClick(func(*application.Context) {
				if err := fake.Show(x.Name); err != nil {
					t.log.Error(err.Error())
				}
			})
		}
		m.AddSeparator()
	}
	m.Add("Quit Silkpool's tray").OnClick(func(*application.Context) { t.app.Quit() })
	return m
}

// toggle shows the panel, or hides it if it's showing. The first click
// makes its window.
func (t *tray) toggle() {
	t.mu.Lock()
	w := t.window
	if w == nil {
		w = t.app.Window.NewWithOptions(application.WebviewWindowOptions{
			Name:             "panel",
			Title:            "Silkpool",
			Width:            panel.Width,
			Height:           420,
			URL:              panel.Path,
			Frameless:        true,
			Hidden:           true,
			AlwaysOnTop:      true,
			DisableResize:    true,
			HideOnFocusLost:  true,
			HideOnEscape:     true,
			BackgroundType:   application.BackgroundTypeTranslucent,
			BackgroundColour: application.NewRGBA(0, 0, 0, 0),
			// A frameless tool window keeps its system menu, so Windows
			// draws a close button over the panel unless they're hidden.
			MinimiseButtonState: application.ButtonHidden,
			MaximiseButtonState: application.ButtonHidden,
			CloseButtonState:    application.ButtonHidden,
			Mac: application.MacWindow{
				Backdrop:     application.MacBackdropTranslucent,
				CornerRadius: 12,
			},
			// Windows 11 rounds the Acrylic flyout's corners at 8 px.
			Windows: application.WindowsWindow{
				BackdropType:    application.Acrylic,
				HiddenOnTaskbar: true,
			},
		})
		// Closing hides it, as losing focus does.
		w.RegisterHook(events.Common.WindowClosing, func(e *application.WindowEvent) {
			w.Hide()
			e.Cancel()
		})
		t.icon.AttachWindow(w).WindowOffset(offset())
		t.window = w
	}
	ti := t.icon
	t.mu.Unlock()
	ti.ToggleWindow()
	if w.IsVisible() {
		focusPanel(w)
	}
}

// fit makes the window as tall as the panel, which its script says it is,
// and keeps it at the icon.
func (t *tray) fit(e *application.CustomEvent) {
	h, ok := e.Data.(float64)
	if !ok || h < 1 || h > 4000 {
		return
	}
	t.mu.Lock()
	w := t.window
	t.mu.Unlock()
	if w == nil {
		return
	}
	w.SetSize(panel.Width, int(h))
	if w.IsVisible() {
		if err := t.icon.PositionWindow(w, offset()); err != nil {
			t.log.Warn("can't keep the panel at the icon: " + err.Error())
		}
	}
}

// setIcon sets the icon and its tooltip to the state's when it has
// changed, or at once if force.
func (t *tray) setIcon(force bool) {
	glyph, tooltip := t.panel.Icon()
	t.mu.Lock()
	ti, changed := t.icon, glyph != t.glyph
	t.glyph = glyph
	t.mu.Unlock()
	if !changed && !force {
		return
	}
	ti.SetTooltip(tooltip)
	setIcon(ti, icon.For(glyph))
}

// push sends the window the parts of the panel that changed.
func (t *tray) push() {
	html, err := t.panel.Changes()
	if err != nil {
		t.log.Error("can't render the panel: " + err.Error())
		return
	}
	if html != "" {
		t.app.Event.Emit("panel", html)
	}
}
