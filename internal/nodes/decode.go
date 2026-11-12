// Package nodes answers why the scheduler said no: node conditions, how much
// of each node's allocatable is already requested, what the kubelet evicted,
// and which node a Pending pod came closest to.
//
// Like the digest, the analysis is pure. It takes Node and Pod objects in and
// hands a Report back, so every rule runs against a JSON fixture.
package nodes

import (
	"encoding/json"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// Snapshot is the cluster state one report is computed from.
type Snapshot struct {
	Nodes []corev1.Node
	Pods  []corev1.Pod
}

type rawList struct {
	Kind  string            `json:"kind"`
	Items []json.RawMessage `json:"items"`
}

type kindProbe struct {
	Kind string `json:"kind"`
}

// Decode accepts what `kubectl get nodes,pods -A -o json` prints (a List of
// mixed items), a NodeList or PodList, or a bare JSON array of objects. Items
// of any other kind are skipped, so a dump that also carries events is fine.
// An item without a kind takes it from the list kind (NodeList, PodList).
func Decode(data []byte) (Snapshot, error) {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return Snapshot{}, fmt.Errorf("empty node/pod payload")
	}

	var items []json.RawMessage
	listKind := ""
	switch trimmed[0] {
	case '[':
		if err := json.Unmarshal(data, &items); err != nil {
			return Snapshot{}, fmt.Errorf("decode object array: %w", err)
		}
	case '{':
		var list rawList
		if err := json.Unmarshal(data, &list); err != nil {
			return Snapshot{}, fmt.Errorf("decode list: %w", err)
		}
		if list.Items == nil {
			return Snapshot{}, fmt.Errorf("payload is a %q, not a list of nodes and pods", list.Kind)
		}
		items = list.Items
		listKind = list.Kind
	default:
		return Snapshot{}, fmt.Errorf("payload is not JSON (starts with %q)", trimmed[0])
	}

	var snap Snapshot
	for i, item := range items {
		var probe kindProbe
		if err := json.Unmarshal(item, &probe); err != nil {
			return Snapshot{}, fmt.Errorf("item %d: %w", i, err)
		}
		kind := probe.Kind
		if kind == "" {
			kind = strings.TrimSuffix(listKind, "List")
		}
		switch kind {
		case "Node":
			var n corev1.Node
			if err := json.Unmarshal(item, &n); err != nil {
				return Snapshot{}, fmt.Errorf("item %d: decode node: %w", i, err)
			}
			snap.Nodes = append(snap.Nodes, n)
		case "Pod":
			var p corev1.Pod
			if err := json.Unmarshal(item, &p); err != nil {
				return Snapshot{}, fmt.Errorf("item %d: decode pod: %w", i, err)
			}
			snap.Pods = append(snap.Pods, p)
		}
	}
	if len(snap.Nodes) == 0 {
		return Snapshot{}, fmt.Errorf("payload has no Node objects (capture it with `kubectl get nodes,pods -A -o json`)")
	}
	return snap, nil
}
