// Package model holds the flattened Event representation and the decoder that
// turns a Kubernetes Event payload into it.
//
// Both API shapes are accepted: the classic core/v1 Event (firstTimestamp /
// lastTimestamp / count) and the newer events.k8s.io/v1 Event (eventTime /
// series / note / regarding). Everything downstream sees one struct.
package model

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ObjectRef identifies the object an event is about.
type ObjectRef struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

// String renders the reference as Kind/name.
func (o ObjectRef) String() string {
	if o.Kind == "" {
		return o.Name
	}
	return o.Kind + "/" + o.Name
}

// Event is one Kubernetes event, already normalised.
//
// Count is the number of occurrences the API collapsed into this single row,
// which is the number that matters: forty rows with count=1 and one row with
// count=40 describe very different clusters.
type Event struct {
	Namespace string    `json:"namespace"`
	Reason    string    `json:"reason"`
	Type      string    `json:"type"`
	Message   string    `json:"message"`
	Count     int       `json:"count"`
	First     time.Time `json:"first"`
	Last      time.Time `json:"last"`
	Object    ObjectRef `json:"object"`
	Root      string    `json:"root"`
	Source    string    `json:"source"`
}

const (
	// TypeWarning is the event type that matters during triage.
	TypeWarning = "Warning"
	// TypeNormal is the event type that mostly does not.
	TypeNormal = "Normal"
)

type rawRef struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

type rawSeries struct {
	Count            int        `json:"count"`
	LastObservedTime *time.Time `json:"lastObservedTime"`
}

type rawEvent struct {
	Kind     string `json:"kind"`
	Metadata struct {
		Name              string     `json:"name"`
		Namespace         string     `json:"namespace"`
		CreationTimestamp *time.Time `json:"creationTimestamp"`
	} `json:"metadata"`
	InvolvedObject *rawRef    `json:"involvedObject"`
	Regarding      *rawRef    `json:"regarding"`
	Reason         string     `json:"reason"`
	Message        string     `json:"message"`
	Note           string     `json:"note"`
	Type           string     `json:"type"`
	Count          int        `json:"count"`
	First          *time.Time `json:"firstTimestamp"`
	Last           *time.Time `json:"lastTimestamp"`
	EventTime      *time.Time `json:"eventTime"`
	Series         *rawSeries `json:"series"`
	Source         struct {
		Component string `json:"component"`
		Host      string `json:"host"`
	} `json:"source"`
	ReportingComponent string `json:"reportingComponent"`
	ReportingInstance  string `json:"reportingInstance"`
}

type rawList struct {
	Kind  string     `json:"kind"`
	Items []rawEvent `json:"items"`
}

// Decode accepts an EventList (what `kubectl get events -A -o json` prints), a
// bare JSON array of events, or a single Event object.
func Decode(data []byte) ([]Event, error) {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return nil, fmt.Errorf("empty event payload")
	}

	var raws []rawEvent
	switch trimmed[0] {
	case '[':
		if err := json.Unmarshal(data, &raws); err != nil {
			return nil, fmt.Errorf("decode event array: %w", err)
		}
	case '{':
		var list rawList
		if err := json.Unmarshal(data, &list); err != nil {
			return nil, fmt.Errorf("decode event list: %w", err)
		}
		if list.Items == nil {
			var single rawEvent
			if err := json.Unmarshal(data, &single); err != nil {
				return nil, fmt.Errorf("decode event: %w", err)
			}
			if single.Reason == "" && single.InvolvedObject == nil && single.Regarding == nil {
				return nil, fmt.Errorf("payload has no items and does not look like an Event")
			}
			raws = []rawEvent{single}
		} else {
			raws = list.Items
		}
	default:
		return nil, fmt.Errorf("payload is not JSON (starts with %q)", trimmed[0])
	}

	out := make([]Event, 0, len(raws))
	for _, r := range raws {
		out = append(out, r.normalise())
	}
	return out, nil
}

func (r rawEvent) normalise() Event {
	ref := r.InvolvedObject
	if ref == nil {
		ref = r.Regarding
	}
	if ref == nil {
		ref = &rawRef{}
	}

	ns := ref.Namespace
	if ns == "" {
		ns = r.Metadata.Namespace
	}
	if ns == "" {
		ns = "default"
	}

	msg := r.Message
	if msg == "" {
		msg = r.Note
	}

	typ := r.Type
	if typ == "" {
		typ = TypeNormal
	}

	// count: core/v1 count first, then the events.k8s.io series counter, then 1.
	count := r.Count
	if count <= 0 && r.Series != nil && r.Series.Count > 0 {
		count = r.Series.Count
	}
	if count <= 0 {
		count = 1
	}

	first := firstNonNil(r.First, r.EventTime, r.Metadata.CreationTimestamp)
	last := firstNonNil(r.Last, seriesTime(r.Series), r.EventTime, r.Metadata.CreationTimestamp)
	if first.IsZero() {
		first = last
	}
	if last.IsZero() {
		last = first
	}
	if last.Before(first) {
		first, last = last, first
	}

	src := r.Source.Component
	if src == "" {
		src = r.ReportingComponent
	}
	if r.Source.Host != "" {
		if src == "" {
			src = r.Source.Host
		} else {
			src += "/" + r.Source.Host
		}
	}

	obj := ObjectRef{Kind: ref.Kind, Name: ref.Name, Namespace: ns}
	return Event{
		Namespace: ns,
		Reason:    r.Reason,
		Type:      typ,
		Message:   collapseSpace(msg),
		Count:     count,
		First:     first.UTC(),
		Last:      last.UTC(),
		Object:    obj,
		Root:      RootOf(obj.Kind, obj.Name),
		Source:    src,
	}
}

func seriesTime(s *rawSeries) *time.Time {
	if s == nil {
		return nil
	}
	return s.LastObservedTime
}

func firstNonNil(ts ...*time.Time) time.Time {
	for _, t := range ts {
		if t != nil && !t.IsZero() {
			return *t
		}
	}
	return time.Time{}
}

func collapseSpace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
