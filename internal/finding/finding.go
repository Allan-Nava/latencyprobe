// Package finding is the result model: one Finding per observation, ranked
// OK < WARN < BAD < ERROR, worst first.
package finding

import (
	"sort"
	"time"
)

// Status of a finding. ERROR means the check could not run, not that the
// stream failed it.
type Status string

const (
	OK    Status = "OK"
	WARN  Status = "WARN"
	BAD   Status = "BAD"
	ERROR Status = "ERROR"
)

var rank = map[Status]int{OK: 0, WARN: 1, BAD: 2, ERROR: 3}

// AtLeast reports whether s is at or above threshold.
func AtLeast(s, threshold Status) bool { return rank[s] >= rank[threshold] }

// Finding is one observation.
//
// Rule names the requirement it comes from — a section of
// draft-pantos-hls-rfc8216bis-22, or its Appendix B.1 low-latency profile — so
// an operator who disagrees has something to quote. Value and Unit carry the
// measurement, so a machine reader never parses Message.
type Finding struct {
	Check   string   `json:"check"`
	Rule    string   `json:"rule,omitempty"`
	Status  Status   `json:"status"`
	Message string   `json:"message"`
	Value   *float64 `json:"value,omitempty"`
	Unit    string   `json:"unit,omitempty"`
	Hint    string   `json:"hint,omitempty"`
}

// Num returns a pointer to v, for Finding.Value.
func Num(v float64) *float64 { return &v }

// Result is a whole probe run.
type Result struct {
	Source   string    `json:"source"`
	Playlist string    `json:"playlist"`
	Started  time.Time `json:"started"`
	Seconds  float64   `json:"seconds"`
	Requests int       `json:"requests"`
	Findings []Finding `json:"findings"`
}

// Sort orders findings worst first, keeping check order within a status.
func (r *Result) Sort() {
	sort.SliceStable(r.Findings, func(i, j int) bool {
		return rank[r.Findings[i].Status] > rank[r.Findings[j].Status]
	})
}

// Worst is the highest status among the findings, OK when there are none.
func (r *Result) Worst() Status {
	w := OK
	for _, f := range r.Findings {
		if rank[f.Status] > rank[w] {
			w = f.Status
		}
	}
	return w
}

// Count tallies findings per status.
func (r *Result) Count() map[Status]int {
	c := map[Status]int{OK: 0, WARN: 0, BAD: 0, ERROR: 0}
	for _, f := range r.Findings {
		c[f.Status]++
	}
	return c
}
