// Command latencyprobe behaves like a low-latency HLS player towards a live
// origin and reports how the server answers: whether blocking reloads block,
// how soon a new part reaches a client, whether delta updates and rendition
// reports agree with the playlists they summarise.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/Allan-Nava/latencyprobe/internal/finding"
	"github.com/Allan-Nava/latencyprobe/internal/probe"
	"github.com/Allan-Nava/latencyprobe/internal/report"
)

var version = "dev"

const usage = `latencyprobe — how a live origin answers a low-latency HLS player.

Usage:
  latencyprobe [flags] <playlist URL>

The URL is a Media Playlist or a Multivariant Playlist (its first variant is
followed unless --variant says otherwise).

Flags:
  --duration D     follow the live edge with blocking reloads for D (default 30s)
  --variant N      which variant of a multivariant playlist to follow (default 0)
  --format F       text (default), json or markdown
  --exit-on S      exit 1 when a finding reaches warn|bad|error (default: never)
  --version        print the version

Exit status is 0 whenever the probe ran, findings or not; --exit-on gates CI on
them. 2 is a usage error.

Examples:
  latencyprobe https://cdn.example/live/master.m3u8
  latencyprobe --duration 2m --exit-on bad https://cdn.example/live/720p.m3u8
`

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("latencyprobe", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	duration := fs.Duration("duration", 30*time.Second, "")
	variant := fs.Int("variant", 0, "")
	format := fs.String("format", "text", "")
	exitOn := fs.String("exit-on", "", "")
	showVersion := fs.Bool("version", false, "")
	help := fs.Bool("help", false, "")
	fs.BoolVar(help, "h", false, "")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(stderr, "%v\n\n%s", err, usage)
		return 2
	}
	if *help {
		fmt.Fprint(stdout, usage)
		return 0
	}
	if *showVersion {
		fmt.Fprintln(stdout, version)
		return 0
	}
	if fs.NArg() != 1 {
		fmt.Fprintf(stderr, "latencyprobe takes one playlist URL, got %d\n\n%s", fs.NArg(), usage)
		return 2
	}
	if *format != "text" && *format != "json" && *format != "markdown" {
		fmt.Fprintf(stderr, "--format must be text, json or markdown, got %q\n", *format)
		return 2
	}
	threshold := finding.Status(strings.ToUpper(*exitOn))
	if *exitOn != "" && threshold != finding.WARN && threshold != finding.BAD && threshold != finding.ERROR {
		fmt.Fprintf(stderr, "--exit-on must be warn, bad or error, got %q\n", *exitOn)
		return 2
	}
	if *duration <= 0 {
		fmt.Fprintln(stderr, "--duration must be positive")
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if *format == "text" {
		fmt.Fprintf(stderr, "following the live edge for %s…\n", *duration)
	}
	r := probe.Run(ctx, probe.Options{
		URL:      fs.Arg(0),
		Duration: *duration,
		Variant:  *variant,
		Client:   &http.Client{Transport: http.DefaultTransport},
	})
	switch *format {
	case "json":
		if err := report.JSON(stdout, r); err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
	case "markdown":
		report.Markdown(stdout, r)
	default:
		report.Text(stdout, r)
	}
	if *exitOn != "" && finding.AtLeast(r.Worst(), threshold) {
		return 1
	}
	return 0
}
