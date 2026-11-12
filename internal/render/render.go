// Package render prints a digest as a table, as JSON, or as Markdown.
package render

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/moveeeax/k8s-event-digest/internal/digest"
)

// Supported output formats.
const (
	FormatTable    = "table"
	FormatJSON     = "json"
	FormatMarkdown = "markdown"
)

const messageWidth = 72

// Write renders res in the requested format.
func Write(w io.Writer, res digest.Result, format string) error {
	switch strings.ToLower(format) {
	case FormatJSON:
		return writeJSON(w, res)
	case FormatMarkdown:
		return writeMarkdown(w, res)
	case FormatTable, "":
		return writeTable(w, res)
	default:
		return fmt.Errorf("unknown format %q (want table, json or markdown)", format)
	}
}

func writeJSON(w io.Writer, res digest.Result) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if res.Groups == nil {
		res.Groups = []digest.Group{}
	}
	return enc.Encode(res)
}

func writeTable(w io.Writer, res digest.Result) error {
	if len(res.Groups) == 0 {
		if _, err := fmt.Fprintf(w, "no events matched (%d considered, window %s)\n",
			res.EventsConsidered, res.Window); err != nil {
			return err
		}
		if res.Baseline != nil {
			fmt.Fprintf(w, "%s\n", baselineLine(res))
		}
		return writeGoneTable(w, res)
	}

	fmt.Fprintf(w, "%d groups from %d events (%d occurrences) in the last %s, %d new within %s\n",
		res.GroupsTotal, res.EventsFiltered, res.TotalOccurrences, res.Window,
		res.NewGroups(), res.NewWithin)
	if res.Baseline != nil {
		fmt.Fprintf(w, "%s\n", baselineLine(res))
	}
	fmt.Fprintln(w)

	diff := res.Baseline != nil
	for _, ns := range namespaces(res.Groups) {
		fmt.Fprintf(w, "namespace: %s\n", ns)
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		if diff {
			fmt.Fprint(tw, "  CHANGE\tCLASS")
		} else {
			fmt.Fprint(tw, "  CLASS")
		}
		fmt.Fprintln(tw, "\tTYPE\tREASON\tROOT\tOCC\tOBJS\tFIRST\tLAST\tMESSAGE")
		for _, g := range res.Groups {
			if g.Namespace != ns {
				continue
			}
			if diff {
				fmt.Fprintf(tw, "  %s\t%s", g.Change, g.Class)
			} else {
				fmt.Fprintf(tw, "  %s", g.Class)
			}
			fmt.Fprintf(tw, "\t%s\t%s\t%s\t%d\t%d\t%s\t%s\t%s\n",
				shortType(g.Type), g.Reason, g.Root, g.Occurrences, g.Objects,
				ago(res.GeneratedAt, g.First), ago(res.GeneratedAt, g.Last),
				truncate(g.Message, messageWidth))
		}
		if err := tw.Flush(); err != nil {
			return err
		}
		fmt.Fprintln(w)
	}
	return writeGoneTable(w, res)
}

func baselineLine(res digest.Result) string {
	b := res.Baseline
	return fmt.Sprintf("vs baseline %s: %d added, %d worse (rate over %gx), %d unchanged, %d gone",
		b.GeneratedAt.Format(time.RFC3339), b.Added, b.Worse, b.WorseFactor, b.Unchanged, b.Gone)
}

// writeGoneTable lists what the baseline had and this run does not. Ages are
// relative to this run.
func writeGoneTable(w io.Writer, res digest.Result) error {
	if len(res.Gone) == 0 {
		return nil
	}
	fmt.Fprintln(w, "gone since baseline:")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  NAMESPACE\tTYPE\tREASON\tROOT\tOCC\tLAST SEEN")
	for _, g := range res.Gone {
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%d\t%s\n",
			g.Namespace, shortType(g.Type), g.Reason, g.Root, g.Occurrences,
			ago(res.GeneratedAt, g.Last))
	}
	return tw.Flush()
}

func writeMarkdown(w io.Writer, res digest.Result) error {
	fmt.Fprintf(w, "### Event digest — %s\n\n", res.GeneratedAt.Format(time.RFC3339))
	fmt.Fprintf(w, "%d groups from %d events (%d occurrences) in the last %s. New within %s: **%d**.\n\n",
		res.GroupsTotal, res.EventsFiltered, res.TotalOccurrences, res.Window,
		res.NewWithin, res.NewGroups())

	if res.Baseline != nil {
		fmt.Fprintf(w, "Compared %s.\n\n", strings.TrimPrefix(baselineLine(res), "vs "))
	}

	if len(res.Groups) == 0 {
		if _, err := fmt.Fprintln(w, "_No events matched._"); err != nil {
			return err
		}
		return writeGoneMarkdown(w, res)
	}

	diff := res.Baseline != nil
	if diff {
		fmt.Fprintln(w, "| Change | Class | Type | Namespace | Reason | Root | Occ | Objs | First | Last | Message |")
		fmt.Fprintln(w, "|---|---|---|---|---|---|---:|---:|---|---|---|")
	} else {
		fmt.Fprintln(w, "| Class | Type | Namespace | Reason | Root | Occ | Objs | First | Last | Message |")
		fmt.Fprintln(w, "|---|---|---|---|---|---:|---:|---|---|---|")
	}
	for _, g := range res.Groups {
		if diff {
			fmt.Fprintf(w, "| %s ", g.Change)
		}
		fmt.Fprintf(w, "| %s | %s | `%s` | `%s` | `%s` | %d | %d | %s | %s | %s |\n",
			g.Class, shortType(g.Type), g.Namespace, g.Reason, g.Root,
			g.Occurrences, g.Objects,
			ago(res.GeneratedAt, g.First), ago(res.GeneratedAt, g.Last),
			escapePipes(truncate(g.Message, messageWidth)))
	}
	return writeGoneMarkdown(w, res)
}

func writeGoneMarkdown(w io.Writer, res digest.Result) error {
	if len(res.Gone) == 0 {
		return nil
	}
	fmt.Fprintln(w, "\nGone since baseline:")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "| Type | Namespace | Reason | Root | Occ | Last seen |")
	fmt.Fprintln(w, "|---|---|---|---|---:|---|")
	for _, g := range res.Gone {
		if _, err := fmt.Fprintf(w, "| %s | `%s` | `%s` | `%s` | %d | %s |\n",
			shortType(g.Type), g.Namespace, g.Reason, g.Root, g.Occurrences,
			ago(res.GeneratedAt, g.Last)); err != nil {
			return err
		}
	}
	return nil
}

func namespaces(groups []digest.Group) []string {
	seen := map[string]struct{}{}
	out := []string{}
	for _, g := range groups {
		if _, ok := seen[g.Namespace]; ok {
			continue
		}
		seen[g.Namespace] = struct{}{}
		out = append(out, g.Namespace)
	}
	sort.Strings(out)
	return out
}

func shortType(t string) string {
	if t == "Warning" {
		return "warn"
	}
	return "normal"
}

// ago prints a kubectl-style relative age, which is the only time format that
// is readable at 03:00.
func ago(now, then time.Time) string {
	if then.IsZero() {
		return "-"
	}
	d := now.Sub(then)
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours())/24)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}

func escapePipes(s string) string {
	return strings.ReplaceAll(s, "|", `\|`)
}
