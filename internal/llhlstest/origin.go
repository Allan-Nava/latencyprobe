// Package llhlstest is a live low-latency HLS origin for tests: it publishes
// Partial Segments in real time from a start instant and answers the delivery
// directives of §6.2.5 — and can be told to get them wrong, one fault at a time,
// so each check is seen both passing and failing.
package llhlstest

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Faults are deliberate protocol violations. The zero value is a conforming origin.
type Faults struct {
	NoBlock        bool          // answer _HLS_msn/_HLS_part at once, with whatever exists
	StaleCache     bool          // behave like a cache keyed without the query: replay the first response
	PublishLag     time.Duration // list each part this long after its media ends
	NoBadRequest   bool          // accept _HLS_part without _HLS_msn instead of a 400
	NoDelta        bool          // ignore _HLS_skip
	SkipTooMuch    bool          // skip segments inside the Skip Boundary
	NoReports      bool          // omit EXT-X-RENDITION-REPORT
	StaleReports   int           // report the other renditions this many segments behind
	PartHoldBack   float64       // override PART-HOLD-BACK (0 keeps 3x PART-TARGET)
	NoPDT          bool          // omit EXT-X-PROGRAM-DATE-TIME
	SlowParts      float64       // publish parts this many times slower than PART-TARGET
	AgeHeader      bool          // set Age: 1 on playlist responses, as an HTTP cache would
	NoServerCtl    bool          // no EXT-X-SERVER-CONTROL at all: not low-latency
	IgnoreAdvance  bool          // block on far-future _HLS_msn instead of a 400
	ForgetsToBlock int           // every Nth blocking request is answered at once
}

// Origin serves a multivariant playlist at /master.m3u8 and renditions
// /a.m3u8 and /b.m3u8.
type Origin struct {
	*httptest.Server
	Faults     Faults
	Target     float64 // TARGETDURATION, seconds
	PartTarget float64 // PART-TARGET, seconds
	PerSegment int     // parts per segment
	Window     int     // complete segments listed
	SkipUntil  float64 // CAN-SKIP-UNTIL, seconds
	start      time.Time
	mu         sync.Mutex
	stale      map[string]string
	blocking   int
}

// New starts an origin publishing parts of partTarget seconds, perSegment to a
// segment, beginning `warm` ago so the playlist already has a window.
func New(partTarget float64, perSegment, window int, f Faults) *Origin {
	o := &Origin{
		Faults:     f,
		PartTarget: partTarget,
		PerSegment: perSegment,
		Target:     partTarget * float64(perSegment),
		Window:     window,
	}
	o.SkipUntil = 6 * o.Target
	o.start = time.Now().Add(-time.Duration(float64(window+1) * o.Target * float64(time.Second)))
	o.Server = httptest.NewServer(http.HandlerFunc(o.serve))
	return o
}

func (o *Origin) partDur() time.Duration {
	slow := o.Faults.SlowParts
	if slow <= 0 {
		slow = 1
	}
	return time.Duration(o.PartTarget * slow * float64(time.Second))
}

// published is how many parts exist at instant t.
func (o *Origin) published(t time.Time) int64 {
	n := int64(t.Sub(o.start) - o.Faults.PublishLag)
	if n < 0 {
		return 0
	}
	return n / int64(o.partDur())
}

func (o *Origin) serve(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/master.m3u8":
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=800000\na.m3u8\n#EXT-X-STREAM-INF:BANDWIDTH=2000000\nb.m3u8\n")
		return
	case "/a.m3u8", "/b.m3u8":
	default:
		http.NotFound(w, r)
		return
	}
	if o.Faults.AgeHeader {
		w.Header().Set("Age", "1")
	}
	q := r.URL.Query()
	msnS, hasMSN := q["_HLS_msn"]
	_, hasPart := q["_HLS_part"]
	if hasPart && !hasMSN && !o.Faults.NoBadRequest {
		http.Error(w, "_HLS_part without _HLS_msn", http.StatusBadRequest)
		return
	}
	if hasMSN && !o.Faults.NoServerCtl {
		msn, _ := strconv.ParseInt(msnS[0], 10, 64)
		part := -1
		if hasPart {
			part, _ = strconv.Atoi(q.Get("_HLS_part"))
		}
		want := msn*int64(o.PerSegment) + int64(max(part, 0))
		if part < 0 {
			want = (msn+1)*int64(o.PerSegment) - 1
		}
		lastMSN := (o.published(time.Now()) - 1) / int64(o.PerSegment)
		if msn > lastMSN+2 && !o.Faults.IgnoreAdvance {
			http.Error(w, "too far ahead", http.StatusBadRequest)
			return
		}
		o.mu.Lock()
		o.blocking++
		forget := o.Faults.ForgetsToBlock > 0 && o.blocking%o.Faults.ForgetsToBlock == 0
		o.mu.Unlock()
		if !o.Faults.NoBlock && !forget {
			deadline := time.Now().Add(time.Duration(3 * o.Target * float64(time.Second)))
			for o.published(time.Now()) <= want {
				if time.Now().After(deadline) {
					http.Error(w, "not yet", http.StatusServiceUnavailable)
					return
				}
				select {
				case <-r.Context().Done():
					return
				case <-time.After(5 * time.Millisecond):
				}
			}
		}
	}
	body := o.playlist(r.URL.Path, time.Now(), q.Get("_HLS_skip") != "")
	if o.Faults.StaleCache {
		o.mu.Lock()
		if o.stale == nil {
			o.stale = map[string]string{}
		}
		if _, ok := o.stale[r.URL.Path]; !ok {
			o.stale[r.URL.Path] = body
		}
		body = o.stale[r.URL.Path]
		o.mu.Unlock()
	}
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	fmt.Fprint(w, body)
}

func (o *Origin) playlist(path string, now time.Time, skip bool) string {
	n := o.published(now)
	per := int64(o.PerSegment)
	complete := n / per // segments 0..complete-1 are whole
	tail := int(n % per)
	first := max(complete-int64(o.Window), 0)
	partSec := o.partDur().Seconds()
	segSec := partSec * float64(per)

	var b strings.Builder
	fmt.Fprintf(&b, "#EXTM3U\n#EXT-X-VERSION:9\n#EXT-X-TARGETDURATION:%g\n", o.Target)
	if !o.Faults.NoServerCtl {
		phb := o.Faults.PartHoldBack
		if phb == 0 {
			phb = 3 * o.PartTarget
		}
		fmt.Fprintf(&b, "#EXT-X-SERVER-CONTROL:CAN-BLOCK-RELOAD=YES,PART-HOLD-BACK=%g,CAN-SKIP-UNTIL=%g\n", phb, o.SkipUntil)
		fmt.Fprintf(&b, "#EXT-X-PART-INF:PART-TARGET=%g\n", o.PartTarget)
	}
	fmt.Fprintf(&b, "#EXT-X-MEDIA-SEQUENCE:%d\n", first)

	start := first
	if skip && !o.Faults.NoDelta && !o.Faults.NoServerCtl {
		// Segments further from the end than the Skip Boundary are replaced.
		keep := int64(o.SkipUntil/segSec) + 1
		if o.Faults.SkipTooMuch {
			keep = 1
		}
		if complete-first > keep {
			start = complete - keep
			fmt.Fprintf(&b, "#EXT-X-SKIP:SKIPPED-SEGMENTS=%d\n", start-first)
		}
	}
	if !o.Faults.NoPDT {
		t := o.start.Add(time.Duration(float64(start) * segSec * float64(time.Second)))
		fmt.Fprintf(&b, "#EXT-X-PROGRAM-DATE-TIME:%s\n", t.UTC().Format("2006-01-02T15:04:05.000Z"))
	}
	name := strings.TrimSuffix(strings.TrimPrefix(path, "/"), ".m3u8")
	for s := start; s < complete; s++ {
		if s >= complete-3 && !o.Faults.NoServerCtl { // parts kept for the newest segments
			for p := int64(0); p < per; p++ {
				fmt.Fprintf(&b, "#EXT-X-PART:DURATION=%g,URI=\"%s.%d.%d.mp4\"%s\n", partSec, name, s, p, indep(p))
			}
		}
		fmt.Fprintf(&b, "#EXTINF:%g,\n%s.%d.mp4\n", segSec, name, s)
	}
	if !o.Faults.NoServerCtl {
		for p := 0; p < tail; p++ {
			fmt.Fprintf(&b, "#EXT-X-PART:DURATION=%g,URI=\"%s.%d.%d.mp4\"%s\n", partSec, name, complete, p, indep(int64(p)))
		}
		fmt.Fprintf(&b, "#EXT-X-PRELOAD-HINT:TYPE=PART,URI=\"%s.%d.%d.mp4\"\n", name, complete, tail)
		if !o.Faults.NoReports {
			other := "b"
			if name == "b" {
				other = "a"
			}
			msn, part := complete, tail-1
			if part < 0 {
				msn, part = complete-1, o.PerSegment-1
			}
			msn -= int64(o.Faults.StaleReports)
			fmt.Fprintf(&b, "#EXT-X-RENDITION-REPORT:URI=\"%s.m3u8\",LAST-MSN=%d,LAST-PART=%d\n", other, msn, part)
		}
	}
	return b.String()
}

func indep(p int64) string {
	if p == 0 {
		return ",INDEPENDENT=YES"
	}
	return ""
}
