package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Allan-Nava/latencyprobe/internal/llhlstest"
)

func TestUsageErrorsExit2(t *testing.T) {
	for _, args := range [][]string{{}, {"a", "b"}, {"--format", "xml", "u"}, {"--exit-on", "ok", "u"}, {"--duration", "0s", "u"}, {"--nope"}} {
		var out, errb bytes.Buffer
		if code := run(args, &out, &errb); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
}

func TestExitOnGatesOnFindings(t *testing.T) {
	o := llhlstest.New(0.1, 4, 8, llhlstest.Faults{NoBlock: true})
	defer o.Close()
	var out, errb bytes.Buffer
	if code := run([]string{"--duration", "700ms", o.URL + "/master.m3u8"}, &out, &errb); code != 0 {
		t.Fatalf("without --exit-on a run that found problems exits %d, want 0", code)
	}
	if !strings.Contains(out.String(), "🔴 BAD") || !strings.Contains(out.String(), "[§6.2.5.2]") {
		t.Fatalf("text output:\n%s", out.String())
	}
	out.Reset()
	if code := run([]string{"--duration", "700ms", "--exit-on", "bad", "--format", "json", o.URL + "/master.m3u8"}, &out, &errb); code != 1 {
		t.Fatalf("--exit-on bad exits %d, want 1", code)
	}
	var r struct {
		Findings []struct{ Check, Status string }
	}
	if err := json.Unmarshal(out.Bytes(), &r); err != nil || len(r.Findings) == 0 || r.Findings[0].Status != "BAD" {
		t.Fatalf("json: %v %s", err, out.String())
	}
}
