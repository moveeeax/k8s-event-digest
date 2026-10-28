package render

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/moveeeax/k8s-event-digest/internal/digest"
)

var now = time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)

func sample() digest.Result {
	return digest.Result{
		GeneratedAt:      now,
		Window:           "1h",
		NewWithin:        "15m",
		EventsConsidered: 85,
		EventsFiltered:   84,
		TotalOccurrences: 744,
		GroupsTotal:      2,
		Groups: []digest.Group{
			{
				Namespace: "prod", Reason: "FailedScheduling", Kind: "Pod",
				Root: "Deployment/api", Type: "Warning", Occurrences: 100, Objects: 40,
				First: now.Add(-12 * time.Minute), Last: now.Add(-20 * time.Second),
				Class: digest.ClassNew, Score: 12000,
				Message: "0/9 nodes are available: 6 Insufficient cpu | preemption: none",
			},
			{
				Namespace: "kube-system", Reason: "BackOff", Kind: "Pod",
				Root: "Workload/fluentbit", Type: "Warning", Occurrences: 255, Objects: 6,
				First: now.Add(-16 * time.Hour), Last: now.Add(-2 * time.Minute),
				Class: digest.ClassChronic, Score: 4590,
				Message: "Back-off restarting failed container",
			},
		},
	}
}

func render(t *testing.T, res digest.Result, format string) string {
	t.Helper()
	var buf bytes.Buffer
	if err := Write(&buf, res, format); err != nil {
		t.Fatalf("Write(%s): %v", format, err)
	}
	return buf.String()
}

func TestTableGroupsByNamespace(t *testing.T) {
	out := render(t, sample(), FormatTable)
	prodAt := strings.Index(out, "namespace: prod")
	sysAt := strings.Index(out, "namespace: kube-system")
	if prodAt < 0 || sysAt < 0 {
		t.Fatalf("namespace headers missing:\n%s", out)
	}
	if sysAt > prodAt {
		t.Error("namespaces should be printed in sorted order")
	}
	for _, want := range []string{"12m", "20s", "16h", "chronic", "new", "CLASS"} {
		if !strings.Contains(out, want) {
			t.Errorf("table missing %q:\n%s", want, out)
		}
	}
}

func TestMarkdownEscapesPipes(t *testing.T) {
	out := render(t, sample(), FormatMarkdown)
	if !strings.Contains(out, `Insufficient cpu \| preemption`) {
		t.Errorf("a pipe in a message would break the table:\n%s", out)
	}
	if !strings.Contains(out, "`Deployment/api`") {
		t.Errorf("roots should be code-formatted:\n%s", out)
	}
}

func TestJSONAlwaysHasGroupsArray(t *testing.T) {
	empty := digest.Result{GeneratedAt: now, Window: "1h", NewWithin: "15m"}
	out := render(t, empty, FormatJSON)
	if !strings.Contains(out, `"groups": []`) {
		t.Errorf("empty digest should emit [] not null:\n%s", out)
	}
}

func TestEmptyTableAndMarkdown(t *testing.T) {
	empty := digest.Result{GeneratedAt: now, Window: "1h", NewWithin: "15m", EventsConsidered: 3}
	if out := render(t, empty, FormatTable); !strings.Contains(out, "no events matched") {
		t.Errorf("table: %q", out)
	}
	if out := render(t, empty, FormatMarkdown); !strings.Contains(out, "_No events matched._") {
		t.Errorf("markdown: %q", out)
	}
}

func TestUnknownFormat(t *testing.T) {
	var buf bytes.Buffer
	if err := Write(&buf, sample(), "xml"); err == nil {
		t.Error("expected an error for an unknown format")
	}
}

func TestAgo(t *testing.T) {
	cases := map[time.Duration]string{
		30 * time.Second: "30s",
		5 * time.Minute:  "5m",
		3 * time.Hour:    "3h",
		47 * time.Hour:   "47h",
		50 * time.Hour:   "2d",
		96 * time.Hour:   "4d",
	}
	for d, want := range cases {
		if got := ago(now, now.Add(-d)); got != want {
			t.Errorf("ago(-%v) = %q, want %q", d, got, want)
		}
	}
	if got := ago(now, time.Time{}); got != "-" {
		t.Errorf("zero time = %q, want -", got)
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("short", 10); got != "short" {
		t.Errorf("got %q", got)
	}
	long := strings.Repeat("x", 100)
	got := truncate(long, 10)
	if len([]rune(got)) != 10 {
		t.Errorf("truncate produced %d runes: %q", len([]rune(got)), got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("truncated text should be marked: %q", got)
	}
}
