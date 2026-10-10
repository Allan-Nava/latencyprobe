// Package report renders a Result as text for a terminal or JSON for a machine.
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/Allan-Nava/latencyprobe/internal/finding"
)

var glyph = map[finding.Status]string{
	finding.OK:    "🟢 OK   ",
	finding.WARN:  "🟡 WARN ",
	finding.BAD:   "🔴 BAD  ",
	finding.ERROR: "⚫ ERROR",
}

// Text writes one block per finding, worst first, and a summary line.
func Text(w io.Writer, r *finding.Result) {
	width := 0
	for _, f := range r.Findings {
		width = max(width, len(f.Check))
	}
	if r.Playlist != "" && r.Playlist != r.Source {
		fmt.Fprintf(w, "media playlist %s\n\n", r.Playlist)
	}
	indent := strings.Repeat(" ", 9+width+2)
	for _, f := range r.Findings {
		rule := ""
		if f.Rule != "" {
			rule = "  [" + f.Rule + "]"
		}
		fmt.Fprintf(w, "%s  %-*s  %s%s\n", glyph[f.Status], width, f.Check, f.Message, rule)
		if f.Hint != "" && f.Status != finding.OK {
			fmt.Fprintf(w, "%s↳ %s\n", indent, f.Hint)
		}
	}
	c := r.Count()
	fmt.Fprintf(w, "\n%d findings: %d OK, %d WARN, %d BAD, %d ERROR — %d requests in %.1fs\n",
		len(r.Findings), c[finding.OK], c[finding.WARN], c[finding.BAD], c[finding.ERROR], r.Requests, r.Seconds)
	fmt.Fprintln(w, "Rules cite draft-pantos-hls-rfc8216bis-22; B.1 is its low-latency server profile.")
}

// JSON writes the Result as indented JSON.
func JSON(w io.Writer, r *finding.Result) error {
	e := json.NewEncoder(w)
	e.SetIndent("", "  ")
	return e.Encode(r)
}
