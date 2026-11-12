package nodes

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/klog/v2"
)

// Sort orders accepted on the command line.
const (
	SortPressure = "pressure" // broken nodes first, then the tightest
	SortHeadroom = "headroom" // least free CPU or memory first
	SortName     = "name"
)

// Condition statuses as printed. Unknown covers a missing condition too: a
// kubelet that stopped reporting is the reason the node controller marks it.
const (
	StatusTrue    = "True"
	StatusFalse   = "False"
	StatusUnknown = "Unknown"
)

// Reasons a node removes capacity, and why a Pending pod skipped it.
const (
	BlockedNotReady = "not ready"
	BlockedCordoned = "cordoned"
	BlockedTainted  = "untolerated taint"
	BlockedSelector = "node selector"
)

// pressureTypes are the kubelet conditions where True is bad, in column order.
var pressureTypes = []corev1.NodeConditionType{
	corev1.NodeMemoryPressure, corev1.NodeDiskPressure, corev1.NodePIDPressure,
}

// Options controls one report.
type Options struct {
	Now       time.Time
	Since     time.Duration // evictions older than this are dropped; 0 keeps all
	Namespace string        // restricts evictions and Pending pods, never headroom
	SortBy    string
}

// Condition is one node condition and how long it has held.
type Condition struct {
	Type   string    `json:"type"`
	Status string    `json:"status"`
	Since  time.Time `json:"since,omitempty"`
	Reason string    `json:"reason,omitempty"`
}

// Headroom is allocatable minus the requests of every non-terminated pod bound
// to the node. CPU is in millicores, memory in bytes.
type Headroom struct {
	CPUAllocatable  int64 `json:"cpuAllocatableMilli"`
	CPURequested    int64 `json:"cpuRequestedMilli"`
	CPUFree         int64 `json:"cpuFreeMilli"`
	MemAllocatable  int64 `json:"memoryAllocatableBytes"`
	MemRequested    int64 `json:"memoryRequestedBytes"`
	MemFree         int64 `json:"memoryFreeBytes"`
	PodsAllocatable int64 `json:"podsAllocatable"`
	Pods            int64 `json:"pods"`
}

// FreeFraction is the smaller of the CPU and memory free shares, which is the
// one that runs out first.
func (h Headroom) FreeFraction() float64 {
	f := 1.0
	if h.CPUAllocatable > 0 {
		f = min(f, float64(h.CPUFree)/float64(h.CPUAllocatable))
	}
	if h.MemAllocatable > 0 {
		f = min(f, float64(h.MemFree)/float64(h.MemAllocatable))
	}
	return f
}

// Node is one row of the report.
type Node struct {
	Name          string      `json:"name"`
	Ready         Condition   `json:"ready"`
	Pressure      []Condition `json:"pressure"`
	UnderPressure bool        `json:"underPressure"`
	Cordoned      bool        `json:"cordoned"`
	Taints        []string    `json:"taints,omitempty"`
	Headroom      Headroom    `json:"headroom"`
	Blocked       []string    `json:"blocked,omitempty"` // why the node removes capacity
}

// Eviction is a pod the kubelet evicted from a node.
type Eviction struct {
	Namespace string    `json:"namespace"`
	Pod       string    `json:"pod"`
	Node      string    `json:"node"`
	At        time.Time `json:"at,omitempty"`
	Resource  string    `json:"resource,omitempty"`  // what the node was low on
	Threshold string    `json:"threshold,omitempty"` // the eviction threshold quantity
	Available string    `json:"available,omitempty"` // what was left when it fired
	Message   string    `json:"message"`
}

// Shortfall is how much of one resource a node lacks for a pod.
type Shortfall struct {
	Resource string `json:"resource"`
	Short    int64  `json:"short"` // millicores for cpu, bytes for memory, a count for pods
}

// Pending explains one unscheduled pod.
type Pending struct {
	Namespace  string         `json:"namespace"`
	Pod        string         `json:"pod"`
	CPURequest int64          `json:"cpuRequestMilli"`
	MemRequest int64          `json:"memoryRequestBytes"`
	Closest    string         `json:"closestNode,omitempty"`
	Shortfalls []Shortfall    `json:"shortfalls,omitempty"`
	Excluded   map[string]int `json:"excluded,omitempty"` // nodes skipped before resources, by reason
	Reason     string         `json:"reason"`
}

// Report is a full nodes run.
type Report struct {
	GeneratedAt   time.Time  `json:"generatedAt"`
	Since         string     `json:"since"`
	NodesTotal    int        `json:"nodesTotal"`
	NotReady      int        `json:"notReady"`
	UnderPressure int        `json:"underPressure"`
	Cordoned      int        `json:"cordoned"`
	Nodes         []Node     `json:"nodes"`
	Evictions     []Eviction `json:"evictions"`
	Pending       []Pending  `json:"pending"`
}

// Unhealthy counts nodes that are not Ready or report a pressure condition,
// which is what --fail-on-pressure acts on.
func (r Report) Unhealthy() int {
	n := 0
	for _, node := range r.Nodes {
		if node.Ready.Status != StatusTrue || node.UnderPressure {
			n++
		}
	}
	return n
}

// Build computes the report.
func Build(snap Snapshot, opts Options) Report {
	if opts.Now.IsZero() {
		opts.Now = time.Now().UTC()
	}
	if opts.SortBy == "" {
		opts.SortBy = SortPressure
	}

	rep := Report{
		GeneratedAt: opts.Now.UTC(),
		Since:       durString(opts.Since),
		NodesTotal:  len(snap.Nodes),
		Evictions:   []Eviction{},
		Pending:     []Pending{},
	}

	byNode := map[string][]corev1.Pod{}
	for _, p := range snap.Pods {
		if p.Spec.NodeName != "" && !terminated(p) {
			byNode[p.Spec.NodeName] = append(byNode[p.Spec.NodeName], p)
		}
	}

	rep.Nodes = make([]Node, 0, len(snap.Nodes))
	for i := range snap.Nodes {
		row := nodeRow(&snap.Nodes[i], byNode[snap.Nodes[i].Name])
		if row.Ready.Status != StatusTrue {
			rep.NotReady++
		}
		if row.UnderPressure {
			rep.UnderPressure++
		}
		if row.Cordoned {
			rep.Cordoned++
		}
		rep.Nodes = append(rep.Nodes, row)
	}
	sortNodes(rep.Nodes, opts.SortBy)

	free := make(map[string]Headroom, len(rep.Nodes))
	for _, n := range rep.Nodes {
		free[n.Name] = n.Headroom
	}

	for _, p := range snap.Pods {
		if opts.Namespace != "" && !strings.EqualFold(p.Namespace, opts.Namespace) {
			continue
		}
		if ev, ok := eviction(p); ok {
			if opts.Since > 0 && (ev.At.IsZero() || ev.At.Before(opts.Now.Add(-opts.Since))) {
				continue
			}
			rep.Evictions = append(rep.Evictions, ev)
			continue
		}
		if p.Status.Phase == corev1.PodPending && p.Spec.NodeName == "" {
			rep.Pending = append(rep.Pending, explain(p, snap.Nodes, free))
		}
	}
	sort.Slice(rep.Evictions, func(i, j int) bool {
		a, b := rep.Evictions[i], rep.Evictions[j]
		if !a.At.Equal(b.At) {
			return a.At.After(b.At)
		}
		return a.Namespace+"/"+a.Pod < b.Namespace+"/"+b.Pod
	})
	sort.Slice(rep.Pending, func(i, j int) bool {
		a, b := rep.Pending[i], rep.Pending[j]
		return a.Namespace+"/"+a.Pod < b.Namespace+"/"+b.Pod
	})
	return rep
}

func nodeRow(n *corev1.Node, pods []corev1.Pod) Node {
	row := Node{
		Name:     n.Name,
		Ready:    condition(n, corev1.NodeReady),
		Cordoned: n.Spec.Unschedulable,
	}
	for _, t := range pressureTypes {
		c := condition(n, t)
		if c.Status == StatusTrue {
			row.UnderPressure = true
		}
		row.Pressure = append(row.Pressure, c)
	}
	hardTaint := false
	for _, t := range n.Spec.Taints {
		row.Taints = append(row.Taints, taintString(t))
		if hard(t) && !mirrorsState(t) {
			hardTaint = true
		}
	}

	h := Headroom{
		CPUAllocatable:  n.Status.Allocatable.Cpu().MilliValue(),
		MemAllocatable:  n.Status.Allocatable.Memory().Value(),
		PodsAllocatable: n.Status.Allocatable.Pods().Value(),
		Pods:            int64(len(pods)),
	}
	for i := range pods {
		cpu, mem := podRequests(&pods[i])
		h.CPURequested += cpu
		h.MemRequested += mem
	}
	h.CPUFree = h.CPUAllocatable - h.CPURequested
	h.MemFree = h.MemAllocatable - h.MemRequested
	row.Headroom = h

	if row.Ready.Status != StatusTrue {
		row.Blocked = append(row.Blocked, BlockedNotReady)
	}
	if row.Cordoned {
		row.Blocked = append(row.Blocked, BlockedCordoned)
	}
	if hardTaint {
		row.Blocked = append(row.Blocked, BlockedTainted)
	}
	return row
}

func condition(n *corev1.Node, t corev1.NodeConditionType) Condition {
	for _, c := range n.Status.Conditions {
		if c.Type != t {
			continue
		}
		status := string(c.Status)
		if status != StatusTrue && status != StatusFalse {
			status = StatusUnknown
		}
		return Condition{Type: string(t), Status: status, Since: c.LastTransitionTime.UTC(), Reason: c.Reason}
	}
	return Condition{Type: string(t), Status: StatusUnknown}
}

// hard taints are the ones that keep new pods off: PreferNoSchedule is only a
// scoring hint.
func hard(t corev1.Taint) bool {
	return t.Effect == corev1.TaintEffectNoSchedule || t.Effect == corev1.TaintEffectNoExecute
}

// mirrorsState is true for the taints the node controller and kubelet add to
// mirror a condition or a cordon. Those are already reported in their own
// columns, so they do not count as a separate reason the node lost capacity.
func mirrorsState(t corev1.Taint) bool {
	switch t.Key {
	case corev1.TaintNodeNotReady, corev1.TaintNodeUnreachable, corev1.TaintNodeUnschedulable,
		corev1.TaintNodeMemoryPressure, corev1.TaintNodeDiskPressure, corev1.TaintNodePIDPressure:
		return true
	}
	return false
}

func taintString(t corev1.Taint) string {
	if t.Value == "" {
		return t.Key + ":" + string(t.Effect)
	}
	return t.Key + "=" + t.Value + ":" + string(t.Effect)
}

func terminated(p corev1.Pod) bool {
	return p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed
}

// podRequests is the CPU (millicores) and memory (bytes) the scheduler counts
// for a pod, following the rule in k8s.io/component-helpers/resource: regular
// containers and restartable (sidecar) init containers add up, a plain init
// container only needs to fit next to the sidecars started before it, the
// larger of the two wins, and the RuntimeClass overhead is added on top.
// Pod-level requests, when set, replace the container sum.
func podRequests(p *corev1.Pod) (cpu, mem int64) {
	var reqCPU, reqMem int64
	for _, c := range p.Spec.Containers {
		reqCPU += c.Resources.Requests.Cpu().MilliValue()
		reqMem += c.Resources.Requests.Memory().Value()
	}

	var sideCPU, sideMem, initCPU, initMem int64
	for _, c := range p.Spec.InitContainers {
		cCPU := c.Resources.Requests.Cpu().MilliValue()
		cMem := c.Resources.Requests.Memory().Value()
		if c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways {
			reqCPU += cCPU
			reqMem += cMem
			sideCPU += cCPU
			sideMem += cMem
			cCPU, cMem = sideCPU, sideMem
		} else {
			cCPU += sideCPU
			cMem += sideMem
		}
		initCPU = max(initCPU, cCPU)
		initMem = max(initMem, cMem)
	}
	reqCPU = max(reqCPU, initCPU)
	reqMem = max(reqMem, initMem)

	if pr := p.Spec.Resources; pr != nil {
		if q, ok := pr.Requests[corev1.ResourceCPU]; ok {
			reqCPU = q.MilliValue()
		}
		if q, ok := pr.Requests[corev1.ResourceMemory]; ok {
			reqMem = q.Value()
		}
	}

	reqCPU += p.Spec.Overhead.Cpu().MilliValue()
	reqMem += p.Spec.Overhead.Memory().Value()
	return reqCPU, reqMem
}

var (
	lowOnRe     = regexp.MustCompile(`low on resource: ([A-Za-z0-9._/-]+)`)
	thresholdRe = regexp.MustCompile(`Threshold quantity: ([^,\s]+), available: ([^\s]+?)\.?(?:\s|$)`)
)

// eviction recognises a pod the kubelet evicted: phase Failed, reason Evicted.
// The time comes from the DisruptionTarget condition the kubelet sets, and
// falls back to the latest container finish when that condition is missing.
func eviction(p corev1.Pod) (Eviction, bool) {
	if p.Status.Phase != corev1.PodFailed || p.Status.Reason != "Evicted" {
		return Eviction{}, false
	}
	ev := Eviction{
		Namespace: p.Namespace,
		Pod:       p.Name,
		Node:      p.Spec.NodeName,
		Message:   strings.Join(strings.Fields(p.Status.Message), " "),
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.DisruptionTarget && c.Status == corev1.ConditionTrue {
			ev.At = c.LastTransitionTime.UTC()
		}
	}
	if ev.At.IsZero() {
		for _, cs := range p.Status.ContainerStatuses {
			if t := cs.State.Terminated; t != nil && t.FinishedAt.After(ev.At) {
				ev.At = t.FinishedAt.UTC()
			}
		}
	}
	if m := lowOnRe.FindStringSubmatch(ev.Message); m != nil {
		ev.Resource = strings.TrimSuffix(m[1], ".")
	}
	if m := thresholdRe.FindStringSubmatch(ev.Message); m != nil {
		ev.Threshold, ev.Available = m[1], m[2]
	}
	return ev, true
}

// explain names the node a Pending pod came closest to. Nodes are first
// filtered the way the scheduler's cheap checks would (Ready, cordon,
// nodeSelector, NoSchedule/NoExecute taints); the rest are compared on CPU and
// memory requests and the pod count. A pod held by scheduling gates is never
// tried, and says so. Affinity, topology spread, ports and volumes are not
// modelled, so a pod that fits on resources is reported as such rather than
// guessed at.
func explain(p corev1.Pod, nodes []corev1.Node, free map[string]Headroom) Pending {
	cpu, mem := podRequests(&p)
	out := Pending{Namespace: p.Namespace, Pod: p.Name, CPURequest: cpu, MemRequest: mem}
	if len(p.Spec.SchedulingGates) > 0 {
		gates := make([]string, 0, len(p.Spec.SchedulingGates))
		for _, g := range p.Spec.SchedulingGates {
			gates = append(gates, g.Name)
		}
		out.Reason = "held by scheduling gates: " + strings.Join(gates, ", ")
		return out
	}

	excluded := map[string]int{}
	bestName := ""
	var best []Shortfall
	bestScore := 0.0
	for i := range nodes {
		n := &nodes[i]
		if why := skip(&p, n); why != "" {
			excluded[why]++
			continue
		}
		h := free[n.Name]
		var short []Shortfall
		score := 0.0
		if cpu > h.CPUFree {
			short = append(short, Shortfall{Resource: "cpu", Short: cpu - h.CPUFree})
			score = max(score, float64(cpu-h.CPUFree)/float64(max(cpu, 1)))
		}
		if mem > h.MemFree {
			short = append(short, Shortfall{Resource: "memory", Short: mem - h.MemFree})
			score = max(score, float64(mem-h.MemFree)/float64(max(mem, 1)))
		}
		if h.PodsAllocatable > 0 && h.Pods >= h.PodsAllocatable {
			short = append(short, Shortfall{Resource: "pods", Short: 1})
			score = max(score, 1)
		}
		if bestName == "" || score < bestScore || (score == bestScore && len(short) < len(best)) {
			bestName, best, bestScore = n.Name, short, score
		}
	}
	if len(excluded) > 0 {
		out.Excluded = excluded
	}

	switch {
	case bestName == "":
		out.Reason = "no eligible node: " + excludedString(excluded)
	case len(best) == 0:
		out.Closest = bestName
		out.Reason = fmt.Sprintf("fits %s on cpu and memory requests; blocked by something not modelled here (affinity, topology spread, ports, volumes)", bestName)
	default:
		out.Closest = bestName
		out.Shortfalls = best
		parts := make([]string, 0, len(best))
		for _, s := range best {
			parts = append(parts, s.Resource+" short by "+FormatAmount(s.Resource, s.Short))
		}
		out.Reason = fmt.Sprintf("closest is %s: %s", bestName, strings.Join(parts, ", "))
	}
	return out
}

func skip(p *corev1.Pod, n *corev1.Node) string {
	if condition(n, corev1.NodeReady).Status != StatusTrue {
		return BlockedNotReady
	}
	if n.Spec.Unschedulable {
		return BlockedCordoned
	}
	for k, v := range p.Spec.NodeSelector {
		if n.Labels[k] != v {
			return BlockedSelector
		}
	}
	for i := range n.Spec.Taints {
		t := &n.Spec.Taints[i]
		if hard(*t) && !tolerated(p.Spec.Tolerations, t) {
			return BlockedTainted
		}
	}
	return ""
}

func tolerated(tols []corev1.Toleration, t *corev1.Taint) bool {
	for i := range tols {
		// Lt/Gt tolerations only pass API validation when the comparison
		// feature gate is on, so honouring them here matches what got stored.
		if tols[i].ToleratesTaint(klog.Background(), t, true) {
			return true
		}
	}
	return false
}

func excludedString(m map[string]int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%d %s", m[k], k))
	}
	if len(parts) == 0 {
		return "the cluster has no nodes"
	}
	return strings.Join(parts, ", ")
}

func sortNodes(rows []Node, by string) {
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		switch by {
		case SortName:
			return a.Name < b.Name
		case SortPressure:
			if ra, rb := rank(a), rank(b); ra != rb {
				return ra < rb
			}
		}
		if fa, fb := a.Headroom.FreeFraction(), b.Headroom.FreeFraction(); fa != fb {
			return fa < fb
		}
		return a.Name < b.Name
	})
}

// rank puts a NotReady node before one under pressure before a cordoned or
// tainted one before a healthy one.
func rank(n Node) int {
	switch {
	case n.Ready.Status != StatusTrue:
		return 0
	case n.UnderPressure:
		return 1
	case len(n.Blocked) > 0:
		return 2
	default:
		return 3
	}
}

// FormatAmount prints millicores the way a Kubernetes quantity does (500m, 2)
// and bytes in binary units.
func FormatAmount(res string, v int64) string {
	switch res {
	case "cpu":
		return resource.NewMilliQuantity(v, resource.DecimalSI).String()
	case "memory":
		return formatBytes(v)
	default:
		return fmt.Sprintf("%d", v)
	}
}

func formatBytes(v int64) string {
	neg := v < 0
	if neg {
		v = -v
	}
	units := []string{"", "Ki", "Mi", "Gi", "Ti"}
	f := float64(v)
	u := 0
	for f >= 1024 && u < len(units)-1 {
		f /= 1024
		u++
	}
	s := fmt.Sprintf("%.0f%s", f, units[u])
	if u > 0 && f < 10 {
		s = strings.TrimSuffix(fmt.Sprintf("%.1f", f), ".0") + units[u]
	}
	if neg {
		s = "-" + s
	}
	return s
}

func durString(d time.Duration) string {
	if d <= 0 {
		return "all"
	}
	return d.String()
}
