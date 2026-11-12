package render

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/moveeeax/k8s-event-digest/internal/nodes"
)

// WriteNodes renders a nodes report in the requested format.
func WriteNodes(w io.Writer, rep nodes.Report, format string) error {
	switch strings.ToLower(format) {
	case FormatJSON:
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	case FormatMarkdown:
		return writeNodesMarkdown(w, rep)
	case FormatTable, "":
		return writeNodesTable(w, rep)
	default:
		return fmt.Errorf("unknown format %q (want table, json or markdown)", format)
	}
}

func nodesSummary(rep nodes.Report) string {
	window := "in the last " + rep.Since
	if rep.Since == "all" {
		window = "on record"
	}
	return fmt.Sprintf("%d nodes: %d not ready, %d under pressure, %d cordoned; %d pending pods, %d evictions %s",
		rep.NodesTotal, rep.NotReady, rep.UnderPressure, rep.Cordoned,
		len(rep.Pending), len(rep.Evictions), window)
}

func writeNodesTable(w io.Writer, rep nodes.Report) error {
	fmt.Fprintf(w, "%s\n\n", nodesSummary(rep))

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NODE\tREADY\tMEM\tDISK\tPID\tCPU FREE\tMEM FREE\tPODS\tBLOCKED\tTAINTS")
	for _, n := range rep.Nodes {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			n.Name, readyCell(rep.GeneratedAt, n.Ready),
			pressureCell(rep.GeneratedAt, n.Pressure, 0),
			pressureCell(rep.GeneratedAt, n.Pressure, 1),
			pressureCell(rep.GeneratedAt, n.Pressure, 2),
			freeCell("cpu", n.Headroom.CPUFree, n.Headroom.CPUAllocatable),
			freeCell("memory", n.Headroom.MemFree, n.Headroom.MemAllocatable),
			fmt.Sprintf("%d/%d", n.Headroom.Pods, n.Headroom.PodsAllocatable),
			dash(strings.Join(n.Blocked, ",")),
			truncate(dash(strings.Join(n.Taints, ",")), 48))
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	if len(rep.Evictions) > 0 {
		fmt.Fprintln(w, "\nevicted by the kubelet:")
		tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  AGE\tNODE\tPOD\tLOW ON\tTHRESHOLD\tAVAILABLE")
		for _, e := range rep.Evictions {
			fmt.Fprintf(tw, "  %s\t%s\t%s/%s\t%s\t%s\t%s\n",
				ago(rep.GeneratedAt, e.At), dash(e.Node), e.Namespace, e.Pod,
				dash(e.Resource), dash(e.Threshold), dash(e.Available))
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}

	if len(rep.Pending) > 0 {
		fmt.Fprintln(w, "\npending pods:")
		tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  POD\tCPU REQ\tMEM REQ\tWHY")
		for _, p := range rep.Pending {
			fmt.Fprintf(tw, "  %s/%s\t%s\t%s\t%s\n", p.Namespace, p.Pod,
				nodes.FormatAmount("cpu", p.CPURequest), nodes.FormatAmount("memory", p.MemRequest),
				p.Reason)
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}
	return nil
}

func writeNodesMarkdown(w io.Writer, rep nodes.Report) error {
	fmt.Fprintf(w, "### Node pressure — %s\n\n", rep.GeneratedAt.Format(time.RFC3339))
	fmt.Fprintf(w, "%s.\n\n", nodesSummary(rep))

	fmt.Fprintln(w, "| Node | Ready | Mem | Disk | PID | CPU free | Mem free | Pods | Blocked |")
	fmt.Fprintln(w, "|---|---|---|---|---|---:|---:|---:|---|")
	for _, n := range rep.Nodes {
		fmt.Fprintf(w, "| `%s` | %s | %s | %s | %s | %s | %s | %d/%d | %s |\n",
			n.Name, readyCell(rep.GeneratedAt, n.Ready),
			pressureCell(rep.GeneratedAt, n.Pressure, 0),
			pressureCell(rep.GeneratedAt, n.Pressure, 1),
			pressureCell(rep.GeneratedAt, n.Pressure, 2),
			freeCell("cpu", n.Headroom.CPUFree, n.Headroom.CPUAllocatable),
			freeCell("memory", n.Headroom.MemFree, n.Headroom.MemAllocatable),
			n.Headroom.Pods, n.Headroom.PodsAllocatable,
			dash(strings.Join(n.Blocked, ", ")))
	}

	if len(rep.Evictions) > 0 {
		fmt.Fprintln(w, "\nEvicted by the kubelet:")
		fmt.Fprintln(w)
		fmt.Fprintln(w, "| Age | Node | Pod | Low on | Threshold | Available |")
		fmt.Fprintln(w, "|---|---|---|---|---|---|")
		for _, e := range rep.Evictions {
			fmt.Fprintf(w, "| %s | `%s` | `%s/%s` | %s | %s | %s |\n",
				ago(rep.GeneratedAt, e.At), dash(e.Node), e.Namespace, e.Pod,
				dash(e.Resource), dash(e.Threshold), dash(e.Available))
		}
	}

	if len(rep.Pending) > 0 {
		fmt.Fprintln(w, "\nPending pods:")
		fmt.Fprintln(w)
		fmt.Fprintln(w, "| Pod | CPU req | Mem req | Why |")
		fmt.Fprintln(w, "|---|---:|---:|---|")
		for _, p := range rep.Pending {
			fmt.Fprintf(w, "| `%s/%s` | %s | %s | %s |\n", p.Namespace, p.Pod,
				nodes.FormatAmount("cpu", p.CPURequest), nodes.FormatAmount("memory", p.MemRequest),
				escapePipes(p.Reason))
		}
	}
	return nil
}

func readyCell(now time.Time, c nodes.Condition) string {
	word := map[string]string{nodes.StatusTrue: "yes", nodes.StatusFalse: "no"}[c.Status]
	if word == "" {
		word = "unknown"
	}
	if c.Since.IsZero() {
		return word
	}
	return word + " " + ago(now, c.Since)
}

// pressureCell prints "-" for a healthy condition and "yes <age>" for one the
// kubelet is currently reporting.
func pressureCell(now time.Time, conds []nodes.Condition, i int) string {
	if i >= len(conds) {
		return "?"
	}
	switch c := conds[i]; c.Status {
	case nodes.StatusFalse:
		return "-"
	case nodes.StatusTrue:
		if c.Since.IsZero() {
			return "yes"
		}
		return "yes " + ago(now, c.Since)
	default:
		return "?"
	}
}

func freeCell(res string, free, alloc int64) string {
	if alloc <= 0 {
		return nodes.FormatAmount(res, free)
	}
	return fmt.Sprintf("%s (%d%%)", nodes.FormatAmount(res, free), free*100/alloc)
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
