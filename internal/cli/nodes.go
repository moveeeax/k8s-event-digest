package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/moveeeax/k8s-event-digest/internal/collect"
	"github.com/moveeeax/k8s-event-digest/internal/nodes"
	"github.com/moveeeax/k8s-event-digest/internal/render"
)

type nodesOptions struct {
	fromJSON       string
	dumpJSON       string
	kubeconfig     string
	kubecontext    string
	namespace      string
	since          string
	sortBy         string
	nowFlag        string
	format         string
	failOnPressure bool
	timeout        time.Duration
}

// runNodes is the `nodes` subcommand: why the scheduler said no.
func runNodes(argv []string, stdout, stderr io.Writer) int {
	var o nodesOptions
	fs := flag.NewFlagSet("k8s-event-digest nodes", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { nodesUsage(fs, stderr) }

	fs.StringVar(&o.fromJSON, "from-json", "", "read a `kubectl get nodes,pods -A -o json` dump instead of the API (\"-\" for stdin)")
	fs.StringVar(&o.dumpJSON, "dump-json", "", "write the raw collected nodes and pods to this path so the run can be replayed")
	fs.StringVar(&o.kubeconfig, "kubeconfig", "", "explicit kubeconfig path (default: ambient KUBECONFIG / ~/.kube/config)")
	fs.StringVar(&o.kubecontext, "context", "", "kubeconfig context to use")
	fs.StringVar(&o.namespace, "namespace", "", "only report evictions and Pending pods from this namespace (headroom always counts every pod)")
	fs.StringVar(&o.namespace, "n", "", "shorthand for --namespace")
	fs.StringVar(&o.since, "since", "1h", "only evictions inside this window (0 for everything)")
	fs.StringVar(&o.sortBy, "sort-by", nodes.SortPressure, "pressure, headroom or name")
	fs.StringVar(&o.nowFlag, "now", "", "treat this RFC3339 timestamp as the current time (for reproducible replays)")
	fs.StringVar(&o.format, "format", "table", "table, json or markdown")
	fs.BoolVar(&o.failOnPressure, "fail-on-pressure", false, "exit 1 when any node is not Ready or reports memory, disk or PID pressure")
	fs.DurationVar(&o.timeout, "timeout", 30*time.Second, "API request timeout")

	if err := fs.Parse(argv); err != nil {
		return ExitError
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "error: unexpected argument %q\n", fs.Arg(0))
		return ExitError
	}

	now := time.Now().UTC()
	if o.nowFlag != "" {
		parsed, err := time.Parse(time.RFC3339, o.nowFlag)
		if err != nil {
			fmt.Fprintf(stderr, "error: --now %q is not an RFC3339 timestamp\n", o.nowFlag)
			return ExitError
		}
		now = parsed.UTC()
	}
	since, err := parseDuration("--since", o.since)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return ExitError
	}
	sortBy := strings.ToLower(o.sortBy)
	switch sortBy {
	case nodes.SortPressure, nodes.SortHeadroom, nodes.SortName:
	default:
		fmt.Fprintf(stderr, "error: --sort-by %q must be pressure, headroom or name\n", o.sortBy)
		return ExitError
	}
	switch strings.ToLower(o.format) {
	case render.FormatTable, render.FormatJSON, render.FormatMarkdown:
	default:
		fmt.Fprintf(stderr, "error: --format %q must be table, json or markdown\n", o.format)
		return ExitError
	}

	raw, err := gatherNodes(o)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return ExitError
	}
	if o.dumpJSON != "" {
		if err := os.WriteFile(o.dumpJSON, raw, 0o644); err != nil {
			fmt.Fprintf(stderr, "error: write dump: %v\n", err)
			return ExitError
		}
	}

	snap, err := nodes.Decode(raw)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return ExitError
	}

	rep := nodes.Build(snap, nodes.Options{Now: now, Since: since, Namespace: o.namespace, SortBy: sortBy})
	if err := render.WriteNodes(stdout, rep, o.format); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return ExitError
	}

	if o.failOnPressure && rep.Unhealthy() > 0 {
		return ExitFindings
	}
	return ExitOK
}

func gatherNodes(o nodesOptions) ([]byte, error) {
	if o.fromJSON != "" {
		return collect.FromFile(o.fromJSON)
	}
	ctx := context.Background()
	if o.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, o.timeout)
		defer cancel()
	}
	return collect.LiveNodes(ctx, o.kubeconfig, o.kubecontext, o.timeout)
}

func nodesUsage(fs *flag.FlagSet, w io.Writer) {
	fmt.Fprintf(w, `k8s-event-digest nodes %s — why the scheduler said no, on one screen.

Node conditions, CPU and memory headroom against scheduled pod requests,
recent kubelet evictions, and the node each Pending pod came closest to.

Usage:
  k8s-event-digest nodes [flags]

Exit codes:
  0  report printed
  1  --fail-on-pressure and at least one node is not Ready or under pressure
  2  bad flags, unreadable input or an API failure

Flags:
`, Version)
	fs.PrintDefaults()
}
