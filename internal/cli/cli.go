// Package cli wires flags, collection, the digest engine and rendering
// together. Run returns the process exit code so it can be tested directly.
package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/moveeeax/k8s-event-digest/internal/collect"
	"github.com/moveeeax/k8s-event-digest/internal/digest"
	"github.com/moveeeax/k8s-event-digest/internal/model"
	"github.com/moveeeax/k8s-event-digest/internal/render"
)

// Exit codes.
const (
	ExitOK       = 0 // digest produced, nothing to gate on
	ExitFindings = 1 // --fail-on-new and at least one group is new
	ExitError    = 2 // bad flags, unreadable input, API failure
)

// Version is stamped at build time with -ldflags.
var Version = "dev"

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Set(v string) error {
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			*s = append(*s, part)
		}
	}
	return nil
}

type options struct {
	fromJSON     string
	dumpJSON     string
	kubeconfig   string
	kubecontext  string
	namespace    string
	allNS        bool
	reason       string
	kind         string
	typeFilter   string
	ignoreReason stringList
	window       string
	newWithin    string
	chronicAfter string
	nowFlag      string
	minCount     int
	top          int
	format       string
	failOnNew    bool
	timeout      time.Duration
	showVersion  bool
}

// Run executes one invocation. argv excludes the program name.
func Run(argv []string, stdout, stderr io.Writer) int {
	var o options
	fs := flag.NewFlagSet("k8s-event-digest", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { usage(fs, stderr) }

	fs.StringVar(&o.fromJSON, "from-json", "", "read events from a `kubectl get events -o json` dump instead of the API (\"-\" for stdin)")
	fs.StringVar(&o.dumpJSON, "dump-json", "", "write the raw collected payload to this path so the run can be replayed")
	fs.StringVar(&o.kubeconfig, "kubeconfig", "", "explicit kubeconfig path (default: ambient KUBECONFIG / ~/.kube/config)")
	fs.StringVar(&o.kubecontext, "context", "", "kubeconfig context to use")
	fs.StringVar(&o.namespace, "namespace", "", "restrict to one namespace")
	fs.StringVar(&o.namespace, "n", "", "shorthand for --namespace")
	fs.BoolVar(&o.allNS, "A", false, "all namespaces (the default; accepted for kubectl muscle memory)")
	fs.StringVar(&o.reason, "reason", "", "only this event reason")
	fs.StringVar(&o.kind, "kind", "", "only this involved-object kind")
	fs.StringVar(&o.typeFilter, "type", "all", "warning, normal or all")
	fs.Var(&o.ignoreReason, "ignore-reason", "drop these reasons entirely (repeatable, comma-separated)")
	fs.StringVar(&o.window, "window", "1h", "only events last seen inside this window (0 for everything)")
	fs.StringVar(&o.newWithin, "new-within", "15m", "a group whose first occurrence is inside this window is new")
	fs.StringVar(&o.chronicAfter, "chronic-after", "6h", "a group spanning longer than this without a recent spike is chronic")
	fs.StringVar(&o.nowFlag, "now", "", "treat this RFC3339 timestamp as the current time (for reproducible replays)")
	fs.IntVar(&o.minCount, "min-count", 0, "drop groups with fewer occurrences than this")
	fs.IntVar(&o.top, "top", 0, "keep only the top N groups after ranking")
	fs.StringVar(&o.format, "format", "table", "table, json or markdown")
	fs.BoolVar(&o.failOnNew, "fail-on-new", false, "exit 1 when any group is classified new")
	fs.DurationVar(&o.timeout, "timeout", 30*time.Second, "API request timeout")
	fs.BoolVar(&o.showVersion, "version", false, "print version and exit")

	if err := fs.Parse(argv); err != nil {
		return ExitError
	}
	if o.showVersion {
		fmt.Fprintf(stdout, "k8s-event-digest %s\n", Version)
		return ExitOK
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

	opts := digest.Defaults(now)
	var err error
	if opts.Window, err = parseDuration("--window", o.window); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return ExitError
	}
	if opts.NewWithin, err = parseDuration("--new-within", o.newWithin); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return ExitError
	}
	if opts.ChronicAfter, err = parseDuration("--chronic-after", o.chronicAfter); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return ExitError
	}

	switch strings.ToLower(o.typeFilter) {
	case digest.FilterWarning, digest.FilterNormal, digest.FilterAll:
	default:
		fmt.Fprintf(stderr, "error: --type %q must be warning, normal or all\n", o.typeFilter)
		return ExitError
	}
	switch strings.ToLower(o.format) {
	case render.FormatTable, render.FormatJSON, render.FormatMarkdown:
	default:
		fmt.Fprintf(stderr, "error: --format %q must be table, json or markdown\n", o.format)
		return ExitError
	}

	opts.Namespace = o.namespace
	opts.Reason = o.reason
	opts.Kind = o.kind
	opts.TypeFilter = strings.ToLower(o.typeFilter)
	opts.IgnoreReasons = o.ignoreReason
	opts.MinCount = o.minCount
	opts.Top = o.top

	raw, err := gather(o)
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

	events, err := model.Decode(raw)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return ExitError
	}

	res := digest.Build(events, opts)
	if err := render.Write(stdout, res, o.format); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return ExitError
	}

	if o.failOnNew && res.NewGroups() > 0 {
		return ExitFindings
	}
	return ExitOK
}

func gather(o options) ([]byte, error) {
	if o.fromJSON != "" {
		return collect.FromFile(o.fromJSON)
	}
	ctx := context.Background()
	if o.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, o.timeout)
		defer cancel()
	}
	return collect.Live(ctx, o.kubeconfig, o.kubecontext, o.namespace, o.timeout)
}

// parseDuration accepts everything time.ParseDuration does, plus a plain day
// suffix: --chronic-after 3d reads better than 72h at 03:00.
func parseDuration(flagName, value string) (time.Duration, error) {
	v := strings.TrimSpace(value)
	if v == "" || v == "0" {
		return 0, nil
	}
	if strings.HasSuffix(v, "d") {
		days, err := strconv.ParseFloat(strings.TrimSuffix(v, "d"), 64)
		if err != nil {
			return 0, fmt.Errorf("%s %q is not a duration", flagName, value)
		}
		return time.Duration(days * float64(24*time.Hour)), nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s %q is not a duration", flagName, value)
	}
	if d < 0 {
		return 0, fmt.Errorf("%s %q must not be negative", flagName, value)
	}
	return d, nil
}

func usage(fs *flag.FlagSet, w io.Writer) {
	fmt.Fprintf(w, `k8s-event-digest %s — rank Kubernetes events instead of scrolling them.

Usage:
  k8s-event-digest [flags]

Exit codes:
  0  digest printed
  1  --fail-on-new and at least one group is new
  2  bad flags, unreadable input or an API failure

Flags:
`, Version)
	fs.PrintDefaults()
}
