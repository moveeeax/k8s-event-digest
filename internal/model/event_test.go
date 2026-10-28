package model

import (
	"os"
	"testing"
	"time"
)

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return ts.UTC()
}

func TestDecodeCoreV1List(t *testing.T) {
	data := []byte(`{
      "kind": "EventList",
      "items": [{
        "metadata": {"name": "api.1", "namespace": "prod"},
        "involvedObject": {"kind": "Pod", "name": "api-7d9f8c4b6d-x2k9p", "namespace": "prod"},
        "reason": "FailedScheduling",
        "message": "0/9 nodes   are\navailable",
        "type": "Warning",
        "count": 12,
        "firstTimestamp": "2026-10-28T11:00:00Z",
        "lastTimestamp": "2026-10-28T11:45:00Z",
        "source": {"component": "default-scheduler"}
      }]
    }`)

	events, err := Decode(data)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	e := events[0]
	if e.Count != 12 {
		t.Errorf("Count = %d, want 12", e.Count)
	}
	if e.Root != "Deployment/api" {
		t.Errorf("Root = %q, want Deployment/api", e.Root)
	}
	if e.Namespace != "prod" || e.Type != TypeWarning {
		t.Errorf("Namespace/Type = %q/%q", e.Namespace, e.Type)
	}
	if e.Message != "0/9 nodes are available" {
		t.Errorf("Message = %q, want whitespace collapsed", e.Message)
	}
	if !e.First.Equal(mustTime(t, "2026-10-28T11:00:00Z")) {
		t.Errorf("First = %v", e.First)
	}
	if !e.Last.Equal(mustTime(t, "2026-10-28T11:45:00Z")) {
		t.Errorf("Last = %v", e.Last)
	}
	if e.Source != "default-scheduler" {
		t.Errorf("Source = %q", e.Source)
	}
}

// The events.k8s.io/v1 shape has no count, no firstTimestamp and no message —
// everything has to come from series, eventTime and note.
func TestDecodeEventsV1Fixture(t *testing.T) {
	data, err := os.ReadFile("testdata/eventsv1.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	events, err := Decode(data)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2", len(events))
	}

	sandbox := events[0]
	if sandbox.Count != 7 {
		t.Errorf("series count: got %d, want 7", sandbox.Count)
	}
	if !sandbox.First.Equal(mustTime(t, "2026-10-28T10:02:11.482913Z")) {
		t.Errorf("First = %v, want eventTime", sandbox.First)
	}
	if !sandbox.Last.Equal(mustTime(t, "2026-10-28T11:41:09.118422Z")) {
		t.Errorf("Last = %v, want series.lastObservedTime", sandbox.Last)
	}
	if sandbox.Namespace != "jobs" || sandbox.Object.Kind != "Pod" {
		t.Errorf("regarding not used: %+v", sandbox.Object)
	}
	if sandbox.Message == "" {
		t.Error("note was not used as the message")
	}

	killing := events[1]
	if killing.Count != 1 {
		t.Errorf("missing count should default to 1, got %d", killing.Count)
	}
	if !killing.First.Equal(killing.Last) {
		t.Errorf("single-shot event should have First == Last, got %v / %v", killing.First, killing.Last)
	}
	if killing.Message != "Stopping container postgres" {
		t.Errorf("Message = %q", killing.Message)
	}
	if killing.Root != "StatefulSet/pg" {
		t.Errorf("Root = %q, want StatefulSet/pg", killing.Root)
	}
}

func TestDecodeBareArrayAndSingle(t *testing.T) {
	arr := []byte(`[{"involvedObject":{"kind":"Node","name":"ip-10-0-3-17"},"reason":"ImageGCFailed","type":"Warning","count":3,"lastTimestamp":"2026-10-28T11:00:00Z"}]`)
	events, err := Decode(arr)
	if err != nil {
		t.Fatalf("array: %v", err)
	}
	if len(events) != 1 || events[0].Root != "Node/ip-10-0-3-17" {
		t.Fatalf("array decode: %+v", events)
	}
	if events[0].Namespace != "default" {
		t.Errorf("cluster-scoped object should fall back to default, got %q", events[0].Namespace)
	}

	single := []byte(`{"kind":"Event","involvedObject":{"kind":"Pod","name":"pg-0","namespace":"data"},"reason":"Killing","lastTimestamp":"2026-10-28T11:00:00Z"}`)
	events, err = Decode(single)
	if err != nil {
		t.Fatalf("single: %v", err)
	}
	if len(events) != 1 || events[0].Type != TypeNormal {
		t.Fatalf("single decode: %+v", events)
	}
}

func TestDecodeRejectsGarbage(t *testing.T) {
	for name, input := range map[string]string{
		"empty":     "",
		"not json":  "FailedScheduling",
		"wrong obj": `{"kind":"PodList","metadata":{}}`,
		"broken":    `{"items": [`,
	} {
		if _, err := Decode([]byte(input)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestRootOf(t *testing.T) {
	cases := []struct{ kind, name, want string }{
		{"Pod", "api-7d9f8c4b6d-x2k9p", "Deployment/api"},
		{"Pod", "checkout-api-5f9b7c8d44-qq7wv", "Deployment/checkout-api"},
		{"Pod", "pg-0", "StatefulSet/pg"},
		{"Pod", "pg-12", "StatefulSet/pg"},
		{"Pod", "backup-29384160-9wqtr", "CronJob/backup"},
		{"Pod", "fluentbit-k4m2z", "Workload/fluentbit"},
		{"Pod", "standalone", "Pod/standalone"},
		{"ReplicaSet", "api-7d9f8c4b6d", "Deployment/api"},
		{"Job", "backup-29384160", "CronJob/backup"},
		{"Job", "oneoff-migration", "Job/oneoff-migration"},
		{"Node", "ip-10-0-3-17", "Node/ip-10-0-3-17"},
		{"Deployment", "api", "Deployment/api"},
		{"", "orphan", "orphan"},
		{"Pod", "", "Pod"},
	}
	for _, c := range cases {
		if got := RootOf(c.kind, c.name); got != c.want {
			t.Errorf("RootOf(%q, %q) = %q, want %q", c.kind, c.name, got, c.want)
		}
	}
}
