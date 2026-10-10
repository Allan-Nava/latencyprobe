// Package playlist parses the parts of an HLS playlist that low-latency delivery
// depends on: the server-control attributes, Partial Segments, preload hints,
// rendition reports, delta-update skips and EXT-X-PROGRAM-DATE-TIME.
//
// It is not a general HLS parser. Tags this tool has no use for are skipped, and
// a playlist is accepted as long as the tags it does read are well formed.
//
// Section numbers refer to draft-pantos-hls-rfc8216bis-22.
package playlist

import (
	"bufio"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Media is a Media Playlist.
type Media struct {
	TargetDuration float64
	MediaSequence  int64
	PartTarget     float64 // EXT-X-PART-INF:PART-TARGET, 0 when absent
	Server         ServerControl
	// Segments in playlist order, each with its Media Sequence Number. The last
	// one may be Pending: Partial Segments of a Parent Segment that has no URI
	// yet, because it is still being produced.
	Segments []Segment
	Hints    []PreloadHint
	Reports  []RenditionReport
	// Skip is the EXT-X-SKIP tag of a Playlist Delta Update (§4.4.5.2), nil on a
	// full playlist. SkipTags counts them: the tag MUST NOT appear more than once.
	Skip     *Skip
	SkipTags int
	EndList  bool
}

// ServerControl is EXT-X-SERVER-CONTROL (§4.4.3.8). A nil float means absent.
type ServerControl struct {
	Present           bool
	CanBlockReload    bool
	HoldBack          *float64
	PartHoldBack      *float64
	CanSkipUntil      *float64
	CanSkipDateranges bool
}

// Segment is a Media Segment, or the Pending tail of Partial Segments.
type Segment struct {
	MSN      int64
	Duration float64 // EXTINF; 0 when Pending
	URI      string  // empty when Pending
	Pending  bool
	Parts    []Part
	// Start is the wall-clock time of the segment's first sample, from the
	// nearest EXT-X-PROGRAM-DATE-TIME at or before it. Zero when the playlist
	// carries none.
	Start time.Time
}

// Part is an EXT-X-PART (§4.4.4.9).
type Part struct {
	Index       int
	Duration    float64
	URI         string
	Independent bool
	Gap         bool
}

// PreloadHint is EXT-X-PRELOAD-HINT (§4.4.5.3).
type PreloadHint struct {
	Type string
	URI  string
}

// RenditionReport is EXT-X-RENDITION-REPORT (§4.4.5.4). LastPart is -1 when
// the attribute is absent.
type RenditionReport struct {
	URI      string
	LastMSN  int64
	LastPart int
	HasMSN   bool
}

// Skip is EXT-X-SKIP (§4.4.5.2).
type Skip struct {
	SkippedSegments int64
}

// Multivariant is a Multivariant Playlist: the Media Playlists it names.
type Multivariant struct {
	Variants []Variant
	// Renditions are EXT-X-MEDIA URIs. IFrames are EXT-X-I-FRAME-STREAM-INF URIs,
	// kept apart because rendition reports for them are only a SHOULD.
	Renditions []string
	IFrames    []string
}

// Variant is an EXT-X-STREAM-INF and the URI line that follows it.
type Variant struct {
	URI       string
	Bandwidth int64
}

// IsMultivariant reports whether text is a Multivariant Playlist.
func IsMultivariant(text string) bool {
	return strings.Contains(text, "#EXT-X-STREAM-INF") || strings.Contains(text, "#EXT-X-I-FRAME-STREAM-INF")
}

// ParseMultivariant reads the Media Playlist URIs out of a Multivariant Playlist.
func ParseMultivariant(text string) (*Multivariant, error) {
	m := &Multivariant{}
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	var pending *Variant
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "":
		case strings.HasPrefix(line, "#EXT-X-STREAM-INF:"):
			a := attrs(line[len("#EXT-X-STREAM-INF:"):])
			bw, _ := strconv.ParseInt(a["BANDWIDTH"], 10, 64)
			pending = &Variant{Bandwidth: bw}
		case strings.HasPrefix(line, "#EXT-X-MEDIA:"):
			if u := attrs(line[len("#EXT-X-MEDIA:"):])["URI"]; u != "" {
				m.Renditions = append(m.Renditions, u)
			}
		case strings.HasPrefix(line, "#EXT-X-I-FRAME-STREAM-INF:"):
			if u := attrs(line[len("#EXT-X-I-FRAME-STREAM-INF:"):])["URI"]; u != "" {
				m.IFrames = append(m.IFrames, u)
			}
		case strings.HasPrefix(line, "#"):
		default:
			if pending != nil {
				pending.URI = line
				m.Variants = append(m.Variants, *pending)
				pending = nil
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(m.Variants) == 0 {
		return nil, fmt.Errorf("multivariant playlist lists no variant streams")
	}
	return m, nil
}

// ParseMedia parses a Media Playlist.
func ParseMedia(text string) (*Media, error) {
	if !strings.HasPrefix(strings.TrimLeft(text, "\ufeff \t\r\n"), "#EXTM3U") {
		return nil, fmt.Errorf("not a playlist: no #EXTM3U header")
	}
	p := &Media{}
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 1<<20), 1<<24)

	var (
		cur      = Segment{}
		extinf   = -1.0
		pdt      time.Time
		seqSet   bool
		segments []Segment
	)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		tag, val, _ := strings.Cut(line, ":")
		switch {
		case line == "":
		case tag == "#EXT-X-TARGETDURATION":
			p.TargetDuration, _ = strconv.ParseFloat(val, 64)
		case tag == "#EXT-X-MEDIA-SEQUENCE":
			n, err := strconv.ParseInt(val, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("EXT-X-MEDIA-SEQUENCE %q: %w", val, err)
			}
			p.MediaSequence, seqSet = n, true
		case tag == "#EXT-X-PART-INF":
			p.PartTarget = num(attrs(val)["PART-TARGET"])
		case tag == "#EXT-X-SERVER-CONTROL":
			a := attrs(val)
			p.Server = ServerControl{
				Present:           true,
				CanBlockReload:    a["CAN-BLOCK-RELOAD"] == "YES",
				HoldBack:          optNum(a, "HOLD-BACK"),
				PartHoldBack:      optNum(a, "PART-HOLD-BACK"),
				CanSkipUntil:      optNum(a, "CAN-SKIP-UNTIL"),
				CanSkipDateranges: a["CAN-SKIP-DATERANGES"] == "YES",
			}
		case tag == "#EXT-X-SKIP":
			n, _ := strconv.ParseInt(attrs(val)["SKIPPED-SEGMENTS"], 10, 64)
			p.SkipTags++
			if p.Skip == nil {
				p.Skip = &Skip{SkippedSegments: n}
			}
		case tag == "#EXT-X-PROGRAM-DATE-TIME":
			t, err := parseTime(val)
			if err != nil {
				return nil, fmt.Errorf("EXT-X-PROGRAM-DATE-TIME %q: %w", val, err)
			}
			pdt = t
		case tag == "#EXTINF":
			d, _, _ := strings.Cut(val, ",")
			extinf = num(d)
		case tag == "#EXT-X-PART":
			a := attrs(val)
			cur.Parts = append(cur.Parts, Part{
				Index:       len(cur.Parts),
				Duration:    num(a["DURATION"]),
				URI:         a["URI"],
				Independent: a["INDEPENDENT"] == "YES",
				Gap:         a["GAP"] == "YES",
			})
		case tag == "#EXT-X-PRELOAD-HINT":
			a := attrs(val)
			p.Hints = append(p.Hints, PreloadHint{Type: a["TYPE"], URI: a["URI"]})
		case tag == "#EXT-X-RENDITION-REPORT":
			a := attrs(val)
			r := RenditionReport{URI: a["URI"], LastPart: -1}
			if v, ok := a["LAST-MSN"]; ok {
				r.LastMSN, _ = strconv.ParseInt(v, 10, 64)
				r.HasMSN = true
			}
			if v, ok := a["LAST-PART"]; ok {
				r.LastPart, _ = strconv.Atoi(v)
			}
			p.Reports = append(p.Reports, r)
		case tag == "#EXT-X-ENDLIST":
			p.EndList = true
		case strings.HasPrefix(line, "#"):
		default: // a segment URI closes the segment
			if extinf < 0 {
				return nil, fmt.Errorf("segment %q has no EXTINF", line)
			}
			cur.Duration, cur.URI = extinf, line
			if !pdt.IsZero() {
				cur.Start = pdt
				pdt = time.Time{}
			}
			segments = append(segments, cur)
			cur, extinf = Segment{}, -1
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if p.TargetDuration <= 0 {
		return nil, fmt.Errorf("no EXT-X-TARGETDURATION")
	}
	if len(cur.Parts) > 0 {
		cur.Pending = true
		if !pdt.IsZero() {
			cur.Start = pdt
		}
		segments = append(segments, cur)
	}
	_ = seqSet // absent means 0 (§4.4.3.2)

	first := p.MediaSequence
	if p.Skip != nil {
		first += p.Skip.SkippedSegments
	}
	for i := range segments {
		segments[i].MSN = first + int64(i)
	}
	// Carry PROGRAM-DATE-TIME forward across segments that have none of their
	// own: each starts where the previous one ended.
	for i := 1; i < len(segments); i++ {
		if segments[i].Start.IsZero() && !segments[i-1].Start.IsZero() {
			segments[i].Start = segments[i-1].Start.Add(seconds(segments[i-1].Duration))
		}
	}
	p.Segments = segments
	return p, nil
}

// Last is the newest media position the playlist describes: the MSN and Part
// Index of its last Partial Segment, or the last segment with part -1 when the
// playlist has no parts.
func (p *Media) Last() (msn int64, part int) {
	if len(p.Segments) == 0 {
		return p.MediaSequence - 1, -1
	}
	s := p.Segments[len(p.Segments)-1]
	if len(s.Parts) > 0 && (s.Pending || p.PartTarget > 0) {
		return s.MSN, len(s.Parts) - 1
	}
	return s.MSN, -1
}

// Next is what a client blocks on next (§6.3.8): the Part after the last one,
// or the segment after the last one when the playlist has no parts. A part
// index past the end of its Parent Segment is the server's to roll over to part
// 0 of the next segment (§6.2.5.2).
func (p *Media) Next() (msn int64, part int) {
	msn, part = p.Last()
	switch {
	case part >= 0:
		return msn, part + 1
	case p.PartTarget > 0:
		return msn + 1, 0
	default:
		return msn + 1, -1
	}
}

// Contains reports whether the playlist holds the given position: segment msn
// complete, or (when part >= 0) its Partial Segment with that index. A segment
// whose parts were trimmed counts as containing every part.
func (p *Media) Contains(msn int64, part int) bool {
	for _, s := range p.Segments {
		if s.MSN < msn {
			continue
		}
		if s.MSN > msn {
			return true
		}
		if part < 0 {
			return !s.Pending
		}
		if part < len(s.Parts) {
			return true
		}
		if s.Pending {
			return false
		}
		if len(s.Parts) == 0 {
			return true // parts trimmed from a complete segment
		}
		// A Part Index past the last part of its Parent Segment is a request for
		// part 0 of the next segment (§6.2.5.2).
		return p.Contains(msn+1, 0)
	}
	return false
}

// EdgeEnd is the wall-clock time at which the newest media in the playlist ends,
// from EXT-X-PROGRAM-DATE-TIME. ok is false when the playlist carries none.
func (p *Media) EdgeEnd() (t time.Time, ok bool) {
	if len(p.Segments) == 0 {
		return time.Time{}, false
	}
	s := p.Segments[len(p.Segments)-1]
	if s.Start.IsZero() {
		return time.Time{}, false
	}
	if s.Pending {
		d := 0.0
		for _, pt := range s.Parts {
			d += pt.Duration
		}
		return s.Start.Add(seconds(d)), true
	}
	return s.Start.Add(seconds(s.Duration)), true
}

// Duration is the total EXTINF duration of the complete segments listed.
func (p *Media) Duration() float64 {
	d := 0.0
	for _, s := range p.Segments {
		d += s.Duration
	}
	return d
}

// WithDirectives returns playlist URL u with the given _HLS_ delivery directives
// set (§6.2.5), replacing any already there and keeping every other parameter.
func WithDirectives(u *url.URL, kv ...string) *url.URL {
	c := *u
	q := c.Query()
	for _, k := range []string{"_HLS_msn", "_HLS_part", "_HLS_skip"} {
		q.Del(k)
	}
	for i := 0; i+1 < len(kv); i += 2 {
		q.Set(kv[i], kv[i+1])
	}
	c.RawQuery = q.Encode()
	return &c
}

func seconds(f float64) time.Duration { return time.Duration(f * float64(time.Second)) }

func num(s string) float64 {
	f, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return f
}

func optNum(a map[string]string, k string) *float64 {
	v, ok := a[k]
	if !ok {
		return nil
	}
	f := num(v)
	return &f
}

func parseTime(s string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999Z0700", "2006-01-02T15:04:05Z0700"} {
		if t, err := time.Parse(layout, strings.TrimSpace(s)); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("not an ISO-8601 date-time")
}

// attrs parses an attribute-list (§4.2): NAME=value pairs separated by commas,
// where a quoted-string value may itself contain commas.
func attrs(s string) map[string]string {
	out := map[string]string{}
	for len(s) > 0 {
		eq := strings.IndexByte(s, '=')
		if eq < 0 {
			break
		}
		name := strings.TrimSpace(s[:eq])
		s = s[eq+1:]
		var val string
		if strings.HasPrefix(s, `"`) {
			end := strings.IndexByte(s[1:], '"')
			if end < 0 {
				val, s = s[1:], ""
			} else {
				val, s = s[1:end+1], s[end+2:]
			}
		} else {
			comma := strings.IndexByte(s, ',')
			if comma < 0 {
				val, s = s, ""
			} else {
				val, s = s[:comma], s[comma:]
			}
		}
		out[name] = strings.TrimSpace(val)
		s = strings.TrimPrefix(strings.TrimLeft(s, " "), ",")
	}
	return out
}
