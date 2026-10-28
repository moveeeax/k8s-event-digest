# k8s-event-digest

[![ci](https://github.com/moveeeax/k8s-event-digest/actions/workflows/ci.yml/badge.svg)](https://github.com/moveeeax/k8s-event-digest/actions/workflows/ci.yml)
[![go](https://img.shields.io/badge/go-1.24%2B-00ADD8)](https://go.dev/)
[![license](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

`kubectl get events -A` is the worst good data source in Kubernetes. Everything that matters is in
there, sorted by nothing useful, truncated at an hour, and drowned by the same `FailedMount`
repeating four hundred times. During an incident you do not need every event — you need to know
which three things are new.

`k8s-event-digest` collapses events into ranked groups, tells you which group started recently and
which one has been broken since Tuesday, and exits non-zero when something new shows up.

## What it answers

- Of everything the cluster is complaining about, **what started in the last fifteen minutes**?
- How many **distinct workloads** does each complaint touch, not how many event rows exist?
- Which complaints are **chronic background noise** that have been running for days?

## Install

```sh
go install github.com/moveeeax/k8s-event-digest/cmd/k8s-event-digest@latest
```

Or build from a checkout:

```sh
go build -o k8s-event-digest ./cmd/k8s-event-digest
```

## Usage

Against a live cluster, using the ambient kubeconfig:

```sh
k8s-event-digest --window 1h --top 15
```

Against a dump, which is how you replay an incident afterwards:

```sh
kubectl get events -A -o json > /tmp/events.json
k8s-event-digest --from-json /tmp/events.json --now 2026-10-28T12:00:00Z
```

```
7 groups from 84 events (746 occurrences) in the last 1h0m0s, 1 new within 15m0s

namespace: kube-system
  CLASS    TYPE  REASON   ROOT                OCC  OBJS  FIRST  LAST  MESSAGE
  chronic  warn  BackOff  Workload/fluentbit  255  6     16h    1m    Back-off restarting failed container fluentbit in pod fluentbit

namespace: payments
  CLASS  TYPE  REASON     ROOT               OCC  OBJS  FIRST  LAST  MESSAGE
  burst  warn  Unhealthy  Deployment/ledger  68   22    54m    1m    Liveness probe failed: Get "http://10.0.2.19:8080/healthz": context dea…

namespace: prod
  CLASS   TYPE    REASON            ROOT            OCC  OBJS  FIRST  LAST  MESSAGE
  new     warn    FailedScheduling  Deployment/api  100  40    11m    11s   0/9 nodes are available: 6 Insufficient cpu, 3 node(s) had untolerated …
  steady  normal  Scheduled         Deployment/api  12   12    30m    29m   Successfully assigned prod/api to ip-10-0-2-41.eu-central-1.compute.int…
```

Forty pods of one Deployment, one row. 100 occurrences, not 40 — the `count` field on each event is
summed rather than the rows being counted.

Pipe it somewhere:

```sh
k8s-event-digest --format json | jq '.groups[] | select(.class=="new")'
k8s-event-digest --format markdown --top 10   # paste straight into the incident channel
```

Gate a deploy on "no new complaints":

```sh
k8s-event-digest --namespace prod --type warning --new-within 15m --fail-on-new
```

Capture a run for later:

```sh
k8s-event-digest --dump-json /tmp/events.json --format markdown > digest.md
```

## How it works

1. **Collect.** Either `client-go` against the cluster or a JSON dump from disk. Live collection
   marshals the API's `EventList` straight back to JSON, so both paths feed the exact same decoder —
   which is why `--dump-json` output replays byte-for-byte through `--from-json`.
2. **Normalise.** Both API shapes are accepted: classic `core/v1` (`count`, `firstTimestamp`,
   `lastTimestamp`) and `events.k8s.io/v1` (`series.count`, `eventTime`, `note`, `regarding`).
3. **Resolve the controller root.** A Pod named `api-7d9f8c4b6d-x2k9p` becomes `Deployment/api`,
   `pg-0` becomes `StatefulSet/pg`, `backup-29384160-9wqtr` becomes `CronJob/backup`.
4. **Group** by (`reason`, involved-object `kind`, `namespace`, root) and sum occurrences, distinct
   objects, first seen and last seen.
5. **Classify and rank.** `occurrences × distinct objects`, with Warnings weighted 3×, and `new`
   groups floated to the top regardless of raw count.

### Classes

| Class | Meaning |
|---|---|
| `new` | first occurrence is inside `--new-within` — this is the triage signal |
| `burst` | older than `--new-within`, but most of its occurrences landed in the recent window |
| `chronic` | spans longer than `--chronic-after` at a roughly flat rate |
| `steady` | everything else |

The burst-versus-chronic split uses a rate estimate, not a timestamp check. An event row with
`count: 291` stretched across three days has a last-seen of two minutes ago, which looks recent and
is not; occurrences are spread uniformly across each row's span before being attributed to the
recent window.

## Flags

| Flag | Default | What it does |
|---|---|---|
| `--from-json PATH` | | read a `kubectl get events -o json` dump (`-` for stdin) |
| `--dump-json PATH` | | write the raw collected payload for later replay |
| `--window DUR` | `1h` | only events last seen inside this window (`0` for everything) |
| `--new-within DUR` | `15m` | first occurrence inside this window makes a group `new` |
| `--chronic-after DUR` | `6h` | span longer than this without a spike makes a group `chronic` |
| `--namespace`, `-n` | | restrict to one namespace |
| `--reason`, `--kind` | | filter by event reason or involved-object kind |
| `--type` | `all` | `warning`, `normal` or `all` |
| `--ignore-reason R,R` | | drop reasons entirely (repeatable) |
| `--min-count N` | `0` | drop groups below N occurrences |
| `--top N` | `0` | keep the top N groups after ranking |
| `--format` | `table` | `table`, `json` or `markdown` |
| `--fail-on-new` | `false` | exit 1 when any group is `new` |
| `--now RFC3339` | | treat this as the current time, for reproducible replays |
| `--kubeconfig`, `--context` | | override the ambient kubeconfig |

Durations accept everything Go's `time.ParseDuration` does, plus a day suffix: `--chronic-after 3d`.

### Exit codes

| Code | Meaning |
|---|---|
| `0` | digest printed |
| `1` | `--fail-on-new` and at least one group is `new` |
| `2` | bad flags, unreadable input, or an API failure |

## A caveat worth knowing

An Event carries `involvedObject` and nothing else — there is no owner reference anywhere in the
payload. The controller root is therefore recovered from the generated name, using the fact that a
ReplicaSet hash is 6-10 characters from Kubernetes' ambiguity-free alphabet
(`bcdfghjklmnpqrstvwxz2456789`). That separates a Deployment pod from a CronJob pod whose middle
segment is a plain timestamp.

A bare five-character suffix is shared by DaemonSet and Job pods and cannot be told apart from the
name alone, so those collapse under a kind-neutral `Workload/` root rather than a guessed one. When
nothing matches, the object is its own root — a group that should have collapsed stays split, which
is the safe direction to fail.

## Development

```sh
go test ./...                       # the whole suite, no cluster required
go vet ./...
go run ./cmd/k8s-event-digest --from-json examples/events-sample.json --now 2026-10-28T12:00:00Z
```

`examples/events-sample.json` is a synthetic 85-event cluster dump with one of each class in it, and
it is the same fixture the tests run against.

## License

MIT
