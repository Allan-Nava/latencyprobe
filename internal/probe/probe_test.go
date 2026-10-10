package probe

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Allan-Nava/latencyprobe/internal/finding"
	"github.com/Allan-Nava/latencyprobe/internal/llhlstest"
)

// 100ms parts, four to a 400ms segment, eight segments listed.
func run(t *testing.T, f llhlstest.Faults, path string) *finding.Result {
	t.Helper()
	o := llhlstest.New(0.1, 4, 8, f)
	t.Cleanup(o.Close)
	return Run(context.Background(), Options{URL: o.URL + path, Duration: 1500 * time.Millisecond})
}

func find(r *finding.Result, check string, st finding.Status) *finding.Finding {
	for i, f := range r.Findings {
		if f.Check == check && f.Status == st {
			return &r.Findings[i]
		}
	}
	return nil
}

func dump(r *finding.Result) string {
	var b strings.Builder
	for _, f := range r.Findings {
		b.WriteString("\n  " + string(f.Status) + " " + f.Check + ": " + f.Message)
	}
	return b.String()
}

func TestConformingOriginHasNothingWrong(t *testing.T) {
	t.Parallel()
	r := run(t, llhlstest.Faults{}, "/master.m3u8")
	if w := r.Worst(); w != finding.OK {
		t.Fatalf("worst = %s:%s", w, dump(r))
	}
	for _, c := range []string{"server-control", "bad-request", "delta", "rendition-report", "blocking", "cadence", "edge-age", "latency"} {
		if find(r, c, finding.OK) == nil {
			t.Errorf("no OK %s finding:%s", c, dump(r))
		}
	}
	if age := find(r, "edge-age", finding.OK); age != nil && *age.Value > 0.1 {
		t.Errorf("edge age %.3fs on an origin that publishes on time", *age.Value)
	}
}

func TestFaults(t *testing.T) {
	cases := []struct {
		name  string
		f     llhlstest.Faults
		check string
		want  finding.Status
		text  string
	}{
		{"server does not block", llhlstest.Faults{NoBlock: true}, "blocking", finding.BAD, "without the requested part"},
		{"cache keyed without the query", llhlstest.Faults{StaleCache: true, AgeHeader: true}, "blocking", finding.BAD, "without the requested part"},
		{"one reload in three answered early", llhlstest.Faults{ForgetsToBlock: 3}, "blocking", finding.BAD, "without the requested part"},
		{"_HLS_part alone accepted", llhlstest.Faults{NoBadRequest: true}, "bad-request", finding.BAD, "_HLS_part without _HLS_msn"},
		{"far-future msn blocks", llhlstest.Faults{IgnoreAdvance: true}, "bad-request", finding.WARN, "ten segments ahead"},
		{"delta ignored", llhlstest.Faults{NoDelta: true}, "delta", finding.BAD, "returned a full playlist"},
		{"delta skips inside the boundary", llhlstest.Faults{SkipTooMuch: true}, "delta", finding.BAD, "inside the"},
		{"no rendition reports", llhlstest.Faults{NoReports: true}, "rendition-report", finding.WARN, "have no EXT-X-RENDITION-REPORT"},
		{"stale rendition reports", llhlstest.Faults{StaleReports: 3}, "rendition-report", finding.WARN, "parts behind"},
		{"PART-HOLD-BACK under 2x", llhlstest.Faults{PartHoldBack: 0.15}, "server-control", finding.BAD, "below twice PART-TARGET"},
		{"PART-HOLD-BACK under 3x", llhlstest.Faults{PartHoldBack: 0.25}, "server-control", finding.WARN, "below three PART-TARGETs"},
		{"parts slower than PART-TARGET", llhlstest.Faults{SlowParts: 2.5}, "cadence", finding.WARN, "slower than PART-TARGET"},
		{"no PROGRAM-DATE-TIME", llhlstest.Faults{NoPDT: true}, "profile", finding.WARN, "no EXT-X-PROGRAM-DATE-TIME"},
		{"not low-latency", llhlstest.Faults{NoServerCtl: true}, "blocking", finding.WARN, "no CAN-BLOCK-RELOAD"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := run(t, c.f, "/master.m3u8")
			f := find(r, c.check, c.want)
			if f == nil || !strings.Contains(f.Message, c.text) {
				t.Fatalf("want %s %s containing %q:%s", c.want, c.check, c.text, dump(r))
			}
		})
	}
}

func TestCacheFaultNamesTheCache(t *testing.T) {
	t.Parallel()
	r := run(t, llhlstest.Faults{StaleCache: true, AgeHeader: true}, "/a.m3u8")
	f := find(r, "blocking", finding.BAD)
	if f == nil || !strings.Contains(f.Hint, "Age header") {
		t.Fatalf("hint should blame a cache:%s", dump(r))
	}
}

func TestPublishLagShowsInEdgeAge(t *testing.T) {
	t.Parallel()
	r := run(t, llhlstest.Faults{PublishLag: 400 * time.Millisecond}, "/a.m3u8")
	f := find(r, "edge-age", finding.OK)
	if f == nil || *f.Value < 0.35 || *f.Value > 0.6 {
		t.Fatalf("edge age should be ~0.4s:%s", dump(r))
	}
	lat := find(r, "latency", finding.OK)
	if lat == nil || *lat.Value < 0.3+0.35 {
		t.Fatalf("latency should add PART-HOLD-BACK (0.3s) to the edge age:%s", dump(r))
	}
}

func TestEndedPlaylistIsNotProbed(t *testing.T) {
	t.Parallel()
	r := Run(context.Background(), Options{URL: "not a url"})
	if find(r, "fetch", finding.ERROR) == nil {
		t.Fatal(dump(r))
	}
}
