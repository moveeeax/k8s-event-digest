package model

import "regexp"

// An Event carries only involvedObject — there is no ownerReference anywhere in
// the payload, so the controller root has to be recovered from the generated
// name. These are the shapes the Kubernetes controllers actually produce:
//
//	Deployment   api        -> ReplicaSet api-7d9f8c4b6d -> Pod api-7d9f8c4b6d-x2k9p
//	StatefulSet  pg         -> Pod pg-0
//	CronJob      backup     -> Job backup-29384160       -> Pod backup-29384160-9wqtr
//	DaemonSet    fluentbit  -> Pod fluentbit-k4m2z
//
// A ReplicaSet hash is 6-10 characters drawn from Kubernetes' ambiguity-free
// alphabet (bcdfghjklmnpqrstvwxz2456789 — no 0, 1, 3, a, e, i, o, u), which is
// what separates a Deployment pod from a CronJob pod whose middle segment is a
// plain timestamp. A bare 5-character suffix is shared by DaemonSet and Job
// pods and cannot be told apart from the name alone, so those collapse under a
// kind-neutral `Workload/` root rather than a guessed one.
var (
	podFromCronJob     = regexp.MustCompile(`^(.+)-(\d{8,12})-([a-z0-9]{5})$`)
	podFromReplicaSet  = regexp.MustCompile(`^(.+)-([bcdfghjklmnpqrstvwxz2456789]{6,10})-([a-z0-9]{5})$`)
	podFromStatefulSet = regexp.MustCompile(`^(.+)-(\d{1,4})$`)
	podGenericSuffix   = regexp.MustCompile(`^(.+)-([a-z0-9]{5})$`)
	replicaSetName     = regexp.MustCompile(`^(.+)-([bcdfghjklmnpqrstvwxz2456789]{6,10})$`)
	jobFromCronJob     = regexp.MustCompile(`^(.+)-(\d{8,12})$`)
)

// RootOf walks a generated object name back to the workload a human would name
// in a postmortem. It is a heuristic on purpose: it keeps the tool usable
// against an offline JSON dump, where owner references do not exist.
//
// When nothing matches, the object is its own root, which is the safe failure —
// a group that should have collapsed stays split rather than merging two
// unrelated workloads.
func RootOf(kind, name string) string {
	if name == "" {
		return kind
	}
	switch kind {
	case "Pod":
		if m := podFromCronJob.FindStringSubmatch(name); m != nil {
			return "CronJob/" + m[1]
		}
		if m := podFromReplicaSet.FindStringSubmatch(name); m != nil {
			return "Deployment/" + m[1]
		}
		if m := podFromStatefulSet.FindStringSubmatch(name); m != nil {
			return "StatefulSet/" + m[1]
		}
		if m := podGenericSuffix.FindStringSubmatch(name); m != nil {
			return "Workload/" + m[1]
		}
	case "ReplicaSet":
		if m := replicaSetName.FindStringSubmatch(name); m != nil {
			return "Deployment/" + m[1]
		}
	case "Job":
		if m := jobFromCronJob.FindStringSubmatch(name); m != nil {
			return "CronJob/" + m[1]
		}
	}
	if kind == "" {
		return name
	}
	return kind + "/" + name
}
