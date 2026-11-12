package digest

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/moveeeax/k8s-event-digest/internal/model"
)

// Changes a group can carry when a run is compared against a baseline.
const (
	ChangeAdded     = "added"     // the group did not exist in the baseline
	ChangeWorse     = "worse"     // it existed, and its occurrence rate went up by more than WorseFactor
	ChangeUnchanged = "unchanged" // it existed, at a comparable rate
	ChangeGone      = "gone"      // it existed in the baseline and is absent now
)

// DefaultWorseFactor is the rate multiplier that turns an existing group worse.
const DefaultWorseFactor = 2.0

// minRateAge keeps a group first seen a second before the digest from having
// an absurd per-hour rate.
const minRateAge = time.Minute

// Baseline is a previous digest the current run is compared against.
type Baseline struct {
	GeneratedAt time.Time
	Groups      []Group
	WorseFactor float64 // 0 means DefaultWorseFactor
}

// BaselineSummary is what the comparison found, as it appears in the output.
type BaselineSummary struct {
	GeneratedAt time.Time `json:"generatedAt"`
	WorseFactor float64   `json:"worseFactor"`
	Added       int       `json:"added"`
	Worse       int       `json:"worse"`
	Unchanged   int       `json:"unchanged"`
	Gone        int       `json:"gone"`
}

// Rate is occurrences per hour since the group was first seen, as of at. A
// chronic complaint keeps a flat rate however long it runs; one that a deploy
// made louder piles up occurrences faster than it ages.
func (g Group) Rate(at time.Time) float64 {
	age := at.Sub(g.First)
	if age < minRateAge {
		age = minRateAge
	}
	return float64(g.Occurrences) / age.Hours()
}

// compare marks every current group added, worse or unchanged and returns the
// baseline groups that no longer exist. Identity is the grouping key, so a
// baseline captured with other filters simply does not line up.
func compare(current []Group, at time.Time, base Baseline) (BaselineSummary, []Group) {
	factor := base.WorseFactor
	if factor <= 0 {
		factor = DefaultWorseFactor
	}
	sum := BaselineSummary{GeneratedAt: base.GeneratedAt.UTC(), WorseFactor: factor}

	before := make(map[key]Group, len(base.Groups))
	for _, g := range base.Groups {
		before[groupKey(g)] = g
	}

	seen := make(map[key]struct{}, len(current))
	for i := range current {
		k := groupKey(current[i])
		seen[k] = struct{}{}
		old, ok := before[k]
		switch {
		case !ok:
			current[i].Change = ChangeAdded
			sum.Added++
		case current[i].Rate(at) > factor*old.Rate(base.GeneratedAt):
			current[i].Change = ChangeWorse
			sum.Worse++
		default:
			current[i].Change = ChangeUnchanged
			sum.Unchanged++
		}
	}

	gone := []Group{}
	for _, g := range base.Groups {
		if _, ok := seen[groupKey(g)]; ok {
			continue
		}
		g.Change = ChangeGone
		gone = append(gone, g)
	}
	sum.Gone = len(gone)
	return sum, gone
}

func groupKey(g Group) key {
	return key{g.Namespace, g.Reason, g.Kind, g.Root}
}

// LoadBaseline reads either a previous `--format json` digest or a raw event
// payload (a `--dump-json` file or a `kubectl get events -o json` dump).
//
// A digest is taken as it is: its groups are what that run saw. A raw payload
// is digested here with opts, minus --top, as of its newest event, because a
// dump does not record when it was taken. Truncated reports whether the
// baseline digest itself had been cut by --top, which makes groups it dropped
// look added.
func LoadBaseline(data []byte, opts Options) (base Baseline, truncated bool, err error) {
	var probe struct {
		Groups      *json.RawMessage `json:"groups"`
		GeneratedAt *time.Time       `json:"generatedAt"`
	}
	trimmed := strings.TrimSpace(string(data))
	if strings.HasPrefix(trimmed, "{") && json.Unmarshal(data, &probe) == nil && probe.Groups != nil {
		var prev Result
		if err := json.Unmarshal(data, &prev); err != nil {
			return Baseline{}, false, fmt.Errorf("decode baseline digest: %w", err)
		}
		if probe.GeneratedAt == nil || prev.GeneratedAt.IsZero() {
			return Baseline{}, false, fmt.Errorf("baseline digest has no generatedAt")
		}
		return Baseline{GeneratedAt: prev.GeneratedAt, Groups: prev.Groups}, prev.GroupsTotal > len(prev.Groups), nil
	}

	events, err := model.Decode(data)
	if err != nil {
		return Baseline{}, false, fmt.Errorf("baseline: %w", err)
	}
	at := time.Time{}
	for _, e := range events {
		if e.Last.After(at) {
			at = e.Last
		}
	}
	if at.IsZero() {
		return Baseline{}, false, fmt.Errorf("baseline events carry no timestamps")
	}
	opts.Now = at
	opts.Top = 0
	opts.Baseline = nil
	res := Build(events, opts)
	return Baseline{GeneratedAt: at, Groups: res.Groups}, false, nil
}
