// Command llhls-origin serves a live low-latency HLS stream on localhost — the
// playlists only, no media — so latencyprobe can be tried without a real origin,
// and each protocol fault it detects can be switched on to watch it fire.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"time"

	"github.com/Allan-Nava/latencyprobe/internal/llhlstest"
)

func main() {
	var f llhlstest.Faults
	partTarget := flag.Float64("part-target", 0.5, "PART-TARGET in seconds")
	perSegment := flag.Int("parts", 4, "parts per segment")
	flag.BoolVar(&f.NoBlock, "no-block", false, "answer blocking reloads at once")
	flag.BoolVar(&f.StaleCache, "stale-cache", false, "replay the first playlist, like a cache keyed without the query string")
	flag.BoolVar(&f.AgeHeader, "age", false, "set an Age header, as an HTTP cache would")
	flag.DurationVar(&f.PublishLag, "lag", 0, "list each part this long after its media ends")
	flag.BoolVar(&f.NoDelta, "no-delta", false, "ignore _HLS_skip")
	flag.BoolVar(&f.SkipTooMuch, "skip-too-much", false, "skip segments inside the Skip Boundary")
	flag.BoolVar(&f.NoReports, "no-reports", false, "omit rendition reports")
	flag.IntVar(&f.StaleReports, "stale-reports", 0, "rendition reports this many segments behind")
	flag.Float64Var(&f.PartHoldBack, "part-hold-back", 0, "PART-HOLD-BACK override in seconds")
	flag.Float64Var(&f.SlowParts, "slow", 0, "publish parts this many times slower than PART-TARGET")
	flag.Parse()

	o := llhlstest.New(*partTarget, *perSegment, 8, f)
	defer o.Close()
	fmt.Printf("live LL-HLS origin: %s/master.m3u8  (Ctrl-C to stop)\n", o.URL)
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	select {
	case <-c:
	case <-time.After(24 * time.Hour):
	}
}
