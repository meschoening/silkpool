package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/meschoening/silkpool/internal/control"
	"github.com/meschoening/silkpool/internal/wording"
)

// runStatus shows what the node is doing, in the tray panel's order, in
// words only. --watch reprints it on every change.
func runStatus(ctx context.Context, s *session, args []string) int {
	fs := s.flags("status", "silkpool status [--json] [--watch]")
	watch := fs.Bool("watch", false, "")
	if _, code, ok := s.parse(fs, args, 0); !ok {
		return code
	}
	if code := s.connect(ctx, false); code != ExitOK {
		return code
	}
	if !*watch {
		r, code := s.readStatus(ctx)
		if code != ExitOK {
			return code
		}
		s.printStatus(r)
		return ExitOK
	}
	first := true
	err := s.client.Watch(ctx, func(st control.Status) error {
		if s.json {
			b, err := json.Marshal(st)
			if err != nil {
				return err
			}
			fmt.Fprintln(s.env.Stdout, string(b))
			return nil
		}
		r, err := decode(st)
		if err != nil {
			return err
		}
		if !first {
			fmt.Fprintln(s.env.Stdout)
		}
		first = false
		s.printStatus(r)
		return nil
	})
	if err != nil {
		return s.failed(err)
	}
	return ExitOK
}

// labelWidth is the width of the status's labels.
const labelWidth = 13

// line prints a label and its lines, the later ones indented under the
// first.
func (s *session) line(label string, lines ...string) {
	pad := strings.Repeat(" ", max(labelWidth-len(label), 2))
	indent := strings.Repeat(" ", len(label)+len(pad))
	for i, l := range lines {
		if i == 0 {
			fmt.Fprintf(s.env.Stdout, "%s%s%s\n", label, pad, l)
		} else {
			fmt.Fprintf(s.env.Stdout, "%s%s\n", indent, l)
		}
	}
}

// printStatus prints a status, or its JSON.
func (s *session) printStatus(r read) {
	if s.json {
		s.printJSON(r.status)
		return
	}
	out, v, n, st := s.env.Stdout, r.view, r.node, r.status
	fmt.Fprintf(out, "%s · %s · %s\n", v.Name, st.Machine.OSVersion, wording.Arch(st.Machine.Arch))
	if !n.Enrolled {
		fmt.Fprintln(out, "This machine isn't enrolled in a Hub.")
		fmt.Fprintln(out, "Enroll it with a code from the Hub's Add node page:")
		fmt.Fprintln(out, "  sudo silkpool enroll <code>")
		s.line("Agent", st.Version)
		return
	}
	state := v.State()
	if v.Amber() {
		state = s.amber(state)
	}
	fmt.Fprintf(out, "%s · %s\n", state, v.Lead())
	for _, d := range v.Details() {
		fmt.Fprintln(out, d)
	}
	if p := v.Provenance(); p != "" {
		fmt.Fprintln(out, p)
	}
	for _, x := range v.Notices() {
		fmt.Fprintln(out)
		lines := sentences(x.Text)
		if x.Link != "" {
			lines = append(lines, x.Link)
		}
		if x.Clears != "" {
			lines = append(lines, x.Clears)
		}
		if x.Amber {
			for i := range lines {
				lines[i] = s.amber(lines[i])
			}
		}
		s.notice(lines)
	}
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Slots   limit %d\n", n.Availability.SlotLimit)
	rows := v.Rows()
	if len(rows) == 0 {
		fmt.Fprintln(out, "        No runners while paused.")
	}
	for _, row := range rows {
		main := row.Main
		if row.Right != "" {
			main += " · " + row.Right
		}
		sub := row.Sub
		if row.Amber {
			main, sub = s.amber(main), s.amber(sub)
		}
		fmt.Fprintf(out, "  %-6d%s\n", row.Slot, main)
		if sub != "" {
			fmt.Fprintf(out, "        %s\n", sub)
		}
	}
	fmt.Fprintln(out, v.Budget())
	fmt.Fprintln(out)
	s.autoResume(v)
	s.line("Hub", v.Hub(st.Hub.URL))
	s.line("Runs jobs for", target(r))
	s.line("Agent", fmt.Sprintf("%s · revision %d", st.Version, n.Availability.Revision))
}

// autoResume prints the auto-resume settings of v's node and what they
// mean now.
func (s *session) autoResume(v wording.View) {
	s.line("Auto-resume", wording.AutoResume(v.Node.Availability.AutoResume), v.AutoResumePreview())
}

// notice prints a notice's lines under its label.
func (s *session) notice(lines []string) {
	for i, l := range lines {
		label := "        "
		if i == 0 {
			label = "Notice  "
		}
		fmt.Fprintln(s.env.Stdout, label+l)
	}
}

// target names the node's target, from what it covers.
func target(r read) string {
	n := r.node
	switch {
	case n.Scope.Repo != "":
		return n.Scope.Owner + "/" + n.Scope.Repo
	case n.Scope.Owner != "":
		return n.Scope.Owner + "'s repositories"
	case n.Target != "":
		return n.Target
	}
	return "No target yet: choose one for this node in the Hub."
}

// sentences splits text into its sentences.
func sentences(text string) []string {
	var out []string
	for text != "" {
		i := strings.Index(text, ". ")
		if i < 0 {
			out = append(out, text)
			break
		}
		out = append(out, text[:i+1])
		text = text[i+2:]
	}
	return out
}
