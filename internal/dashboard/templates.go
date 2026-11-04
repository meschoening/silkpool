package dashboard

import (
	"embed"
	"fmt"
	"html/template"
)

//go:embed templates/*.tmpl
var templateFiles embed.FS

// funcs are the dashboard's own template functions, beside the shared
// ones.
var funcs = template.FuncMap{
	// step is the open setup step's name and body, for the "step"
	// template.
	"step": func(name string, now stepBody) any {
		return struct {
			Name string
			Now  stepBody
		}{name, now}
	},
	// tabs are the top bar's.
	"tabs": func() []tab { return tabs },
	// nodes counts nodes: "1 node", "4 nodes".
	"nodes": func(n int) string { return count(n, "node") },
	// count counts things: "1 notice", "2 notices".
	"count": count,
}

// count is n of noun, plural unless n is 1.
func count(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// A tab is one of the top bar's.
type tab struct{ Name, Title, Path string }

// tabs are the top bar's: Nodes, Targets, Activity and Settings.
var tabs = []tab{
	{"nodes", "Nodes", "/"},
	{"targets", "Targets", "/targets"},
	{"activity", "Activity", "/activity"},
	{"settings", "Settings", "/settings"},
}
