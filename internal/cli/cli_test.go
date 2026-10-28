package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/moveeeax/k8s-event-digest/internal/digest"
)

const fixtureNow = "2026-10-28T12:00:00Z"

func run(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := Run(args, &out, &errb)
	return code, out.String(), errb.String()
}

func base(extra ...string) []string {
	return append([]string{"--from-json", "testdata/cluster.json", "--now", fixtureNow}, extra...)
}

func TestTableOutput(t *testing.T) {
	code, out, errOut := run(t, base()...)
	if code != ExitOK {
		t.Fatalf("exit %d, stderr: %s", code, errOut)
	}
	for _, want := range []string{"FailedScheduling", "Deployment/api", "namespace: prod", "new"} {
		if !strings.Contains(out, want) {
			t.Errorf("table output missing %q\n%s", want, out)
		}
	}
	// The default 1h window must hide the 2.5h-old readiness failures.
	if strings.Contains(out, "Deployment/checkout") {
		t.Error("an event outside the default window leaked into the table")
	}
}

func TestJSONOutputShape(t *testing.T) {
	code, out, errOut := run(t, base("--format", "json")...)
	if code != ExitOK {
		t.Fatalf("exit %d, stderr: %s", code, errOut)
	}
	var res digest.Result
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if len(res.Groups) == 0 {
		t.Fatal("no groups in JSON output")
	}
	if res.Groups[0].Class != digest.ClassNew {
		t.Errorf("first group class = %q, want new", res.Groups[0].Class)
	}
	if res.GeneratedAt.UTC().Format(time.RFC3339) != fixtureNow {
		t.Errorf("GeneratedAt = %v, --now was ignored", res.GeneratedAt)
	}
}

func TestMarkdownOutput(t *testing.T) {
	code, out, _ := run(t, base("--format", "markdown", "--top", "3")...)
	if code != ExitOK {
		t.Fatalf("exit %d", code)
	}
	if !strings.HasPrefix(out, "### Event digest") {
		t.Errorf("markdown should start with a heading:\n%s", out)
	}
	if strings.Count(out, "\n|") < 4 { // header, separator, 3 rows
		t.Errorf("expected a markdown table with 3 rows:\n%s", out)
	}
}

// Acceptance: --fail-on-new exits 1 when a new group exists, 0 when none does.
func TestFailOnNew(t *testing.T) {
	code, _, _ := run(t, base("--fail-on-new")...)
	if code != ExitFindings {
		t.Errorf("exit %d, want %d (the fixture has a new group)", code, ExitFindings)
	}

	code, _, _ = run(t, base("--fail-on-new", "--ignore-reason", "FailedScheduling")...)
	if code != ExitOK {
		t.Errorf("exit %d, want 0 once the only new reason is ignored", code)
	}

	// Without the flag a new group is reported but never gates.
	if code, _, _ := run(t, base()...); code != ExitOK {
		t.Errorf("exit %d without --fail-on-new, want 0", code)
	}
}

// Acceptance: an unparsable duration exits 2.
func TestBadInputExitsTwo(t *testing.T) {
	cases := [][]string{
		base("--window", "1 hour"),
		base("--new-within", "fifteen"),
		base("--chronic-after", "-3h"),
		base("--now", "yesterday"),
		base("--type", "scary"),
		base("--format", "yaml"),
		base("extra-positional"),
		{"--from-json", "testdata/does-not-exist.json"},
	}
	for _, args := range cases {
		code, _, errOut := run(t, args...)
		if code != ExitError {
			t.Errorf("%v: exit %d, want %d", args, code, ExitError)
		}
		if !strings.Contains(errOut, "error") {
			t.Errorf("%v: stderr lacks an explanation: %q", args, errOut)
		}
	}
}

func TestDayDurationSuffix(t *testing.T) {
	code, out, errOut := run(t, base("--window", "7d", "--chronic-after", "2d", "--format", "json")...)
	if code != ExitOK {
		t.Fatalf("exit %d, stderr: %s", code, errOut)
	}
	var res digest.Result
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatal(err)
	}
	if res.Window != "168h0m0s" {
		t.Errorf("Window = %q, want 168h0m0s", res.Window)
	}
	if res.EventsFiltered != res.EventsConsidered {
		t.Errorf("a 7d window kept %d of %d events", res.EventsFiltered, res.EventsConsidered)
	}
}

// Acceptance: --from-json reproduces a run with no API calls, and --dump-json
// round-trips through the same decoder.
func TestDumpJSONRoundTrip(t *testing.T) {
	dump := filepath.Join(t.TempDir(), "events.json")
	code, first, _ := run(t, base("--dump-json", dump, "--format", "json")...)
	if code != ExitOK {
		t.Fatalf("exit %d", code)
	}
	if _, err := os.Stat(dump); err != nil {
		t.Fatalf("dump not written: %v", err)
	}

	code, second, _ := run(t, "--from-json", dump, "--now", fixtureNow, "--format", "json")
	if code != ExitOK {
		t.Fatalf("replay exit %d", code)
	}
	if first != second {
		t.Error("replaying the dump produced a different digest")
	}
}

func TestVersionAndEmptyResult(t *testing.T) {
	code, out, _ := run(t, "--version")
	if code != ExitOK || !strings.Contains(out, "k8s-event-digest") {
		t.Errorf("--version: exit %d, out %q", code, out)
	}

	code, out, _ = run(t, base("--reason", "NoSuchReason")...)
	if code != ExitOK {
		t.Errorf("exit %d for an empty digest, want 0", code)
	}
	if !strings.Contains(out, "no events matched") {
		t.Errorf("empty digest should say so, got %q", out)
	}
}

func TestParseDuration(t *testing.T) {
	ok := map[string]time.Duration{
		"":      0,
		"0":     0,
		"90m":   90 * time.Minute,
		"1h30m": 90 * time.Minute,
		"2d":    48 * time.Hour,
		"0.5d":  12 * time.Hour,
	}
	for in, want := range ok {
		got, err := parseDuration("--window", in)
		if err != nil {
			t.Errorf("parseDuration(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("parseDuration(%q) = %v, want %v", in, got, want)
		}
	}
	for _, in := range []string{"1 hour", "d", "-5m", "soon", "15"} {
		if _, err := parseDuration("--window", in); err == nil {
			t.Errorf("parseDuration(%q) should have failed", in)
		}
	}
}
