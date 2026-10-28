package digest

import (
	"os"
	"testing"
	"time"

	"github.com/moveeeax/k8s-event-digest/internal/model"
)

var now = time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)

func load(t *testing.T) []model.Event {
	t.Helper()
	data, err := os.ReadFile("testdata/cluster.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	events, err := model.Decode(data)
	if err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	return events
}

func find(t *testing.T, res Result, reason, root string) Group {
	t.Helper()
	for _, g := range res.Groups {
		if g.Reason == reason && g.Root == root {
			return g
		}
	}
	t.Fatalf("group %s/%s not found in %d groups", reason, root, len(res.Groups))
	return Group{}
}

// Acceptance: forty Warning events from forty pods of one Deployment collapse
// into one group with objects=40, and occurrences sum each event's count
// rather than counting rows.
func TestFortyPodsCollapseIntoOneGroup(t *testing.T) {
	res := Build(load(t), Defaults(now))
	g := find(t, res, "FailedScheduling", "Deployment/api")

	if g.Objects != 40 {
		t.Errorf("Objects = %d, want 40", g.Objects)
	}
	if g.Occurrences != 100 {
		t.Errorf("Occurrences = %d, want 100 (summed counts, not 40 rows)", g.Occurrences)
	}
	if g.Type != model.TypeWarning {
		t.Errorf("Type = %q, want Warning", g.Type)
	}
	if g.Class != ClassNew {
		t.Errorf("Class = %q, want new (first seen 12m ago, --new-within 15m)", g.Class)
	}
	if len(g.ObjectNames) != 40 {
		t.Errorf("ObjectNames = %d entries, want 40", len(g.ObjectNames))
	}
}

// Acceptance: a new group outranks a chronic group with more occurrences.
func TestNewSortsAboveHigherCountChronic(t *testing.T) {
	res := Build(load(t), Defaults(now))
	if len(res.Groups) == 0 {
		t.Fatal("no groups")
	}

	top := res.Groups[0]
	if top.Reason != "FailedScheduling" || top.Class != ClassNew {
		t.Fatalf("top group = %s/%s (%s), want the new FailedScheduling group",
			top.Reason, top.Root, top.Class)
	}

	gc := find(t, res, "ImageGCFailed", "Node/ip-10-0-3-17.eu-central-1.compute.internal")
	if gc.Class != ClassChronic {
		t.Errorf("ImageGCFailed class = %q, want chronic", gc.Class)
	}
	if gc.Occurrences <= top.Occurrences {
		t.Fatalf("fixture is not exercising the rule: chronic has %d occurrences, new has %d",
			gc.Occurrences, top.Occurrences)
	}
	for i, g := range res.Groups {
		if g.Reason == "ImageGCFailed" && i == 0 {
			t.Error("a chronic group outranked a new one")
		}
	}
}

func TestChronicVersusBurstVersusSteady(t *testing.T) {
	res := Build(load(t), Defaults(now))

	backoff := find(t, res, "BackOff", "Workload/fluentbit")
	if backoff.Class != ClassChronic {
		t.Errorf("fluentbit BackOff = %q, want chronic (broken since last night)", backoff.Class)
	}
	mount := find(t, res, "FailedMount", "StatefulSet/pg")
	if mount.Class != ClassSteady {
		t.Errorf("pg FailedMount = %q, want steady (4h38m span, under --chronic-after)", mount.Class)
	}
	if mount.Objects != 2 {
		t.Errorf("pg FailedMount objects = %d, want 2", mount.Objects)
	}

	// Same group, two generations of rows: a trickle an hour ago and a spike in
	// the last ten minutes. Too old to be new, too concentrated to be chronic.
	ledger := find(t, res, "Unhealthy", "Deployment/ledger")
	if ledger.Class != ClassBurst {
		t.Errorf("ledger Unhealthy = %q, want burst", ledger.Class)
	}
	if ledger.Occurrences != 68 || ledger.Objects != 22 {
		t.Errorf("ledger = %d occurrences over %d objects, want 68/22",
			ledger.Occurrences, ledger.Objects)
	}
}

// A long-running row whose last timestamp happens to be recent must not be
// mistaken for a spike: 291 occurrences spread over three days put roughly one
// occurrence in the last fifteen minutes.
func TestChronicRowIsNotABurst(t *testing.T) {
	first := now.Add(-72 * time.Hour)
	chronic := model.Event{
		Namespace: "default", Reason: "ImageGCFailed", Type: model.TypeWarning, Count: 291,
		First: first, Last: now.Add(-5 * time.Minute),
		Object: model.ObjectRef{Kind: "Node", Name: "ip-10-0-3-17"},
		Root:   "Node/ip-10-0-3-17",
	}
	opts := Defaults(now)
	opts.Window = 0
	g := Build([]model.Event{chronic}, opts).Groups[0]
	if g.Class != ClassChronic {
		t.Errorf("Class = %q, want chronic", g.Class)
	}

	// The same count squeezed into ten minutes is a burst.
	spike := chronic
	spike.First = now.Add(-10 * time.Minute)
	spike.Last = now.Add(-1 * time.Minute)
	opts.NewWithin = 8 * time.Minute // keep it out of the "new" bucket
	if g := Build([]model.Event{spike}, opts).Groups[0]; g.Class != ClassBurst {
		t.Errorf("Class = %q, want burst", g.Class)
	}
}

// Acceptance: --window drops events whose lastTimestamp is older than it.
func TestWindowExcludesOldEvents(t *testing.T) {
	events := load(t)

	opts := Defaults(now)
	opts.Window = time.Hour
	short := Build(events, opts)
	for _, g := range short.Groups {
		if g.Root == "Deployment/checkout" {
			t.Fatal("checkout was last seen 2.5h ago and must be outside a 1h window")
		}
	}

	opts.Window = 4 * time.Hour
	wide := Build(events, opts)
	g := find(t, wide, "Unhealthy", "Deployment/checkout")
	if g.Occurrences != 17 {
		t.Errorf("Unhealthy occurrences = %d, want 17", g.Occurrences)
	}
	if wide.EventsFiltered <= short.EventsFiltered {
		t.Errorf("a wider window kept %d events, a 1h window kept %d",
			wide.EventsFiltered, short.EventsFiltered)
	}
}

// Acceptance: --ignore-reason removes a group entirely.
func TestIgnoreReasonRemovesGroup(t *testing.T) {
	opts := Defaults(now)
	opts.IgnoreReasons = []string{"failedscheduling"} // case-insensitive on purpose
	res := Build(load(t), opts)
	for _, g := range res.Groups {
		if g.Reason == "FailedScheduling" {
			t.Fatal("ignored reason still present")
		}
	}
	if res.NewGroups() != 0 {
		t.Errorf("NewGroups = %d after ignoring the only new reason", res.NewGroups())
	}
}

func TestFiltersAndLimits(t *testing.T) {
	events := load(t)

	opts := Defaults(now)
	opts.TypeFilter = FilterWarning
	warnings := Build(events, opts)
	for _, g := range warnings.Groups {
		if g.Type != model.TypeWarning {
			t.Fatalf("--type warning returned %q", g.Type)
		}
	}

	opts = Defaults(now)
	opts.TypeFilter = FilterNormal
	normals := Build(events, opts)
	if len(normals.Groups) == 0 {
		t.Fatal("--type normal returned nothing; the fixture has Normal events")
	}
	for _, g := range normals.Groups {
		if g.Type == model.TypeWarning {
			t.Fatalf("--type normal returned %q", g.Type)
		}
	}

	opts = Defaults(now)
	opts.Namespace = "kube-system"
	ns := Build(events, opts)
	for _, g := range ns.Groups {
		if g.Namespace != "kube-system" {
			t.Fatalf("--namespace leaked %q", g.Namespace)
		}
	}

	opts = Defaults(now)
	opts.Kind = "Node"
	kinds := Build(events, opts)
	if len(kinds.Groups) != 1 || kinds.Groups[0].Kind != "Node" {
		t.Fatalf("--kind Node returned %d groups", len(kinds.Groups))
	}

	opts = Defaults(now)
	opts.MinCount = 50
	min := Build(events, opts)
	for _, g := range min.Groups {
		if g.Occurrences < 50 {
			t.Fatalf("--min-count 50 kept a group with %d", g.Occurrences)
		}
	}

	opts = Defaults(now)
	opts.Top = 2
	top := Build(events, opts)
	if len(top.Groups) != 2 {
		t.Fatalf("--top 2 returned %d groups", len(top.Groups))
	}
	if top.GroupsTotal <= 2 {
		t.Errorf("GroupsTotal = %d, should report the pre-truncation count", top.GroupsTotal)
	}
}

// Blast radius is a multiplier: the same number of complaints spread over more
// objects has to rank higher.
func TestScoreRewardsBlastRadiusAndWarnings(t *testing.T) {
	base := now.Add(-2 * time.Hour)
	mk := func(name, typ string, count int) model.Event {
		return model.Event{
			Namespace: "x", Reason: "R", Type: typ, Count: count,
			First: base, Last: base.Add(time.Minute),
			Object: model.ObjectRef{Kind: "Pod", Name: name, Namespace: "x"},
			Root:   "Workload/" + name,
		}
	}
	spread := []model.Event{mk("a-aaaaa", model.TypeWarning, 5), mk("b-bbbbb", model.TypeWarning, 5)}
	spread[1].Root = spread[0].Root // same root, two objects
	single := []model.Event{mk("c-ccccc", model.TypeWarning, 10)}

	opts := Defaults(now)
	opts.Window = 0
	sp := Build(spread, opts).Groups[0]
	si := Build(single, opts).Groups[0]
	if sp.Occurrences != si.Occurrences {
		t.Fatalf("setup: %d vs %d occurrences", sp.Occurrences, si.Occurrences)
	}
	if sp.Score <= si.Score {
		t.Errorf("two objects scored %v, one object scored %v", sp.Score, si.Score)
	}

	warn := Build([]model.Event{mk("d-ddddd", model.TypeWarning, 4)}, opts).Groups[0]
	norm := Build([]model.Event{mk("e-eeeee", model.TypeNormal, 4)}, opts).Groups[0]
	if warn.Score <= norm.Score {
		t.Errorf("Warning scored %v, Normal scored %v", warn.Score, norm.Score)
	}
}

func TestWindowZeroKeepsEverything(t *testing.T) {
	opts := Defaults(now)
	opts.Window = 0
	res := Build(load(t), opts)
	if res.EventsFiltered != res.EventsConsidered {
		t.Errorf("window 0 kept %d of %d events", res.EventsFiltered, res.EventsConsidered)
	}
	if res.Window != "all" {
		t.Errorf("Window = %q, want \"all\"", res.Window)
	}
}

func TestEmptyInput(t *testing.T) {
	res := Build(nil, Defaults(now))
	if len(res.Groups) != 0 || res.NewGroups() != 0 {
		t.Errorf("empty input produced %d groups", len(res.Groups))
	}
}
