// Package probe behaves like a low-latency HLS client towards a live origin and
// reports how the server answered: whether blocking reloads block, how soon a
// new Partial Segment reaches a client that asks for it, whether delta updates
// and rendition reports say what the full playlists say.
//
// Requirements are quoted from draft-pantos-hls-rfc8216bis-22. A MUST the server
// breaks is BAD; a SHOULD, or the Appendix B.1 low-latency profile, is WARN.
package probe

import (
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"

	"github.com/Allan-Nava/latencyprobe/internal/finding"
	"github.com/Allan-Nava/latencyprobe/internal/playlist"
)

// Options for one run.
type Options struct {
	URL      string
	Duration time.Duration // how long to follow the live edge with blocking reloads
	Variant  int           // index into the multivariant playlist's variants
	Client   *http.Client
	Now      func() time.Time
}

type probe struct {
	opt      Options
	res      *finding.Result
	master   *playlist.Multivariant
	masterU  *url.URL
	mediaU   *url.URL
	requests int
}

type response struct {
	status  int
	body    string
	elapsed time.Duration
	arrived time.Time
	age     string
	date    time.Time
}

// Run probes opt.URL and returns the findings, worst first.
func Run(ctx context.Context, opt Options) *finding.Result {
	if opt.Client == nil {
		opt.Client = &http.Client{}
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	p := &probe{opt: opt, res: &finding.Result{Source: opt.URL, Started: opt.Now()}}
	p.run(ctx)
	p.res.Seconds = opt.Now().Sub(p.res.Started).Seconds()
	p.res.Requests = p.requests
	p.res.Sort()
	return p.res
}

func (p *probe) add(f finding.Finding) { p.res.Findings = append(p.res.Findings, f) }

func (p *probe) fail(check, format string, a ...any) {
	p.add(finding.Finding{Check: check, Status: finding.ERROR, Message: fmt.Sprintf(format, a...)})
}

func (p *probe) get(ctx context.Context, u *url.URL, timeout time.Duration) (*response, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "latencyprobe")
	p.requests++
	t0 := p.opt.Now()
	resp, err := p.opt.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	now := p.opt.Now()
	r := &response{status: resp.StatusCode, body: string(b), elapsed: now.Sub(t0), arrived: now, age: resp.Header.Get("Age")}
	r.date, _ = http.ParseTime(resp.Header.Get("Date"))
	return r, nil
}

func (p *probe) run(ctx context.Context) {
	u, err := url.Parse(p.opt.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		p.fail("fetch", "%q is not an http(s) URL", p.opt.URL)
		return
	}
	r, err := p.get(ctx, u, 15*time.Second)
	if err != nil {
		p.fail("fetch", "cannot fetch %s: %v", u, err)
		return
	}
	if r.status != http.StatusOK {
		p.fail("fetch", "%s answered HTTP %d", u, r.status)
		return
	}
	if playlist.IsMultivariant(r.body) {
		m, err := playlist.ParseMultivariant(r.body)
		if err != nil {
			p.fail("fetch", "multivariant playlist: %v", err)
			return
		}
		if p.opt.Variant < 0 || p.opt.Variant >= len(m.Variants) {
			p.fail("fetch", "--variant %d: the playlist has %d variants (0-%d)", p.opt.Variant, len(m.Variants), len(m.Variants)-1)
			return
		}
		p.master, p.masterU = m, u
		u = u.ResolveReference(mustURL(m.Variants[p.opt.Variant].URI))
		if r, err = p.get(ctx, u, 15*time.Second); err != nil || r.status != http.StatusOK {
			p.fail("fetch", "cannot fetch the media playlist %s: %v", u, errOrStatus(err, r))
			return
		}
	}
	p.mediaU = u
	p.res.Playlist = u.String()
	pl, err := playlist.ParseMedia(r.body)
	if err != nil {
		p.fail("fetch", "media playlist %s: %v", u, err)
		return
	}
	if pl.EndList {
		p.add(finding.Finding{Check: "live", Status: finding.WARN, Message: "the playlist has EXT-X-ENDLIST: not live, so there is no delivery to probe", Hint: "point latencyprobe at a live Media or Multivariant Playlist"})
		return
	}

	p.serverControl(pl)
	p.profile(pl)
	if !pl.Server.CanBlockReload {
		p.add(finding.Finding{
			Check: "blocking", Rule: "§4.4.3.8", Status: finding.WARN,
			Message: "no CAN-BLOCK-RELOAD=YES: the server offers no blocking playlist reload, so clients poll",
			Hint:    "a polling client learns of a new part up to a reload interval late; low-latency HLS depends on blocking reload",
		})
		return
	}
	p.badRequests(ctx, pl)
	if pl.Server.CanSkipUntil != nil {
		p.delta(ctx, pl)
	}
	if p.master != nil {
		p.reports(ctx, pl)
	}
	p.follow(ctx, pl)
}

// serverControl checks the EXT-X-SERVER-CONTROL values against their stated
// minimums (§4.4.3.8).
func (p *probe) serverControl(pl *playlist.Media) {
	sc, td := pl.Server, pl.TargetDuration
	ok := true
	bad := func(rule, msg, hint string, st finding.Status) {
		ok = false
		p.add(finding.Finding{Check: "server-control", Rule: rule, Status: st, Message: msg, Hint: hint})
	}
	if sc.HoldBack != nil && *sc.HoldBack < 3*td {
		bad("§4.4.3.8", fmt.Sprintf("HOLD-BACK=%ss is below three Target Durations (%ss)", secs(*sc.HoldBack), secs(3*td)), "HOLD-BACK MUST be at least three times the Target Duration", finding.BAD)
	}
	if pl.PartTarget > 0 {
		switch {
		case sc.PartHoldBack == nil:
			bad("§4.4.3.8", "EXT-X-PART-INF without PART-HOLD-BACK", "PART-HOLD-BACK is REQUIRED when the playlist has EXT-X-PART-INF: without it a client has no low-latency starting distance", finding.BAD)
		case *sc.PartHoldBack < 2*pl.PartTarget:
			bad("§4.4.3.8", fmt.Sprintf("PART-HOLD-BACK=%ss is below twice PART-TARGET (%ss)", secs(*sc.PartHoldBack), secs(2*pl.PartTarget)), "PART-HOLD-BACK MUST be at least twice the Part Target Duration: closer than that, a player outruns the parts being published", finding.BAD)
		case *sc.PartHoldBack < 3*pl.PartTarget:
			bad("§4.4.3.8", fmt.Sprintf("PART-HOLD-BACK=%ss is below three PART-TARGETs (%ss)", secs(*sc.PartHoldBack), secs(3*pl.PartTarget)), "it SHOULD be at least three times the Part Target Duration; at two, one late part stalls playback", finding.WARN)
		}
	}
	if sc.CanSkipUntil != nil && *sc.CanSkipUntil < 6*td {
		bad("§4.4.3.8", fmt.Sprintf("CAN-SKIP-UNTIL=%ss is below six Target Durations (%ss)", secs(*sc.CanSkipUntil), secs(6*td)), "the Skip Boundary MUST be at least six times the Target Duration", finding.BAD)
	}
	if sc.CanSkipDateranges && sc.CanSkipUntil == nil {
		bad("§4.4.3.8", "CAN-SKIP-DATERANGES without CAN-SKIP-UNTIL", "CAN-SKIP-DATERANGES REQUIRES CAN-SKIP-UNTIL", finding.BAD)
	}
	if ok && sc.Present {
		hb := 3 * td
		if sc.HoldBack != nil {
			hb = *sc.HoldBack
		}
		msg := fmt.Sprintf("HOLD-BACK %ss", secs(hb))
		if sc.PartHoldBack != nil {
			msg = fmt.Sprintf("PART-HOLD-BACK %ss (%.1f× PART-TARGET %ss), %s", secs(*sc.PartHoldBack), *sc.PartHoldBack/pl.PartTarget, secs(pl.PartTarget), msg)
		}
		p.add(finding.Finding{Check: "server-control", Rule: "§4.4.3.8", Status: finding.OK, Message: msg + " — within the stated minimums"})
	}
}

// profile checks what the Appendix B.1 low-latency profile asks of the
// playlist itself.
func (p *probe) profile(pl *playlist.Media) {
	if len(pl.Segments) > 0 && pl.Segments[0].Start.IsZero() {
		p.add(finding.Finding{Check: "profile", Rule: "B.1", Status: finding.WARN, Message: "no EXT-X-PROGRAM-DATE-TIME: the edge cannot be placed in wall-clock time", Hint: "the low-latency profile puts PROGRAM-DATE-TIME in every Media Playlist; without it latencyprobe can time the protocol but not the latency"})
	}
	hasParts := false
	for _, s := range pl.Segments {
		if len(s.Parts) > 0 {
			hasParts = true
		}
	}
	if hasParts {
		hint := false
		for _, h := range pl.Hints {
			if h.Type == "PART" {
				hint = true
			}
		}
		if !hint {
			p.add(finding.Finding{Check: "profile", Rule: "B.1", Status: finding.WARN, Message: "Partial Segments but no EXT-X-PRELOAD-HINT TYPE=PART", Hint: "without the hint a client cannot request the next part before it is listed, and pays a round trip per part"})
		}
	}
}

// badRequests checks the two requests §6.2.5.2 says to refuse.
func (p *probe) badRequests(ctx context.Context, pl *playlist.Media) {
	short := 2 * time.Second
	r, err := p.get(ctx, playlist.WithDirectives(p.mediaU, "_HLS_part", "0"), short)
	switch {
	case err != nil:
		p.add(finding.Finding{Check: "bad-request", Rule: "§6.2.5.2", Status: finding.BAD, Message: fmt.Sprintf("_HLS_part without _HLS_msn did not get an answer within %s: %v", short, err), Hint: "the server MUST return Bad Request (400) for it, not block"})
	case r.status == http.StatusBadRequest:
		p.add(finding.Finding{Check: "bad-request", Rule: "§6.2.5.2", Status: finding.OK, Message: "_HLS_part without _HLS_msn is refused with 400"})
	default:
		p.add(finding.Finding{Check: "bad-request", Rule: "§6.2.5.2", Status: finding.BAD, Message: fmt.Sprintf("_HLS_part without _HLS_msn answered HTTP %d", r.status), Hint: "the server MUST return Bad Request (400)"})
	}

	last, _ := pl.Last()
	far := strconv.FormatInt(last+10, 10)
	r, err = p.get(ctx, playlist.WithDirectives(p.mediaU, "_HLS_msn", far), short)
	switch {
	case err == nil && r.status == http.StatusBadRequest:
		p.add(finding.Finding{Check: "bad-request", Rule: "§6.2.5.2", Status: finding.OK, Message: fmt.Sprintf("_HLS_msn=%s, ten segments ahead, is refused at once", far)})
	default:
		got := "blocked past " + short.String()
		if err == nil {
			got = fmt.Sprintf("answered HTTP %d after %s", r.status, r.elapsed.Round(time.Millisecond))
		}
		p.add(finding.Finding{Check: "bad-request", Rule: "§6.2.5.2", Status: finding.WARN, Message: fmt.Sprintf("_HLS_msn=%s, ten segments ahead, %s", far, got), Hint: "a request more than two segments past the edge SHOULD get an immediate 400, so a client with a bad clock fails fast instead of hanging"})
	}
}

// delta fetches the full playlist and a delta update back to back and checks
// that they agree (§6.2.5.1, §4.4.5.2).
func (p *probe) delta(ctx context.Context, pl *playlist.Media) {
	to := time.Duration((3*pl.TargetDuration + 2) * float64(time.Second))
	fr, err := p.get(ctx, p.mediaU, to)
	if err != nil || fr.status != http.StatusOK {
		p.fail("delta", "cannot fetch the full playlist: %v", errOrStatus(err, fr))
		return
	}
	dr, err := p.get(ctx, playlist.WithDirectives(p.mediaU, "_HLS_skip", "YES"), to)
	if err != nil || dr.status != http.StatusOK {
		p.add(finding.Finding{Check: "delta", Rule: "§6.2.5.1", Status: finding.BAD, Message: fmt.Sprintf("_HLS_skip=YES failed: %v", errOrStatus(err, dr)), Hint: "a server advertising CAN-SKIP-UNTIL MUST respond with a Playlist Delta Update"})
		return
	}
	full, err1 := playlist.ParseMedia(fr.body)
	d, err2 := playlist.ParseMedia(dr.body)
	if err1 != nil || err2 != nil {
		p.fail("delta", "cannot parse the full or the delta playlist: %v %v", err1, err2)
		return
	}
	skip, td := *pl.Server.CanSkipUntil, pl.TargetDuration
	switch {
	case d.SkipTags > 1:
		p.add(finding.Finding{Check: "delta", Rule: "§4.4.5.2", Status: finding.BAD, Message: fmt.Sprintf("the delta update has %d EXT-X-SKIP tags", d.SkipTags), Hint: "EXT-X-SKIP MUST NOT appear more than once"})
	case d.Skip == nil && full.Duration() > skip+td:
		p.add(finding.Finding{Check: "delta", Rule: "§6.2.5.1", Status: finding.BAD, Message: fmt.Sprintf("_HLS_skip=YES returned a full playlist (%ss listed, Skip Boundary %ss)", secs(full.Duration()), secs(skip)), Hint: "the server advertises CAN-SKIP-UNTIL, so it MUST answer with a Playlist Delta Update; a client that asked for one pays for the full playlist on every reload"})
	case d.Skip == nil:
		p.add(finding.Finding{Check: "delta", Rule: "§6.2.5.1", Status: finding.OK, Message: fmt.Sprintf("nothing to skip yet: %ss listed, inside the %ss Skip Boundary", secs(full.Duration()), secs(skip))})
	default:
		kept := d.Duration()
		if kept < skip-td {
			p.add(finding.Finding{Check: "delta", Rule: "§6.2.5.1", Status: finding.BAD, Message: fmt.Sprintf("the delta update keeps only %ss of segments, inside the %ss Skip Boundary", secs(kept), secs(skip)), Value: finding.Num(kept), Unit: "s", Hint: "only segments further from the end than CAN-SKIP-UNTIL may be skipped; a client merging this update loses segments it may still need"})
			return
		}
		byMSN := map[int64]string{}
		for _, s := range full.Segments {
			if !s.Pending {
				byMSN[s.MSN] = s.URI
			}
		}
		for _, s := range d.Segments {
			if u, ok := byMSN[s.MSN]; ok && !s.Pending && u != s.URI {
				p.add(finding.Finding{Check: "delta", Rule: "§4.4.5.2", Status: finding.BAD, Message: fmt.Sprintf("after EXT-X-SKIP:SKIPPED-SEGMENTS=%d, segment %d is %q in the delta update but %q in the full playlist", d.Skip.SkippedSegments, s.MSN, s.URI, u), Hint: "SKIPPED-SEGMENTS must count exactly the segments replaced, or a client merging the update numbers every later segment wrong"})
				return
			}
		}
		msg := fmt.Sprintf("delta update skips %s, keeps %ss, and agrees with the full playlist", plural(d.Skip.SkippedSegments, "segment"), secs(kept))
		if saved := 100 * (1 - float64(len(dr.body))/float64(len(fr.body))); saved >= 1 {
			msg += fmt.Sprintf(" — %.0f%% smaller", saved)
		}
		p.add(finding.Finding{Check: "delta", Rule: "§6.2.5.1", Status: finding.OK, Message: msg})
	}
}

// reports checks each EXT-X-RENDITION-REPORT against the rendition it
// describes, fetched straight after (§4.4.5.4, B.1).
func (p *probe) reports(ctx context.Context, pl *playlist.Media) {
	expected := map[string]bool{}
	for _, v := range p.master.Variants {
		expected[p.masterU.ResolveReference(mustURL(v.URI)).String()] = true
	}
	for _, r := range p.master.Renditions {
		expected[p.masterU.ResolveReference(mustURL(r)).String()] = true
	}
	delete(expected, p.mediaU.String())
	if len(expected) == 0 {
		return
	}
	reported := map[string]bool{}
	to := time.Duration((3*pl.TargetDuration + 2) * float64(time.Second))
	clean := true
	for _, rep := range pl.Reports {
		ru := p.mediaU.ResolveReference(mustURL(rep.URI))
		key := ru.String()
		reported[key] = true
		if !expected[key] {
			clean = false
			p.add(finding.Finding{Check: "rendition-report", Rule: "§4.4.5.4", Status: finding.WARN, Message: fmt.Sprintf("a rendition report for %s, which the multivariant playlist does not list", rep.URI)})
			continue
		}
		r, err := p.get(ctx, ru, to)
		if err != nil || r.status != http.StatusOK {
			clean = false
			p.fail("rendition-report", "cannot fetch reported rendition %s: %v", rep.URI, errOrStatus(err, r))
			continue
		}
		other, err := playlist.ParseMedia(r.body)
		if err != nil {
			clean = false
			p.fail("rendition-report", "reported rendition %s: %v", rep.URI, err)
			continue
		}
		amsn, apart := other.Last()
		per := partsPerSegment(other)
		pos := func(msn int64, part int) int64 {
			if part < 0 {
				part = per - 1
			}
			return msn*int64(per) + int64(part)
		}
		diff := pos(rep.LastMSN, rep.LastPart) - pos(amsn, apart)
		switch {
		case diff > 1:
			clean = false
			p.add(finding.Finding{Check: "rendition-report", Rule: "§4.4.5.4", Status: finding.BAD, Message: fmt.Sprintf("report for %s says %d.%d, but that playlist only reaches %d.%d", rep.URI, rep.LastMSN, rep.LastPart, amsn, apart), Hint: "a client switching renditions blocks on a part the report promised and that does not exist yet"})
		case diff < -int64(per):
			clean = false
			p.add(finding.Finding{Check: "rendition-report", Rule: "§4.4.5.4", Status: finding.WARN, Message: fmt.Sprintf("report for %s says %d.%d, %d parts behind that playlist (%d.%d)", rep.URI, rep.LastMSN, rep.LastPart, -diff, amsn, apart), Value: finding.Num(float64(-diff)), Unit: "parts", Hint: "a report is meant to be as up to date as the playlist carrying it; a stale one sends a switching client to an old part, and it falls behind live"})
		}
	}
	missing := 0
	for k := range expected {
		if !reported[k] {
			missing++
		}
	}
	if missing > 0 {
		clean = false
		p.add(finding.Finding{Check: "rendition-report", Rule: "B.1", Status: finding.WARN, Message: fmt.Sprintf("%d of %d other renditions have no EXT-X-RENDITION-REPORT", missing, len(expected)), Hint: "the low-latency profile requires one per rendition; without it a client switching renditions has to run the tune-in algorithm of Appendix C"})
	}
	if clean {
		p.add(finding.Finding{Check: "rendition-report", Rule: "§4.4.5.4", Status: finding.OK, Message: reportsOK(len(pl.Reports))})
	}
}

func partsPerSegment(pl *playlist.Media) int {
	n := 1
	for _, s := range pl.Segments {
		if !s.Pending && len(s.Parts) > n {
			n = len(s.Parts)
		}
	}
	return n
}

// follow tracks the live edge with blocking reloads for opt.Duration.
func (p *probe) follow(ctx context.Context, pl *playlist.Media) {
	td := pl.TargetDuration
	limit := time.Duration((3*td + 0.5) * float64(time.Second))
	to := limit + 3*time.Second
	end := p.opt.Now().Add(p.opt.Duration)
	partDur := pl.PartTarget
	if partDur <= 0 {
		partDur = td
	}

	var (
		total, unblocked, late, unavailable int
		withAge                             int
		waits, edgeAge, perPart             []float64
		offsets                             []float64
		lastNewAt                           time.Time
		cur                                 = pl
	)
	for p.opt.Now().Before(end) && ctx.Err() == nil {
		msn, part := cur.Next()
		kv := []string{"_HLS_msn", strconv.FormatInt(msn, 10)}
		if part >= 0 {
			kv = append(kv, "_HLS_part", strconv.Itoa(part))
		}
		r, err := p.get(ctx, playlist.WithDirectives(p.mediaU, kv...), to)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			p.fail("blocking", "blocking reload for %d.%d failed: %v", msn, part, err)
			return
		}
		total++
		if r.status == http.StatusServiceUnavailable {
			unavailable++
			continue
		}
		if r.status != http.StatusOK {
			p.fail("blocking", "blocking reload for %d.%d answered HTTP %d", msn, part, r.status)
			return
		}
		np, err := playlist.ParseMedia(r.body)
		if err != nil {
			p.fail("blocking", "blocking reload for %d.%d: %v", msn, part, err)
			return
		}
		if !r.date.IsZero() {
			offsets = append(offsets, r.date.Sub(r.arrived).Seconds())
		}
		if !np.Contains(msn, part) {
			unblocked++
			if r.age != "" && r.age != "0" {
				withAge++
			}
			// A server that does not block would have us spin: wait a part.
			select {
			case <-ctx.Done():
			case <-time.After(time.Duration(partDur * float64(time.Second))):
			}
			continue
		}
		waits = append(waits, r.elapsed.Seconds())
		if r.elapsed > limit {
			late++
		}
		if e, ok := np.EdgeEnd(); ok {
			edgeAge = append(edgeAge, r.arrived.Sub(e).Seconds())
		}
		om, op := cur.Last()
		nm, npart := np.Last()
		if gained := advance(om, op, nm, npart, partsPerSegment(np)); gained > 0 {
			if !lastNewAt.IsZero() {
				perPart = append(perPart, r.arrived.Sub(lastNewAt).Seconds()/float64(gained))
			}
			lastNewAt = r.arrived
			cur = np
		}
	}
	if total == 0 {
		p.fail("blocking", "no blocking reload completed in %s", p.opt.Duration)
		return
	}

	if unblocked > 0 {
		hint := "the server MUST hold the request until the playlist contains the requested part; answering early sends the client into a reload loop and it learns of each part late"
		if withAge > 0 {
			hint = fmt.Sprintf("%d of those responses carried an Age header: a cache is answering, most likely keyed without the query string, so _HLS_msn and _HLS_part never reach the origin", withAge)
		}
		p.add(finding.Finding{Check: "blocking", Rule: "§6.2.5.2", Status: finding.BAD, Message: fmt.Sprintf("%d of %d blocking reloads came back without the requested part", unblocked, total), Value: finding.Num(float64(unblocked)), Unit: "responses", Hint: hint})
	} else {
		p.add(finding.Finding{Check: "blocking", Rule: "§6.2.5.2", Status: finding.OK, Message: fmt.Sprintf("%d blocking reloads, each answered with the requested part; wait p50 %s, p95 %s", total, ms(pct(waits, 50)), ms(pct(waits, 95)))})
	}
	if late > 0 {
		p.add(finding.Finding{Check: "blocking", Rule: "§6.2.5.2", Status: finding.WARN, Message: fmt.Sprintf("%d reloads were held longer than three Target Durations (%ss) and still answered 200", late, secs(3*td)), Hint: "past three Target Durations the server SHOULD give up with 503, so the client can move on"})
	}
	if unavailable > 0 {
		p.add(finding.Finding{Check: "blocking", Rule: "§6.2.5.2", Status: finding.WARN, Message: fmt.Sprintf("%d of %d reloads ended in 503: the requested part did not appear within three Target Durations", unavailable, total), Value: finding.Num(float64(unavailable)), Unit: "responses", Hint: "the edge stalled; a player at PART-HOLD-BACK drains its buffer while it waits"})
	}
	if len(perPart) >= 3 && pl.PartTarget > 0 {
		med := pct(perPart, 50)
		st, note := finding.OK, "in step with PART-TARGET"
		if med > 1.5*pl.PartTarget {
			st, note = finding.WARN, "slower than PART-TARGET: the edge falls behind real time and the latency grows"
		}
		p.add(finding.Finding{Check: "cadence", Status: st, Message: fmt.Sprintf("a new part every %s (median), PART-TARGET %ss — %s", ms(med), secs(pl.PartTarget), note), Value: finding.Num(med), Unit: "s"})
	}
	if len(edgeAge) > 0 {
		p50, p95 := pct(edgeAge, 50), pct(edgeAge, 95)
		p.add(finding.Finding{Check: "edge-age", Status: finding.OK, Message: fmt.Sprintf("a part reaches a blocked client %s after its media ends (p50; p95 %s), by EXT-X-PROGRAM-DATE-TIME", ms(p50), ms(p95)), Value: finding.Num(p50), Unit: "s", Hint: "packaging and delivery delay, plus any offset between the packager's clock and this machine's"})
		hold := 3 * td
		if pl.Server.PartHoldBack != nil {
			hold = *pl.Server.PartHoldBack
		} else if pl.Server.HoldBack != nil {
			hold = *pl.Server.HoldBack
		}
		p.add(finding.Finding{Check: "latency", Status: finding.OK, Message: fmt.Sprintf("a player starting at the recommended hold-back plays %.2fs behind the media's wall clock (hold-back %ss + edge age %s)", hold+p50, secs(hold), ms(p50)), Value: finding.Num(hold + p50), Unit: "s"})
	}
	if len(offsets) > 0 {
		if off := pct(offsets, 50); off > 1.5 || off < -1.5 {
			p.add(finding.Finding{Check: "clock", Status: finding.WARN, Message: fmt.Sprintf("the server's Date header is %+.1fs from this machine's clock", off), Hint: "edge-age and latency compare PROGRAM-DATE-TIME with this machine's clock, so they include an offset of this order; sync the clock (NTP) and re-run"})
		}
	}
}

// advance counts the parts between two positions, with per parts to a segment.
func advance(om int64, op int, nm int64, np int, per int) int64 {
	pos := func(m int64, pt int) int64 {
		if pt < 0 {
			pt = per - 1
		}
		return m*int64(per) + int64(pt)
	}
	return pos(nm, np) - pos(om, op)
}

func pct(xs []float64, q float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	i := int(float64(len(s)-1) * q / 100)
	return s[i]
}

func reportsOK(n int) string {
	if n == 1 {
		return "the rendition report is within a part of the playlist it describes"
	}
	return fmt.Sprintf("%d rendition reports, each within a part of the playlist it describes", n)
}

func plural(n int64, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// secs formats seconds without float noise: 0.30000000000000004 -> "0.3".
func secs(f float64) string { return strconv.FormatFloat(math.Round(f*1000)/1000, 'f', -1, 64) }

func ms(sec float64) string {
	if sec < 1 && sec > -1 {
		return fmt.Sprintf("%.0fms", sec*1000)
	}
	return fmt.Sprintf("%.2fs", sec)
}

func mustURL(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		return &url.URL{Path: s}
	}
	return u
}

func errOrStatus(err error, r *response) any {
	if err != nil {
		return err
	}
	if r == nil {
		return "no response"
	}
	return fmt.Sprintf("HTTP %d", r.status)
}
