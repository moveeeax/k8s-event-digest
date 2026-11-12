package nodes

import (
	"os"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

var now = time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)

const (
	n1 = "ip-10-0-1-11.eu-central-1.compute.internal" // healthy, tight on cpu
	n2 = "ip-10-0-1-12.eu-central-1.compute.internal" // memory pressure
	n3 = "ip-10-0-1-13.eu-central-1.compute.internal" // cordoned
	n4 = "ip-10-0-1-14.eu-central-1.compute.internal" // not ready
	n5 = "ip-10-0-2-21.eu-central-1.compute.internal" // gpu, tainted
)

func load(t *testing.T) Snapshot {
	t.Helper()
	data, err := os.ReadFile("testdata/cluster.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	snap, err := Decode(data)
	if err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	return snap
}

func build(t *testing.T, mutate func(*Options)) Report {
	t.Helper()
	opts := Options{Now: now, Since: time.Hour}
	if mutate != nil {
		mutate(&opts)
	}
	return Build(load(t), opts)
}

func nodeByName(t *testing.T, rep Report, name string) Node {
	t.Helper()
	for _, n := range rep.Nodes {
		if n.Name == name {
			return n
		}
	}
	t.Fatalf("node %s not in report", name)
	return Node{}
}

func pendingByName(t *testing.T, rep Report, ns, pod string) Pending {
	t.Helper()
	for _, p := range rep.Pending {
		if p.Namespace == ns && p.Pod == pod {
			return p
		}
	}
	t.Fatalf("pending pod %s/%s not in report", ns, pod)
	return Pending{}
}

func TestDecodeSkipsOtherKinds(t *testing.T) {
	snap := load(t)
	if len(snap.Nodes) != 5 {
		t.Errorf("Nodes = %d, want 5", len(snap.Nodes))
	}
	if len(snap.Pods) != 15 {
		t.Errorf("Pods = %d, want 15", len(snap.Pods))
	}

	mixed := `{"kind":"List","items":[
		{"kind":"Event","reason":"BackOff"},
		{"kind":"Node","metadata":{"name":"a"}}]}`
	if snap, err := Decode([]byte(mixed)); err != nil || len(snap.Nodes) != 1 {
		t.Errorf("mixed list: %d nodes, err %v", len(snap.Nodes), err)
	}

	// A NodeList from a typed client: items without a kind.
	typed := `{"kind":"NodeList","items":[{"metadata":{"name":"a"}}]}`
	if snap, err := Decode([]byte(typed)); err != nil || len(snap.Nodes) != 1 {
		t.Errorf("NodeList: %d nodes, err %v", len(snap.Nodes), err)
	}

	for _, bad := range []string{"", "nope", `{"kind":"Pod"}`, `{"kind":"PodList","items":[]}`} {
		if _, err := Decode([]byte(bad)); err == nil {
			t.Errorf("Decode(%q) should fail", bad)
		}
	}
}

// Acceptance: one row per node with Ready and each pressure condition, and
// how long it has held.
func TestConditionsAndSince(t *testing.T) {
	rep := build(t, nil)
	if rep.NodesTotal != 5 || rep.NotReady != 1 || rep.UnderPressure != 1 || rep.Cordoned != 1 {
		t.Errorf("summary = %d nodes, %d not ready, %d pressure, %d cordoned; want 5/1/1/1",
			rep.NodesTotal, rep.NotReady, rep.UnderPressure, rep.Cordoned)
	}

	mem := nodeByName(t, rep, n2)
	if !mem.UnderPressure || mem.Pressure[0].Type != "MemoryPressure" || mem.Pressure[0].Status != StatusTrue {
		t.Fatalf("n2 pressure = %+v, want MemoryPressure True", mem.Pressure)
	}
	if want := time.Date(2026, 10, 28, 11, 48, 0, 0, time.UTC); !mem.Pressure[0].Since.Equal(want) {
		t.Errorf("MemoryPressure since %v, want %v", mem.Pressure[0].Since, want)
	}
	// The memory-pressure taint mirrors the condition and is not a second reason.
	if len(mem.Blocked) != 0 {
		t.Errorf("n2 Blocked = %v, want none beyond the pressure column", mem.Blocked)
	}

	down := nodeByName(t, rep, n4)
	if down.Ready.Status != StatusUnknown || down.Ready.Reason != "NodeStatusUnknown" {
		t.Errorf("n4 Ready = %+v, want Unknown/NodeStatusUnknown", down.Ready)
	}
	if strings.Join(down.Blocked, ",") != BlockedNotReady {
		t.Errorf("n4 Blocked = %v, want only not ready", down.Blocked)
	}

	if got := nodeByName(t, rep, n3).Blocked; strings.Join(got, ",") != BlockedCordoned {
		t.Errorf("n3 Blocked = %v, want only cordoned", got)
	}
	if got := nodeByName(t, rep, n5).Blocked; strings.Join(got, ",") != BlockedTainted {
		t.Errorf("n5 Blocked = %v, want untolerated taint", got)
	}
	if got := nodeByName(t, rep, n5).Taints; len(got) != 1 || got[0] != "nvidia.com/gpu=present:NoSchedule" {
		t.Errorf("n5 Taints = %v", got)
	}

	if rep.Unhealthy() != 2 {
		t.Errorf("Unhealthy = %d, want 2 (not ready + memory pressure)", rep.Unhealthy())
	}
}

// Acceptance: headroom is allocatable minus requests of non-terminated pods
// bound to the node, with init and sidecar containers counted the way the
// scheduler counts them.
func TestHeadroom(t *testing.T) {
	h := nodeByName(t, build(t, nil), n1).Headroom
	// api 1000m + api 1000m + ledger max(500m+100m sidecar, 1500m init + 100m sidecar) + web 100m.
	// The Succeeded report pod (2 cpu) does not count.
	if h.CPUAllocatable != 3920 || h.CPURequested != 3700 || h.CPUFree != 220 {
		t.Errorf("cpu = %d alloc, %d requested, %d free; want 3920/3700/220", h.CPUAllocatable, h.CPURequested, h.CPUFree)
	}
	const mi = 1 << 20
	// 2Gi + 2Gi + (4Gi + 128Mi sidecar) = 8320Mi.
	if h.MemRequested != 8320*mi || h.MemFree != (16384-8320)*mi {
		t.Errorf("memory requested %d, free %d; want %d and %d", h.MemRequested, h.MemFree, 8320*mi, (16384-8320)*mi)
	}
	if h.Pods != 4 || h.PodsAllocatable != 58 {
		t.Errorf("pods = %d/%d, want 4/58", h.Pods, h.PodsAllocatable)
	}
}

func TestPodRequestsRules(t *testing.T) {
	always := corev1.ContainerRestartPolicyAlways
	req := func(cpu, mem string) corev1.ResourceRequirements {
		return corev1.ResourceRequirements{Requests: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse(cpu), corev1.ResourceMemory: resource.MustParse(mem),
		}}
	}
	cases := []struct {
		name     string
		spec     corev1.PodSpec
		cpu, mem int64
	}{
		{"containers add up", corev1.PodSpec{Containers: []corev1.Container{
			{Resources: req("250m", "1Mi")}, {Resources: req("750m", "3Mi")}}}, 1000, 4 << 20},
		{"a bigger init container wins", corev1.PodSpec{
			InitContainers: []corev1.Container{{Resources: req("2", "1Mi")}},
			Containers:     []corev1.Container{{Resources: req("500m", "8Mi")}}}, 2000, 8 << 20},
		{"sidecars add to the main containers", corev1.PodSpec{
			InitContainers: []corev1.Container{{Resources: req("100m", "1Mi"), RestartPolicy: &always}},
			Containers:     []corev1.Container{{Resources: req("500m", "8Mi")}}}, 600, 9 << 20},
		{"an init container after a sidecar runs next to it", corev1.PodSpec{
			InitContainers: []corev1.Container{
				{Resources: req("300m", "1Mi"), RestartPolicy: &always},
				{Resources: req("1", "1Mi")}},
			Containers: []corev1.Container{{Resources: req("100m", "1Mi")}}}, 1300, 2 << 20},
		{"overhead is added", corev1.PodSpec{
			Overhead:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")},
			Containers: []corev1.Container{{Resources: req("100m", "1Mi")}}}, 150, 1 << 20},
		{"pod-level requests replace the sum", corev1.PodSpec{
			Resources:  &corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")}},
			Containers: []corev1.Container{{Resources: req("100m", "1Mi")}}}, 2000, 1 << 20},
	}
	for _, c := range cases {
		p := corev1.Pod{Spec: c.spec}
		cpu, mem := podRequests(&p)
		if cpu != c.cpu || mem != c.mem {
			t.Errorf("%s: got %dm / %d bytes, want %dm / %d bytes", c.name, cpu, mem, c.cpu, c.mem)
		}
	}
}

// Acceptance: evictions from the kubelet inside --since, with the resource,
// threshold and what was available parsed out of the message.
func TestEvictions(t *testing.T) {
	rep := build(t, nil)
	if len(rep.Evictions) != 2 {
		t.Fatalf("Evictions = %d inside 1h, want 2: %+v", len(rep.Evictions), rep.Evictions)
	}
	first := rep.Evictions[0]
	if first.Pod != "worker-7f9c6d5b8b-m4kd2" || first.Node != n2 {
		t.Errorf("newest eviction = %s on %s", first.Pod, first.Node)
	}
	if first.Resource != "memory" || first.Threshold != "100Mi" || first.Available != "87232Ki" {
		t.Errorf("parsed %q / %q / %q, want memory / 100Mi / 87232Ki", first.Resource, first.Threshold, first.Available)
	}
	// DisruptionTarget wins over the container finish time.
	if want := time.Date(2026, 10, 28, 11, 49, 30, 0, time.UTC); !first.At.Equal(want) {
		t.Errorf("At = %v, want %v", first.At, want)
	}

	second := rep.Evictions[1]
	if second.Resource != "ephemeral-storage" || second.Threshold != "2Gi" || second.Available != "1536Mi" {
		t.Errorf("second eviction parsed %+v", second)
	}
	if want := time.Date(2026, 10, 28, 11, 31, 5, 0, time.UTC); !second.At.Equal(want) {
		t.Errorf("fallback At = %v, want the container finish %v", second.At, want)
	}

	all := build(t, func(o *Options) { o.Since = 0 })
	if len(all.Evictions) != 3 {
		t.Errorf("--since 0 kept %d evictions, want 3", len(all.Evictions))
	}
	other := build(t, func(o *Options) { o.Namespace = "prod" })
	if len(other.Evictions) != 0 {
		t.Errorf("namespace filter kept %d evictions from workers", len(other.Evictions))
	}
}

// Acceptance: a Pending pod names the node that came closest and the
// resource that fell short.
func TestPendingClosestNode(t *testing.T) {
	rep := build(t, nil)
	if len(rep.Pending) != 4 {
		t.Fatalf("Pending = %d, want 4 (a pod bound to a node is not the scheduler's problem)", len(rep.Pending))
	}

	api := pendingByName(t, rep, "prod", "api-7d9f8c4b6d-p4n2c")
	if api.Closest != n1 {
		t.Errorf("closest = %q, want %s", api.Closest, n1)
	}
	if len(api.Shortfalls) != 1 || api.Shortfalls[0].Resource != "cpu" || api.Shortfalls[0].Short != 780 {
		t.Errorf("shortfalls = %+v, want cpu short by 780m", api.Shortfalls)
	}
	if !strings.Contains(api.Reason, "cpu short by 780m") {
		t.Errorf("reason = %q", api.Reason)
	}
	// n2 (memory-pressure taint) and n5 (gpu taint) are skipped as tainted.
	want := map[string]int{BlockedNotReady: 1, BlockedCordoned: 1, BlockedTainted: 2}
	for k, v := range want {
		if api.Excluded[k] != v {
			t.Errorf("excluded[%s] = %d, want %d (%v)", k, api.Excluded[k], v, api.Excluded)
		}
	}

	gpu := pendingByName(t, rep, "ml", "trainer-0")
	if gpu.Closest != n5 || len(gpu.Shortfalls) != 0 || !strings.Contains(gpu.Reason, "not modelled") {
		t.Errorf("tolerating pod with a fitting node: %+v", gpu)
	}

	huge := pendingByName(t, rep, "batch", "huge-0")
	if huge.Closest != "" || huge.Reason != "no eligible node: 1 cordoned, 3 node selector, 1 not ready" {
		t.Errorf("unplaceable pod: %+v", huge)
	}

	gated := pendingByName(t, rep, "batch", "queued-0")
	if gated.Reason != "held by scheduling gates: kueue.x-k8s.io/admission" {
		t.Errorf("gated pod reason = %q", gated.Reason)
	}
}

func TestTolerationOperators(t *testing.T) {
	taint := corev1.Taint{Key: "dedicated", Value: "db", Effect: corev1.TaintEffectNoSchedule}
	cases := []struct {
		tol  corev1.Toleration
		want bool
	}{
		{corev1.Toleration{Key: "dedicated", Operator: corev1.TolerationOpEqual, Value: "db"}, true},
		{corev1.Toleration{Key: "dedicated", Value: "db", Effect: corev1.TaintEffectNoSchedule}, true},
		{corev1.Toleration{Key: "dedicated", Operator: corev1.TolerationOpExists}, true},
		{corev1.Toleration{Operator: corev1.TolerationOpExists}, true},
		{corev1.Toleration{Key: "dedicated", Value: "web"}, false},
		{corev1.Toleration{Key: "dedicated", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute}, false},
	}
	for _, c := range cases {
		if got := tolerated([]corev1.Toleration{c.tol}, &taint); got != c.want {
			t.Errorf("%+v tolerates %v = %v, want %v", c.tol, taint, got, c.want)
		}
	}
}

func TestSortOrders(t *testing.T) {
	names := func(rep Report) []string {
		out := []string{}
		for _, n := range rep.Nodes {
			out = append(out, n.Name)
		}
		return out
	}

	byPressure := names(build(t, nil))
	if byPressure[0] != n4 || byPressure[1] != n2 || byPressure[len(byPressure)-1] != n1 {
		t.Errorf("pressure order = %v: want not-ready, then pressure, healthy last", byPressure)
	}

	byHeadroom := names(build(t, func(o *Options) { o.SortBy = SortHeadroom }))
	if byHeadroom[0] != n1 || byHeadroom[len(byHeadroom)-1] != n5 {
		t.Errorf("headroom order = %v: want the tightest (n1) first, the empty gpu node last", byHeadroom)
	}

	byName := names(build(t, func(o *Options) { o.SortBy = SortName }))
	if strings.Join(byName, ",") != strings.Join([]string{n1, n2, n3, n4, n5}, ",") {
		t.Errorf("name order = %v", byName)
	}
}

func TestFormatAmount(t *testing.T) {
	cases := []struct {
		res  string
		v    int64
		want string
	}{
		{"cpu", 780, "780m"},
		{"cpu", 2000, "2"},
		{"memory", 2 << 30, "2Gi"},
		{"memory", 8064 << 20, "7.9Gi"},
		{"memory", 512 << 20, "512Mi"},
		{"memory", -(1 << 30), "-1Gi"},
		{"pods", 1, "1"},
	}
	for _, c := range cases {
		if got := FormatAmount(c.res, c.v); got != c.want {
			t.Errorf("FormatAmount(%s, %d) = %q, want %q", c.res, c.v, got, c.want)
		}
	}
}
