# k8s-event-digest

[![ci](https://github.com/moveeeax/k8s-event-digest/actions/workflows/ci.yml/badge.svg)](https://github.com/moveeeax/k8s-event-digest/actions/workflows/ci.yml)
[![go](https://img.shields.io/badge/go-1.26%2B-00ADD8)](https://go.dev/)
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
- What appeared **since the baseline** taken before a deploy?
- **Why will a pod not schedule**: which node came closest, which ones are cordoned, tainted,
  under pressure or full on requests (`k8s-event-digest nodes`)?

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

### What is new since the deploy

`new` is about time: a group whose first occurrence is inside `--new-within`. After a deploy you
usually want something else, the groups that did not exist before it. Take a baseline first, then
compare:

```sh
k8s-event-digest --namespace prod --format json > before.json   # before the rollout
kubectl rollout status deploy/api -n prod
k8s-event-digest --namespace prod --baseline before.json --fail-on-added
```

```
7 groups from 84 events (746 occurrences) in the last 1h0m0s, 1 new within 15m0s
vs baseline 2026-10-28T12:00:00Z: 1 added, 0 worse (rate over 2x), 6 unchanged, 0 gone

namespace: prod
  CHANGE     CLASS   TYPE    REASON            ROOT            OCC  OBJS  FIRST  LAST  MESSAGE
  added      new     warn    FailedScheduling  Deployment/api  100  40    11m    11s   0/9 nodes are available: 6 Insufficient cpu, 3 node(s) had untolerated …
  unchanged  steady  normal  Scheduled         Deployment/api  12   12    30m    29m   Successfully assigned prod/api to ip-10-0-2-41.eu-central-1.compute.int…
```

Every group gets a `change`:

| Change | Meaning |
|---|---|
| `added` | the (`reason`, `kind`, `namespace`, root) key is not in the baseline |
| `worse` | it is, and its occurrence rate grew by more than `--worse-factor` (default `2`) |
| `unchanged` | it is, at a comparable rate |
| `gone` | it was in the baseline and is absent now; listed under "gone since baseline" and in `.gone` |

The rate is occurrences per hour since the group was first seen, as of each digest's own
`generatedAt`. A chronic complaint keeps a flat rate however long it runs, so it stays `unchanged`;
one that a deploy made louder piles up occurrences faster than it ages.

The baseline is either a previous `--format json` digest or a raw `--dump-json` /
`kubectl get events -o json` payload. A raw payload is digested with the current filters as of its
newest event, since a dump does not record when it was taken. Capture the baseline with the same
filters you compare with, and without `--top`: a group the baseline trimmed would come back as
`added` (the tool warns when it sees a truncated baseline). The comparison itself runs before
`--top`, so trimming the current output never hides an added group from `--fail-on-added`.

## Node pressure: why the scheduler said no

`k8s-event-digest nodes` answers the question that usually follows a `FailedScheduling` group,
without reading every node object:

```sh
k8s-event-digest nodes --sort-by headroom --since 1h
kubectl get nodes,pods -A -o json > /tmp/nodes.json     # or replay a capture
k8s-event-digest nodes --from-json /tmp/nodes.json --now 2026-10-28T12:00:00Z
```

```
5 nodes: 1 not ready, 1 under pressure, 1 cordoned; 4 pending pods, 2 evictions in the last 1h0m0s

NODE                                        READY       MEM      DISK  PID  CPU FREE      MEM FREE     PODS  BLOCKED            TAINTS
ip-10-0-1-14.eu-central-1.compute.internal  unknown 6m  -        -     -    3420m (87%)   15Gi (93%)   1/58  not ready          node.kubernetes.io/unreachable:NoExecute,node.k…
ip-10-0-1-12.eu-central-1.compute.internal  yes 9d      yes 12m  -     -    1920m (48%)   2Gi (12%)    1/58  -                  node.kubernetes.io/memory-pressure:NoSchedule
ip-10-0-1-13.eu-central-1.compute.internal  yes 9d      -        -     -    3670m (93%)   15Gi (93%)   1/58  cordoned           node.kubernetes.io/unschedulable:NoSchedule
ip-10-0-2-21.eu-central-1.compute.internal  yes 9d      -        -     -    7910m (100%)  30Gi (100%)  0/58  untolerated taint  nvidia.com/gpu=present:NoSchedule
ip-10-0-1-11.eu-central-1.compute.internal  yes 9d      -        -     -    220m (5%)     7.9Gi (49%)  4/58  -                  -

evicted by the kubelet:
  AGE  NODE                                        POD                              LOW ON             THRESHOLD  AVAILABLE
  10m  ip-10-0-1-12.eu-central-1.compute.internal  workers/worker-7f9c6d5b8b-m4kd2  memory             100Mi      87232Ki
  28m  ip-10-0-1-12.eu-central-1.compute.internal  workers/worker-7f9c6d5b8b-h8s2t  ephemeral-storage  2Gi        1536Mi

pending pods:
  POD                        CPU REQ  MEM REQ  WHY
  batch/huge-0               500m     64Gi     no eligible node: 1 cordoned, 3 node selector, 1 not ready
  batch/queued-0             250m     256Mi    held by scheduling gates: kueue.x-k8s.io/admission
  ml/trainer-0               2        4Gi      fits ip-10-0-2-21.eu-central-1.compute.internal on cpu and memory requests; blocked by something not modelled here (affinity, topology spread, ports, volumes)
  prod/api-7d9f8c4b6d-p4n2c  1        2Gi      closest is ip-10-0-1-11.eu-central-1.compute.internal: cpu short by 780m
```

- **Conditions.** `Ready`, `MemoryPressure`, `DiskPressure` and `PIDPressure`, each with how long it
  has held (from `lastTransitionTime`). A missing or `Unknown` condition prints as `unknown` / `?`.
- **Headroom.** Allocatable minus the requests of every non-terminated pod bound to the node,
  counted the way the scheduler counts them: containers plus sidecar (`restartPolicy: Always`) init
  containers, or the largest plain init container if that is bigger, plus the RuntimeClass
  overhead; pod-level `spec.resources.requests` replace the container sum when set. This is
  requests, not live usage: a node can be full on paper and idle in `kubectl top`.
- **Blocked.** Capacity the scheduler cannot use: not ready, cordoned, or a `NoSchedule` /
  `NoExecute` taint. The taints Kubernetes adds to mirror a condition or a cordon
  (`node.kubernetes.io/not-ready`, `unreachable`, `unschedulable`, `memory-pressure`, ...) are still
  listed under TAINTS but do not count twice.
- **Evictions.** Pods in phase `Failed` with reason `Evicted`. The time is the `DisruptionTarget`
  condition the kubelet sets, or the last container finish when that condition is absent; the
  resource, threshold and what was available are parsed from the kubelet's message. `--since` limits
  them (`0` for everything).
- **Pending pods.** Unbound pods in phase `Pending`. Each node is checked for Ready, cordon,
  `nodeSelector` and untolerated `NoSchedule`/`NoExecute` taints; the rest are compared on CPU,
  memory and pod count, and the one with the smallest relative shortfall is named. Node affinity,
  topology spread, host ports and volumes are not modelled: a pod that fits on resources says so
  instead of guessing. Pods held by `schedulingGates` are reported as such.

`--fail-on-pressure` exits 1 when any node is not Ready or reports a pressure condition, so the same
check can run from an alerting pipeline next to `--format json`.

| Flag (`nodes`) | Default | What it does |
|---|---|---|
| `--from-json PATH` | | read a `kubectl get nodes,pods -A -o json` dump (`-` for stdin) |
| `--dump-json PATH` | | write the raw collected nodes and pods for later replay |
| `--since DUR` | `1h` | only evictions inside this window (`0` for everything) |
| `--sort-by` | `pressure` | `pressure` (not ready, then pressure, then blocked, then tightest), `headroom` or `name` |
| `--namespace`, `-n` | | only evictions and Pending pods from this namespace; headroom always counts every pod |
| `--format` | `table` | `table`, `json` or `markdown` |
| `--fail-on-pressure` | `false` | exit 1 when any node is not Ready or under pressure |
| `--now RFC3339` | | treat this as the current time, for reproducible replays |
| `--kubeconfig`, `--context`, `--timeout` | | as for the digest |

Live mode lists nodes and all pods (`list` on `nodes` and `pods` cluster-wide).

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
| `--baseline PATH` | | compare against a previous `--format json` digest or `--dump-json` payload |
| `--worse-factor F` | `2` | with `--baseline`, a group whose occurrence rate grew by more than F× is `worse` (at least 1) |
| `--fail-on-added` | `false` | with `--baseline`, exit 1 when any group is `added` |
| `--now RFC3339` | | treat this as the current time, for reproducible replays |
| `--kubeconfig`, `--context` | | override the ambient kubeconfig |

Durations accept everything Go's `time.ParseDuration` does, plus a day suffix: `--chronic-after 3d`.

### Exit codes

| Code | Meaning |
|---|---|
| `0` | digest printed |
| `1` | `--fail-on-new` and a group is `new`, `--fail-on-added` and a group is `added`, or `nodes --fail-on-pressure` and a node is not Ready or under pressure |
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
go run ./cmd/k8s-event-digest nodes --from-json examples/nodes-sample.json --now 2026-10-28T12:00:00Z
```

`examples/events-sample.json` is a synthetic 85-event cluster dump with one of each class in it, and
it is the same fixture the tests run against. `examples/nodes-sample.json` is a synthetic five-node
`kubectl get nodes,pods -A -o json` dump with one node in each state, three evictions and four
Pending pods, also shared with the tests.

## License

MIT
