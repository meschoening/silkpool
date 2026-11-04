package dashboard

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// The live connection, as ADR 0009 decided: the Hub renders a page's
// live parts again as its state changes, and pushes those that changed
// to the page, where the live-update script (package ui's live.js)
// morphs them in.
//
// A page names its live parts in page.Live: templates of its own, each
// one element whose id is its name. Its frame then marks the body
// data-stream, and the script opens the page's stream: a GET of the
// page's own URL that accepts text/event-stream, as an EventSource
// sends. The stream runs the page's own handler, so it admits whoever
// the page admits, and renders the page's live parts from what the
// handler rendered. Its first message carries every live part, and each
// later one the parts that changed. It renders the page again every
// LiveEvery: the parts' words change with the Hub's state, which only
// its loop changes, and with the clock alone, such as an elapsed time,
// so a poll catches both. A live page's GET must therefore change
// nothing.
//
// When the page goes elsewhere (a redirect, as when the session ends,
// or a refusal, as when its node is gone), the stream sends a reload
// event and ends, and the script loads the page again. When the Hub
// can't answer, as while it stops, the stream ends quietly, and the
// browser tries again until it's back.

// LiveEvery is how often a page's stream renders the page again.
const LiveEvery = time.Second

// keepAlive is how many renders that change nothing pass before the
// stream sends a comment, which keeps a proxy from closing it as idle.
const keepAlive = 15

// wantsStream reports a request for a page's stream: a GET that accepts
// text/event-stream.
func wantsStream(r *http.Request) bool {
	return r.Method == http.MethodGet && strings.Contains(r.Header.Get("Accept"), "text/event-stream")
}

// A view is what a page's handler answered its stream with: the page it
// rendered, by name, and its data, or only the status it answered with.
type view struct {
	status int
	name   string
	data   any
}

// live reports a page with live parts, rendered as it is.
func (v view) live() bool { return v.status == http.StatusOK && len(v.parts()) > 0 }

// parts are the names of the page's live parts.
func (v view) parts() []string {
	if f, ok := v.data.(framed); ok {
		return f.frame().Live
	}
	return nil
}

// A viewWriter takes a page's answer for its stream: render hands it the
// page instead of writing it, and anything else written is dropped.
type viewWriter struct {
	header http.Header
	view
}

func (v *viewWriter) Header() http.Header { return v.header }

func (v *viewWriter) WriteHeader(status int) {
	if v.status == 0 {
		v.status = status
	}
}

func (v *viewWriter) Write(b []byte) (int, error) {
	v.WriteHeader(http.StatusOK)
	return len(b), nil
}

// view runs the handler of r's page, and returns what it answered.
func (d *Dashboard) view(r *http.Request) view {
	w := &viewWriter{header: http.Header{}}
	d.mux.ServeHTTP(w, r.Clone(r.Context()))
	return w.view
}

// stream serves the stream of r's page, until the browser leaves, the
// page goes elsewhere, or the Hub can't answer. A page without live
// parts has none: 204, which tells the browser not to ask again.
func (d *Dashboard) stream(w http.ResponseWriter, r *http.Request) {
	v := d.view(r)
	if !v.live() && v.status < http.StatusInternalServerError {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	// A proxy in front of the Hub mustn't hold the events back.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	// send writes an event and flushes it. idle counts the renders since
	// the stream last sent one.
	idle := 0
	send := func(event string) error {
		idle = 0
		if _, err := io.WriteString(w, event); err != nil {
			return err
		}
		return rc.Flush()
	}
	sent := map[string]string{}
	tick := time.NewTicker(d.liveEvery)
	defer tick.Stop()
	for {
		switch {
		case v.status >= http.StatusInternalServerError:
			return
		case !v.live():
			send("event: reload\ndata:\n\n")
			return
		}
		changed, err := d.changed(v, sent)
		if err != nil {
			d.log.Error("a page's stream couldn't render it", "path", logPath(r), "err", err)
			return
		}
		idle++
		switch {
		case changed != "":
			err = send(message(changed))
		case idle >= keepAlive:
			err = send(":\n\n")
		}
		if err != nil {
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
		}
		v = d.view(r)
	}
}

// changed renders v's live parts, and returns those that differ from
// what sent holds of them, which it updates.
func (d *Dashboard) changed(v view, sent map[string]string) (string, error) {
	var out strings.Builder
	for _, part := range v.parts() {
		var b strings.Builder
		if err := d.pages[v.name].ExecuteTemplate(&b, part, v.data); err != nil {
			return "", fmt.Errorf("render %s's part %s: %w", v.name, part, err)
		}
		if sent[part] != b.String() {
			sent[part] = b.String()
			out.WriteString(b.String())
			out.WriteByte('\n')
		}
	}
	return out.String(), nil
}

// lineBreaks are the line breaks a message's data may hold, which an
// event's lines can't.
var lineBreaks = strings.NewReplacer("\r\n", "\n", "\r", "\n")

// message returns a message event whose data is html.
func message(html string) string {
	var b strings.Builder
	for line := range strings.SplitSeq(strings.TrimSuffix(lineBreaks.Replace(html), "\n"), "\n") {
		b.WriteString("data: ")
		b.WriteString(line)
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	return b.String()
}
