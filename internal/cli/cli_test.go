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
	"github.com/moveeeax/k8s-event-digest/internal/nodes"
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

// writeBaseline captures a digest of the fixture with extra flags applied, the
// way a pre-deploy run would.
func writeBaseline(t *testing.T, extra ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "before.json")
	code, out, errOut := run(t, base(append([]string{"--format", "json"}, extra...)...)...)
	if code != ExitOK {
		t.Fatalf("baseline run exit %d: %s", code, errOut)
	}
	if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// Acceptance: --fail-on-added exits 1 when the run has a group the baseline
// lacks, and 0 when the baseline already had everything.
func TestFailOnAdded(t *testing.T) {
	without := writeBaseline(t, "--ignore-reason", "FailedScheduling")
	code, out, errOut := run(t, base("--baseline", without, "--fail-on-added", "--format", "json")...)
	if code != ExitFindings {
		t.Fatalf("exit %d, want %d: %s", code, ExitFindings, errOut)
	}
	var res digest.Result
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatal(err)
	}
	if res.Baseline == nil || res.Baseline.Added != 1 {
		t.Fatalf("baseline summary = %+v, want 1 added", res.Baseline)
	}
	for _, g := range res.Groups {
		want := digest.ChangeUnchanged
		if g.Reason == "FailedScheduling" && g.Root == "Deployment/api" {
			want = digest.ChangeAdded
		}
		if g.Change != want {
			t.Errorf("%s/%s change = %q, want %q", g.Reason, g.Root, g.Change, want)
		}
	}

	same := writeBaseline(t)
	if code, _, errOut := run(t, base("--baseline", same, "--fail-on-added")...); code != ExitOK {
		t.Errorf("exit %d against an identical baseline, want 0: %s", code, errOut)
	}

	// A raw dump works as a baseline too.
	if code, _, errOut := run(t, base("--baseline", "testdata/cluster.json", "--fail-on-added")...); code != ExitOK {
		t.Errorf("exit %d against the raw dump itself, want 0: %s", code, errOut)
	}

	// Without the gate an added group is reported but never fails the run.
	if code, _, _ := run(t, base("--baseline", without)...); code != ExitOK {
		t.Errorf("exit %d without --fail-on-added, want 0", code)
	}
}

func TestBaselineTableShowsChangeAndGone(t *testing.T) {
	// The baseline saw everything; the current run ignores BackOff, so that
	// group is gone.
	all := writeBaseline(t)
	code, out, errOut := run(t, base("--baseline", all, "--ignore-reason", "BackOff")...)
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	for _, want := range []string{"CHANGE", "unchanged", "vs baseline 2026-10-28T12:00:00Z", "1 gone", "gone since baseline:", "Workload/fluentbit"} {
		if !strings.Contains(out, want) {
			t.Errorf("table missing %q:\n%s", want, out)
		}
	}
}

func TestTruncatedBaselineWarns(t *testing.T) {
	cut := writeBaseline(t, "--top", "2")
	code, _, errOut := run(t, base("--baseline", cut)...)
	if code != ExitOK {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(errOut, "--top") {
		t.Errorf("a baseline cut by --top should warn, stderr: %q", errOut)
	}
}

func TestBaselineBadInputExitsTwo(t *testing.T) {
	cases := [][]string{
		base("--fail-on-added"),
		base("--baseline", "testdata/does-not-exist.json"),
		base("--baseline", "testdata/cluster.json", "--worse-factor", "0.5"),
		base("--baseline", "-", "--from-json", "-"),
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

func nodesBase(extra ...string) []string {
	return append([]string{"nodes", "--from-json", "testdata/nodes.json", "--now", fixtureNow}, extra...)
}

func TestNodesSubcommand(t *testing.T) {
	code, out, errOut := run(t, nodesBase()...)
	if code != ExitOK {
		t.Fatalf("exit %d, stderr: %s", code, errOut)
	}
	for _, want := range []string{"5 nodes: 1 not ready, 1 under pressure, 1 cordoned", "evicted by the kubelet:", "pending pods:", "cpu short by 780m"} {
		if !strings.Contains(out, want) {
			t.Errorf("nodes output missing %q\n%s", want, out)
		}
	}

	code, out, _ = run(t, nodesBase("--format", "json", "--since", "0", "--sort-by", "name")...)
	if code != ExitOK {
		t.Fatalf("json exit %d", code)
	}
	var rep nodes.Report
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if len(rep.Evictions) != 3 || rep.Since != "all" {
		t.Errorf("--since 0: %d evictions, since %q", len(rep.Evictions), rep.Since)
	}
	if rep.Nodes[0].Name != "ip-10-0-1-11.eu-central-1.compute.internal" {
		t.Errorf("--sort-by name: first node %s", rep.Nodes[0].Name)
	}
}

// Acceptance: --fail-on-pressure is the alerting hook: exit 1 on a NotReady
// or pressured node, 0 otherwise.
func TestNodesFailOnPressure(t *testing.T) {
	if code, _, _ := run(t, nodesBase("--fail-on-pressure")...); code != ExitFindings {
		t.Errorf("exit %d, want %d (the fixture has a NotReady and a pressured node)", code, ExitFindings)
	}

	healthy := filepath.Join(t.TempDir(), "healthy.json")
	payload := `{"kind":"List","items":[{"kind":"Node","metadata":{"name":"a"},
		"status":{"allocatable":{"cpu":"2","memory":"4Gi","pods":"110"},
		"conditions":[{"type":"Ready","status":"True"},{"type":"MemoryPressure","status":"False"}]}}]}`
	if err := os.WriteFile(healthy, []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := run(t, "nodes", "--from-json", healthy, "--fail-on-pressure"); code != ExitOK {
		t.Errorf("exit %d on a healthy node, want 0: %s", code, errOut)
	}
}

func TestNodesDumpRoundTrip(t *testing.T) {
	dump := filepath.Join(t.TempDir(), "nodes.json")
	code, first, _ := run(t, nodesBase("--dump-json", dump, "--format", "json")...)
	if code != ExitOK {
		t.Fatalf("exit %d", code)
	}
	code, second, _ := run(t, "nodes", "--from-json", dump, "--now", fixtureNow, "--format", "json")
	if code != ExitOK || first != second {
		t.Errorf("replaying the nodes dump produced a different report (exit %d)", code)
	}
}

func TestNodesBadInputExitsTwo(t *testing.T) {
	cases := [][]string{
		nodesBase("--since", "soon"),
		nodesBase("--sort-by", "age"),
		nodesBase("--format", "yaml"),
		nodesBase("--now", "yesterday"),
		nodesBase("extra"),
		{"nodes", "--from-json", "testdata/cluster.json"}, // an event dump has no nodes
		{"nodes", "--from-json", "testdata/does-not-exist.json"},
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
