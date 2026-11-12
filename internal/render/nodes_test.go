package render

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/moveeeax/k8s-event-digest/internal/nodes"
)

func nodesSample() nodes.Report {
	return nodes.Report{
		GeneratedAt: now, Since: "1h0m0s", NodesTotal: 2, UnderPressure: 1,
		Nodes: []nodes.Node{
			{
				Name:  "node-b",
				Ready: nodes.Condition{Type: "Ready", Status: nodes.StatusTrue, Since: now.Add(-72 * time.Hour)},
				Pressure: []nodes.Condition{
					{Type: "MemoryPressure", Status: nodes.StatusTrue, Since: now.Add(-12 * time.Minute)},
					{Type: "DiskPressure", Status: nodes.StatusFalse},
					{Type: "PIDPressure", Status: nodes.StatusUnknown},
				},
				UnderPressure: true,
				Headroom: nodes.Headroom{CPUAllocatable: 4000, CPUFree: 1000,
					MemAllocatable: 16 << 30, MemFree: 2 << 30, PodsAllocatable: 110, Pods: 12},
			},
			{
				Name:  "node-a",
				Ready: nodes.Condition{Type: "Ready", Status: nodes.StatusFalse, Since: now.Add(-5 * time.Minute)},
				Pressure: []nodes.Condition{
					{Type: "MemoryPressure", Status: nodes.StatusFalse},
					{Type: "DiskPressure", Status: nodes.StatusFalse},
					{Type: "PIDPressure", Status: nodes.StatusFalse},
				},
				Cordoned: true,
				Taints:   []string{"node.kubernetes.io/unschedulable:NoSchedule"},
				Blocked:  []string{nodes.BlockedNotReady, nodes.BlockedCordoned},
				Headroom: nodes.Headroom{CPUAllocatable: 4000, CPUFree: 4000,
					MemAllocatable: 16 << 30, MemFree: 16 << 30, PodsAllocatable: 110},
			},
		},
		Evictions: []nodes.Eviction{{Namespace: "workers", Pod: "w-1", Node: "node-b",
			At: now.Add(-10 * time.Minute), Resource: "memory", Threshold: "100Mi", Available: "87232Ki"}},
		Pending: []nodes.Pending{{Namespace: "prod", Pod: "api-1", CPURequest: 1500, MemRequest: 1 << 30,
			Closest: "node-b", Reason: "closest is node-b: cpu short by 500m | test"}},
	}
}

func renderNodes(t *testing.T, rep nodes.Report, format string) string {
	t.Helper()
	var buf bytes.Buffer
	if err := WriteNodes(&buf, rep, format); err != nil {
		t.Fatalf("WriteNodes(%s): %v", format, err)
	}
	return buf.String()
}

func TestNodesTable(t *testing.T) {
	out := renderNodes(t, nodesSample(), FormatTable)
	for _, want := range []string{
		"2 nodes: 0 not ready, 1 under pressure, 0 cordoned; 1 pending pods, 1 evictions in the last 1h0m0s",
		"NODE", "yes 3d", "yes 12m", "no 5m", "1 (25%)", "2Gi (12%)", "12/110",
		"not ready,cordoned", "evicted by the kubelet:", "workers/w-1", "87232Ki",
		"pending pods:", "prod/api-1", "1500m", "cpu short by 500m",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("table missing %q:\n%s", want, out)
		}
	}
	// An Unknown pressure condition is a question mark, not a dash.
	if !strings.Contains(out, "?") {
		t.Errorf("unknown PIDPressure should print ?:\n%s", out)
	}
}

func TestNodesMarkdownAndJSON(t *testing.T) {
	md := renderNodes(t, nodesSample(), FormatMarkdown)
	for _, want := range []string{"### Node pressure", "| `node-b` |", "Evicted by the kubelet:", "Pending pods:", `500m \| test`} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown missing %q:\n%s", want, md)
		}
	}

	var back nodes.Report
	if err := json.Unmarshal([]byte(renderNodes(t, nodesSample(), FormatJSON)), &back); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if len(back.Nodes) != 2 || back.Nodes[0].Headroom.CPUFree != 1000 || back.Pending[0].Closest != "node-b" {
		t.Errorf("JSON round trip lost data: %+v", back)
	}

	all := nodesSample()
	all.Since = "all"
	if out := renderNodes(t, all, FormatTable); !strings.Contains(out, "1 evictions on record") {
		t.Errorf("--since 0 summary: %s", out)
	}

	var buf bytes.Buffer
	if err := WriteNodes(&buf, nodesSample(), "xml"); err == nil {
		t.Error("expected an error for an unknown format")
	}
}
