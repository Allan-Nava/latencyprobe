package playlist

import (
	"net/url"
	"testing"
	"time"
)

// The Low-Latency Playlist example of §9.11, with the header lines a real one has.
const spec911 = `#EXTM3U
#EXT-X-TARGETDURATION:4
#EXT-X-VERSION:6
#EXT-X-SERVER-CONTROL:CAN-BLOCK-RELOAD=YES,PART-HOLD-BACK=6.0,CAN-SKIP-UNTIL=24.0
#EXT-X-PART-INF:PART-TARGET=2.0
#EXT-X-MEDIA-SEQUENCE:268
#EXT-X-PROGRAM-DATE-TIME:2026-10-10T12:00:00.000Z
#EXTINF:4.00008,
fileSequence268.mp4
#EXTINF:4.00008,
fileSequence269.mp4
#EXTINF:4.00008,
fileSequence270.mp4
#EXT-X-PART:DURATION=2.00004,INDEPENDENT=YES,URI="filePart271.0.mp4"
#EXT-X-PART:DURATION=2.00004,URI="filePart271.1.mp4"
#EXTINF:4.00008,
fileSequence271.mp4
#EXT-X-PART:DURATION=2.00004,INDEPENDENT=YES,URI="filePart272.0.mp4"
#EXT-X-PART:DURATION=0.50001,URI="filePart272.1.mp4"
#EXTINF:2.50005,
fileSequence272.mp4
#EXT-X-DISCONTINUITY
#EXT-X-PART:DURATION=2.00004,INDEPENDENT=YES,URI="midRoll273.0.mp4"
#EXT-X-PART:DURATION=2.00004,URI="midRoll273.1.mp4"
#EXTINF:4.00008,
midRoll273.mp4
#EXT-X-PART:DURATION=2.00004,INDEPENDENT=YES,URI="midRoll274.0.mp4"
#EXT-X-PRELOAD-HINT:TYPE=PART,URI="midRoll274.1.mp4"
#EXT-X-RENDITION-REPORT:URI="/1M/LL-HLS.m3u8",LAST-MSN=274,LAST-PART=1
`

func TestSpecExample(t *testing.T) {
	p, err := ParseMedia(spec911)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Server.CanBlockReload || *p.Server.PartHoldBack != 6 || *p.Server.CanSkipUntil != 24 || p.PartTarget != 2 {
		t.Fatalf("server control: %+v part target %v", p.Server, p.PartTarget)
	}
	if n := len(p.Segments); n != 7 {
		t.Fatalf("segments = %d, want 6 complete + 1 pending", n)
	}
	if msn, part := p.Last(); msn != 274 || part != 0 {
		t.Fatalf("Last = %d.%d, want 274.0", msn, part)
	}
	if msn, part := p.Next(); msn != 274 || part != 1 {
		t.Fatalf("Next = %d.%d, want 274.1", msn, part)
	}
	if !p.Contains(273, -1) || !p.Contains(274, 0) || p.Contains(274, 1) || p.Contains(274, -1) || !p.Contains(273, 2) || p.Contains(274, 2) {
		t.Fatal("Contains disagrees with the playlist")
	}
	if len(p.Hints) != 1 || p.Hints[0].URI != "midRoll274.1.mp4" {
		t.Fatalf("hints = %+v", p.Hints)
	}
	r := p.Reports[0]
	if r.URI != "/1M/LL-HLS.m3u8" || r.LastMSN != 274 || r.LastPart != 1 {
		t.Fatalf("report = %+v", r)
	}
	// 4.00008*5 + 2.50005 after the first PDT, then the pending part's 2.00004.
	end, ok := p.EdgeEnd()
	want := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC).Add(time.Duration((4.00008*5 + 2.50005 + 2.00004) * float64(time.Second)))
	if !ok || end.Sub(want).Abs() > time.Microsecond {
		t.Fatalf("EdgeEnd = %v, want %v", end, want)
	}
}

func TestDeltaUpdateNumbersAfterTheSkip(t *testing.T) {
	p, err := ParseMedia("#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:100\n#EXT-X-SKIP:SKIPPED-SEGMENTS=30\n#EXTINF:2,\na.ts\n#EXTINF:2,\nb.ts\n")
	if err != nil {
		t.Fatal(err)
	}
	if p.Segments[0].MSN != 130 || p.Segments[1].MSN != 131 || p.SkipTags != 1 {
		t.Fatalf("got %+v", p.Segments)
	}
}

func TestNextWithoutParts(t *testing.T) {
	p, _ := ParseMedia("#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXT-X-MEDIA-SEQUENCE:7\n#EXTINF:6,\na.ts\n")
	if msn, part := p.Next(); msn != 8 || part != -1 {
		t.Fatalf("Next = %d.%d", msn, part)
	}
	p.PartTarget = 1
	if msn, part := p.Next(); msn != 8 || part != 0 {
		t.Fatalf("Next with PART-INF and trimmed parts = %d.%d, want 8.0", msn, part)
	}
}

func TestAttrsQuotedCommas(t *testing.T) {
	a := attrs(`URI="a,b.m3u8",LAST-MSN=3, LAST-PART=2`)
	if a["URI"] != "a,b.m3u8" || a["LAST-MSN"] != "3" || a["LAST-PART"] != "2" {
		t.Fatalf("%v", a)
	}
}

func TestMultivariant(t *testing.T) {
	m, err := ParseMultivariant("#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"a\",NAME=\"en\",URI=\"audio.m3u8\"\n#EXT-X-STREAM-INF:BANDWIDTH=800000\nlow.m3u8\n#EXT-X-STREAM-INF:BANDWIDTH=3000000\nhigh.m3u8\n#EXT-X-I-FRAME-STREAM-INF:BANDWIDTH=90000,URI=\"if.m3u8\"\n")
	if err != nil || len(m.Variants) != 2 || m.Variants[1].URI != "high.m3u8" || m.Renditions[0] != "audio.m3u8" || m.IFrames[0] != "if.m3u8" {
		t.Fatalf("%+v %v", m, err)
	}
}

func TestWithDirectivesKeepsOtherParameters(t *testing.T) {
	u, _ := url.Parse("https://cdn.example/live.m3u8?token=abc&_HLS_msn=1")
	got := WithDirectives(u, "_HLS_msn", "42", "_HLS_part", "3").String()
	if got != "https://cdn.example/live.m3u8?_HLS_msn=42&_HLS_part=3&token=abc" {
		t.Fatal(got)
	}
}
