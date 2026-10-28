// Package digest turns a flat slice of events into ranked groups.
//
// The whole package is pure: it takes events and options in, hands a Result
// back, and never touches the network or the clock. That is what makes every
// rule testable against a JSON fixture with no cluster.
package digest

import (
	"sort"
	"strings"
	"time"

	"github.com/moveeeax/k8s-event-digest/internal/model"
)

// Classes a group can fall into.
const (
	ClassNew     = "new"
	ClassBurst   = "burst"
	ClassChronic = "chronic"
	ClassSteady  = "steady"
)

// Type filters accepted on the command line.
const (
	FilterWarning = "warning"
	FilterNormal  = "normal"
	FilterAll     = "all"
)

// Options controls filtering, classification and ranking.
type Options struct {
	Now           time.Time
	Window        time.Duration // 0 disables the window
	NewWithin     time.Duration
	ChronicAfter  time.Duration
	BurstFraction float64 // share of occurrences inside NewWithin that makes a group a burst
	Namespace     string
	Reason        string
	Kind          string
	TypeFilter    string
	IgnoreReasons []string
	MinCount      int
	Top           int
}

// Defaults returns the option set the CLI starts from.
func Defaults(now time.Time) Options {
	return Options{
		Now:           now,
		Window:        time.Hour,
		NewWithin:     15 * time.Minute,
		ChronicAfter:  6 * time.Hour,
		BurstFraction: 0.5,
		TypeFilter:    FilterAll,
	}
}

// Group is one collapsed complaint: a reason, about one kind of object, under
// one controller root, in one namespace.
type Group struct {
	Namespace   string    `json:"namespace"`
	Reason      string    `json:"reason"`
	Kind        string    `json:"kind"`
	Root        string    `json:"root"`
	Type        string    `json:"type"`
	Occurrences int       `json:"occurrences"`
	Objects     int       `json:"objects"`
	ObjectNames []string  `json:"objectNames"`
	First       time.Time `json:"firstSeen"`
	Last        time.Time `json:"lastSeen"`
	Class       string    `json:"class"`
	Score       float64   `json:"score"`
	Message     string    `json:"message"`
	Sources     []string  `json:"sources,omitempty"`
}

// Span is how long the group has been complaining.
func (g Group) Span() time.Duration { return g.Last.Sub(g.First) }

// Result is a full digest run.
type Result struct {
	GeneratedAt      time.Time `json:"generatedAt"`
	Window           string    `json:"window"`
	NewWithin        string    `json:"newWithin"`
	EventsConsidered int       `json:"eventsConsidered"`
	EventsFiltered   int       `json:"eventsFiltered"`
	TotalOccurrences int       `json:"totalOccurrences"`
	GroupsTotal      int       `json:"groupsTotal"`
	Groups           []Group   `json:"groups"`
}

// NewGroups counts groups classified as new, which is what --fail-on-new acts on.
func (r Result) NewGroups() int {
	n := 0
	for _, g := range r.Groups {
		if g.Class == ClassNew {
			n++
		}
	}
	return n
}

type key struct {
	namespace, reason, kind, root string
}

type acc struct {
	group    Group
	objects  map[string]struct{}
	sources  map[string]struct{}
	recent   float64
	lastSeen time.Time
}

// Build filters, groups, classifies and ranks.
func Build(events []model.Event, opts Options) Result {
	if opts.BurstFraction <= 0 {
		opts.BurstFraction = 0.5
	}
	if opts.TypeFilter == "" {
		opts.TypeFilter = FilterAll
	}
	if opts.Now.IsZero() {
		opts.Now = time.Now().UTC()
	}

	res := Result{
		GeneratedAt:      opts.Now.UTC(),
		Window:           durString(opts.Window),
		NewWithin:        durString(opts.NewWithin),
		EventsConsidered: len(events),
	}

	cutoffWindow := time.Time{}
	if opts.Window > 0 {
		cutoffWindow = opts.Now.Add(-opts.Window)
	}
	cutoffNew := opts.Now.Add(-opts.NewWithin)

	ignored := make(map[string]struct{}, len(opts.IgnoreReasons))
	for _, r := range opts.IgnoreReasons {
		if r = strings.TrimSpace(r); r != "" {
			ignored[strings.ToLower(r)] = struct{}{}
		}
	}

	groups := map[key]*acc{}
	for _, e := range events {
		if !keep(e, opts, ignored, cutoffWindow) {
			continue
		}
		res.EventsFiltered++
		res.TotalOccurrences += e.Count

		k := key{e.Namespace, e.Reason, e.Object.Kind, e.Root}
		a := groups[k]
		if a == nil {
			a = &acc{
				group: Group{
					Namespace: e.Namespace,
					Reason:    e.Reason,
					Kind:      e.Object.Kind,
					Root:      e.Root,
					Type:      model.TypeNormal,
					First:     e.First,
					Last:      e.Last,
				},
				objects: map[string]struct{}{},
				sources: map[string]struct{}{},
			}
			groups[k] = a
		}

		a.group.Occurrences += e.Count
		a.objects[e.Object.Name] = struct{}{}
		if e.Source != "" {
			a.sources[e.Source] = struct{}{}
		}
		if e.Type == model.TypeWarning {
			a.group.Type = model.TypeWarning
		}
		if e.First.Before(a.group.First) {
			a.group.First = e.First
		}
		if e.Last.After(a.group.Last) {
			a.group.Last = e.Last
		}
		// The representative message is the most recent one: during triage the
		// latest wording is the one that still matches reality.
		if e.Message != "" && (a.group.Message == "" || !e.Last.Before(a.lastSeen)) {
			a.group.Message = e.Message
			a.lastSeen = e.Last
		}
		a.recent += float64(e.Count) * recentShare(e, cutoffNew)
	}

	out := make([]Group, 0, len(groups))
	for _, a := range groups {
		g := a.group
		if opts.MinCount > 0 && g.Occurrences < opts.MinCount {
			continue
		}
		g.Objects = len(a.objects)
		g.ObjectNames = sortedKeys(a.objects)
		g.Sources = sortedKeys(a.sources)
		g.Class = classify(g, a.recent, opts, cutoffNew)
		g.Score = score(g)
		out = append(out, g)
	}

	sort.Slice(out, func(i, j int) bool { return less(out[i], out[j]) })
	res.GroupsTotal = len(out)
	if opts.Top > 0 && len(out) > opts.Top {
		out = out[:opts.Top]
	}
	res.Groups = out
	return res
}

func keep(e model.Event, opts Options, ignored map[string]struct{}, cutoffWindow time.Time) bool {
	if !cutoffWindow.IsZero() && e.Last.Before(cutoffWindow) {
		return false
	}
	if opts.Namespace != "" && !strings.EqualFold(e.Namespace, opts.Namespace) {
		return false
	}
	if opts.Reason != "" && !strings.EqualFold(e.Reason, opts.Reason) {
		return false
	}
	if opts.Kind != "" && !strings.EqualFold(e.Object.Kind, opts.Kind) {
		return false
	}
	if _, bad := ignored[strings.ToLower(e.Reason)]; bad {
		return false
	}
	switch strings.ToLower(opts.TypeFilter) {
	case FilterWarning:
		if e.Type != model.TypeWarning {
			return false
		}
	case FilterNormal:
		if e.Type == model.TypeWarning {
			return false
		}
	}
	return true
}

// classify answers the only question triage cares about: is this new, is it
// spiking, or has it been broken since Tuesday.
// recentShare estimates how much of one event row's count landed inside the
// recent window. The API gives first seen, last seen and a total — not a
// histogram — so occurrences are spread uniformly across the row's span. That
// is the difference between "this row was touched a minute ago" and "this row
// is mostly recent": a count of 291 stretched over three days contributes
// almost nothing to the last fifteen minutes, which is exactly what keeps a
// chronic complaint from masquerading as a burst.
func recentShare(e model.Event, cutoff time.Time) float64 {
	if e.Last.Before(cutoff) {
		return 0
	}
	span := e.Last.Sub(e.First)
	if span <= 0 {
		return 1
	}
	start := e.First
	if cutoff.After(start) {
		start = cutoff
	}
	overlap := e.Last.Sub(start)
	if overlap <= 0 {
		return 0
	}
	if overlap >= span {
		return 1
	}
	return float64(overlap) / float64(span)
}

func classify(g Group, recent float64, opts Options, cutoffNew time.Time) string {
	if !g.First.Before(cutoffNew) {
		return ClassNew
	}
	share := 0.0
	if g.Occurrences > 0 {
		share = recent / float64(g.Occurrences)
	}
	if recent > 0 && share >= opts.BurstFraction {
		return ClassBurst
	}
	if opts.ChronicAfter > 0 && g.Span() >= opts.ChronicAfter {
		return ClassChronic
	}
	return ClassSteady
}

// score is occurrences × distinct objects, with Warnings weighted up. Blast
// radius is the multiplier on purpose: one noisy pod is not an incident, forty
// pods saying the same thing is.
func score(g Group) float64 {
	objects := g.Objects
	if objects < 1 {
		objects = 1
	}
	weight := 1.0
	if g.Type == model.TypeWarning {
		weight = 3.0
	}
	return float64(g.Occurrences*objects) * weight
}

func less(a, b Group) bool {
	an, bn := a.Class == ClassNew, b.Class == ClassNew
	if an != bn {
		return an
	}
	if a.Score != b.Score {
		return a.Score > b.Score
	}
	if a.Occurrences != b.Occurrences {
		return a.Occurrences > b.Occurrences
	}
	if !a.Last.Equal(b.Last) {
		return a.Last.After(b.Last)
	}
	if a.Namespace != b.Namespace {
		return a.Namespace < b.Namespace
	}
	if a.Reason != b.Reason {
		return a.Reason < b.Reason
	}
	return a.Root < b.Root
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func durString(d time.Duration) string {
	if d <= 0 {
		return "all"
	}
	return d.String()
}
