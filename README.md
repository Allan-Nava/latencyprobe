<h1 align="center">latencyprobe</h1>

<p align="center"><strong>How does your live origin answer a low-latency HLS player — not what the playlist claims.</strong></p>

<p align="center">
  <a href="https://github.com/Allan-Nava/latencyprobe/actions/workflows/ci.yml"><img alt="CI" src="https://github.com/Allan-Nava/latencyprobe/actions/workflows/ci.yml/badge.svg"></a>
  <a href="LICENSE"><img alt="License: MIT" src="https://img.shields.io/badge/license-MIT-10b981"></a>
  <img alt="Go" src="https://img.shields.io/github/go-mod/go-version/Allan-Nava/latencyprobe?color=10b981">
  <img alt="Zero dependencies" src="https://img.shields.io/badge/dependencies-0-10b981">
</p>

---

Low-latency HLS only works if the server plays its part of the protocol: it has to
**hold** a playlist request until the part the player asked for exists, answer delta
updates that merge cleanly, and keep rendition reports current. A playlist can declare
`CAN-BLOCK-RELOAD=YES` and a three-part `PART-HOLD-BACK` while the CDN in front of it
caches the playlist without its query string — and every player falls back to
polling, seconds behind live, with nothing in the playlist to say so.

**latencyprobe acts as the player.** It issues the blocking reloads, delta-update and
rendition-switch requests a low-latency client would, times the answers, and checks
each against the requirement it comes from.

```
$ latencyprobe https://cdn.example/live/master.m3u8

🔴 BAD    server-control    PART-HOLD-BACK=0.8s is below twice PART-TARGET (1s)  [§4.4.3.8]
                           ↳ PART-HOLD-BACK MUST be at least twice the Part Target Duration: closer than that, a player outruns the parts being published
🔴 BAD    delta             _HLS_skip=YES returned a full playlist (16s listed, Skip Boundary 12s)  [§6.2.5.1]
                           ↳ the server advertises CAN-SKIP-UNTIL, so it MUST answer with a Playlist Delta Update; a client that asked for one pays for the full playlist on every reload
🔴 BAD    blocking          12 of 12 blocking reloads came back without the requested part  [§6.2.5.2]
                           ↳ 12 of those responses carried an Age header: a cache is answering, most likely keyed without the query string, so _HLS_msn and _HLS_part never reach the origin
🟢 OK     bad-request       _HLS_part without _HLS_msn is refused with 400  [§6.2.5.2]
🟢 OK     bad-request       _HLS_msn=24, ten segments ahead, is refused at once  [§6.2.5.2]

6 findings: 3 OK, 0 WARN, 3 BAD, 0 ERROR — 19 requests in 6.2s
```

And a healthy origin, publishing each part 300 ms after its media ends:

```
🟢 OK     blocking          21 blocking reloads, each answered with the requested part; wait p50 499ms, p95 504ms  [§6.2.5.2]
🟢 OK     cadence           a new part every 500ms (median), PART-TARGET 0.5s — in step with PART-TARGET
🟢 OK     edge-age          a part reaches a blocked client 302ms after its media ends (p50; p95 305ms), by EXT-X-PROGRAM-DATE-TIME
🟢 OK     latency           a player starting at the recommended hold-back plays 1.80s behind the media's wall clock (hold-back 1.5s + edge age 302ms)
```

Both outputs come from [`llhls-origin`](#trying-it-without-an-origin), the local test
origin in this repository, with the faults switched on — no public low-latency HLS
stream was reachable to show instead.

**No ffmpeg, no cgo, no dependencies** — one static Go binary. It fetches playlists
only, never media.

## Install

```bash
go install github.com/Allan-Nava/latencyprobe/cmd/latencyprobe@latest
```

or a binary for Linux, macOS or Windows from
[Releases](https://github.com/Allan-Nava/latencyprobe/releases).

## Usage

```bash
latencyprobe https://cdn.example/live/master.m3u8              # first variant, 30 s
latencyprobe --variant 2 --duration 2m https://cdn.example/live/master.m3u8
latencyprobe --format json https://cdn.example/live/720p.m3u8
latencyprobe --exit-on bad https://cdn.example/live/master.m3u8  # gate CI or a deploy
```

Exit status is 0 whenever the probe ran, findings or not; `--exit-on warn|bad|error`
turns findings into exit 1. 2 is a usage error.

## What it checks

Every finding names the requirement it comes from in
[draft-pantos-hls-rfc8216bis-22](https://datatracker.ietf.org/doc/draft-pantos-hls-rfc8216bis/).
A MUST the server breaks is **BAD**, a SHOULD is **WARN**, and so is anything from
Appendix B.1, the low-latency server configuration profile.

| Check | What latencyprobe does | Rule |
|---|---|---|
| `server-control` | `HOLD-BACK` ≥ 3 Target Durations, `PART-HOLD-BACK` present with `EXT-X-PART-INF` and ≥ 2 (should be ≥ 3) `PART-TARGET`s, `CAN-SKIP-UNTIL` ≥ 6 Target Durations | §4.4.3.8 |
| `blocking` | Follows the live edge for `--duration` with `_HLS_msn` / `_HLS_part` requests, as a player does. Every response must contain the part asked for; one that comes back early means the origin is not blocking — or a cache in front of it ignores the query string, which the `Age` header gives away. Holds past three Target Durations should end in 503. | §6.2.5.2 |
| `bad-request` | `_HLS_part` without `_HLS_msn` must get 400; an `_HLS_msn` far past the edge should get an immediate 400, not a hang | §6.2.5.2 |
| `delta` | `_HLS_skip=YES` must return a delta update when the playlist is longer than the Skip Boundary; it may only skip segments beyond the boundary, and `SKIPPED-SEGMENTS` must line the remaining segments up with the full playlist | §6.2.5.1, §4.4.5.2 |
| `rendition-report` | Each `EXT-X-RENDITION-REPORT` checked against the rendition it describes, fetched straight after: not ahead of it, not more than a segment behind; one report per other rendition | §4.4.5.4, B.1 |
| `cadence` | How often a new part actually arrives, against `PART-TARGET` | — |
| `edge-age` | How long after a part's media ends (by `EXT-X-PROGRAM-DATE-TIME`) a blocked client receives it: packaging plus delivery delay | — |
| `latency` | What a player starting at the recommended hold-back actually plays behind wall clock: hold-back + edge age | — |
| `profile` | `EXT-X-PROGRAM-DATE-TIME` present; an `EXT-X-PRELOAD-HINT` for the next part | B.1 |

## Limits

- **Clocks.** `edge-age` and `latency` compare the packager's `PROGRAM-DATE-TIME` with
  this machine's clock. latencyprobe reads the server's `Date` header and warns when the
  two are more than 1.5 s apart, but within that the figures carry the offset. Run it
  from an NTP-synced machine.
- **One vantage point.** It measures the path from where it runs. A CDN edge near you
  can behave differently from one elsewhere; run it from where your viewers are.
- **Playlists, not media.** Whether the parts contain what the playlist says is a
  different question — [segcheck](https://github.com/Allan-Nava/segcheck) downloads the
  segments and parts and answers it.

## Trying it without an origin

`llhls-origin` serves a live low-latency playlist set on localhost and can break the
protocol on demand:

```bash
go run ./cmd/llhls-origin --stale-cache --age      # a CDN keyed without the query string
go run ./cmd/llhls-origin --lag 300ms              # each part listed 300 ms late
go run ./cmd/llhls-origin -h                       # every fault
```

then point latencyprobe at the URL it prints. The same origin runs in the tests, where
each fault is switched on and the finding it should cause is asserted.

## Licence

MIT
