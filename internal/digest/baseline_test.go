package digest

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

func changeOf(t *testing.T, res Result, reason, root string) string {
	t.Helper()
	return find(t, res, reason, root).Change
}

// Acceptance: a baseline without a group marks it added, a baseline group
// that is absent now comes back as gone, and the rest are unchanged.
func TestBaselineAddedGoneUnchanged(t *testing.T) {
	events := load(t)
	opts := Defaults(now)

	before := opts
	before.IgnoreReasons = []string{"FailedScheduling"}
	base := Build(events, before)
	base.Groups = append(base.Groups, Group{
		Namespace: "prod", Reason: "OOMKilling", Kind: "Node", Root: "Node/ip-10-0-9-9",
		Type: "Warning", Occurrences: 4, First: now.Add(-2 * time.Hour), Last: now.Add(-90 * time.Minute),
	})

	opts.Baseline = &Baseline{GeneratedAt: now, Groups: base.Groups}
	res := Build(events, opts)

	if got := changeOf(t, res, "FailedScheduling", "Deployment/api"); got != ChangeAdded {
		t.Errorf("FailedScheduling change = %q, want added", got)
	}
	if got := changeOf(t, res, "BackOff", "Workload/fluentbit"); got != ChangeUnchanged {
		t.Errorf("BackOff change = %q, want unchanged", got)
	}
	if res.Baseline == nil {
		t.Fatal("Baseline summary missing")
	}
	if res.Baseline.Added != 1 || res.AddedGroups() != 1 {
		t.Errorf("Added = %d, want 1", res.Baseline.Added)
	}
	if res.Baseline.Unchanged != len(res.Groups)-1 {
		t.Errorf("Unchanged = %d, want %d", res.Baseline.Unchanged, len(res.Groups)-1)
	}
	if len(res.Gone) != 1 || res.Gone[0].Reason != "OOMKilling" || res.Gone[0].Change != ChangeGone {
		t.Fatalf("Gone = %+v, want the one OOMKilling group", res.Gone)
	}
	if res.Baseline.Gone != 1 {
		t.Errorf("Gone count = %d, want 1", res.Baseline.Gone)
	}
	if res.Baseline.WorseFactor != DefaultWorseFactor {
		t.Errorf("WorseFactor = %v, want the default", res.Baseline.WorseFactor)
	}
}

// Acceptance: occurrence rate up by more than the factor is worse; up by less
// is unchanged.
func TestBaselineWorseUsesRate(t *testing.T) {
	g := Group{Namespace: "prod", Reason: "BackOff", Kind: "Pod", Root: "Deployment/api",
		Type: "Warning", First: now.Add(-10 * time.Hour)}

	cur := g
	cur.Occurrences = 100 // 10/h at now
	cases := []struct {
		baseOcc int
		factor  float64
		want    string
	}{
		// The baseline was taken 2h ago, when the group was 8h old.
		{baseOcc: 40, factor: 2, want: ChangeUnchanged},  // 5/h -> 10/h is 2x, not more
		{baseOcc: 30, factor: 2, want: ChangeWorse},      // 3.75/h -> 10/h
		{baseOcc: 60, factor: 2, want: ChangeUnchanged},  // 7.5/h -> 10/h
		{baseOcc: 30, factor: 3, want: ChangeUnchanged},  // 2.67x under a 3x factor
		{baseOcc: 300, factor: 2, want: ChangeUnchanged}, // rate went down
	}
	for _, c := range cases {
		b := g
		b.Occurrences = c.baseOcc
		current := []Group{cur}
		compare(current, now, Baseline{GeneratedAt: now.Add(-2 * time.Hour), Groups: []Group{b}, WorseFactor: c.factor})
		if current[0].Change != c.want {
			t.Errorf("baseline %d occ, factor %v: change = %q, want %q",
				c.baseOcc, c.factor, current[0].Change, c.want)
		}
	}
}

func TestRateFloorsAgeAtOneMinute(t *testing.T) {
	g := Group{Occurrences: 6, First: now}
	if got := g.Rate(now); got != 360 {
		t.Errorf("Rate = %v, want 360/h (6 over a one-minute floor)", got)
	}
}

// --top must not turn trimmed groups into gone ones, and the added count
// covers groups the output no longer shows.
func TestBaselineComparesBeforeTop(t *testing.T) {
	events := load(t)
	full := Build(events, Defaults(now))

	opts := Defaults(now)
	opts.Top = 1
	opts.Baseline = &Baseline{GeneratedAt: now, Groups: full.Groups}
	res := Build(events, opts)
	if len(res.Groups) != 1 {
		t.Fatalf("Top 1 kept %d groups", len(res.Groups))
	}
	if len(res.Gone) != 0 {
		t.Errorf("groups trimmed by --top reported as gone: %+v", res.Gone)
	}

	opts.Baseline = &Baseline{GeneratedAt: now}
	res = Build(events, opts)
	if res.AddedGroups() != full.GroupsTotal {
		t.Errorf("AddedGroups = %d against an empty baseline, want all %d", res.AddedGroups(), full.GroupsTotal)
	}
}

func TestNoBaselineLeavesChangeEmpty(t *testing.T) {
	res := Build(load(t), Defaults(now))
	if res.Baseline != nil || res.Gone != nil || res.AddedGroups() != 0 {
		t.Error("a run without a baseline must not carry comparison fields")
	}
	for _, g := range res.Groups {
		if g.Change != "" {
			t.Errorf("%s/%s has change %q without a baseline", g.Reason, g.Root, g.Change)
		}
	}
}

func TestLoadBaselineFromDigestJSON(t *testing.T) {
	opts := Defaults(now)
	opts.Top = 2
	prev := Build(load(t), opts)
	data, err := json.Marshal(prev)
	if err != nil {
		t.Fatal(err)
	}

	base, truncated, err := LoadBaseline(data, Defaults(now))
	if err != nil {
		t.Fatal(err)
	}
	if !base.GeneratedAt.Equal(now) {
		t.Errorf("GeneratedAt = %v, want %v", base.GeneratedAt, now)
	}
	if len(base.Groups) != 2 {
		t.Errorf("Groups = %d, want 2", len(base.Groups))
	}
	if !truncated {
		t.Error("a digest cut by --top must be reported as truncated")
	}

	if _, _, err := LoadBaseline([]byte(`{"groups": []}`), Defaults(now)); err == nil {
		t.Error("a digest without generatedAt should be rejected")
	}
}

// A raw dump is digested as of its newest event with the current filters.
func TestLoadBaselineFromRawDump(t *testing.T) {
	data, err := os.ReadFile("testdata/cluster.json")
	if err != nil {
		t.Fatal(err)
	}
	opts := Defaults(now)
	opts.IgnoreReasons = []string{"FailedScheduling"}
	opts.Top = 1
	base, truncated, err := LoadBaseline(data, opts)
	if err != nil {
		t.Fatal(err)
	}
	if truncated {
		t.Error("a raw dump cannot be truncated")
	}
	newest := time.Time{}
	for _, e := range load(t) {
		if e.Last.After(newest) {
			newest = e.Last
		}
	}
	if !base.GeneratedAt.Equal(newest) {
		t.Errorf("GeneratedAt = %v, want the newest event %v", base.GeneratedAt, newest)
	}
	if len(base.Groups) < 2 {
		t.Errorf("--top leaked into the baseline: %d groups", len(base.Groups))
	}
	for _, g := range base.Groups {
		if g.Reason == "FailedScheduling" {
			t.Error("--ignore-reason was not applied to the baseline")
		}
	}

	if _, _, err := LoadBaseline([]byte("not json"), opts); err == nil {
		t.Error("garbage should be rejected")
	}
}
